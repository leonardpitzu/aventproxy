package rtsp

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/utils"

	"github.com/pion/rtp"
)

// RTP transport mode (UDP or TCP)
type TransportMode int

const (
	TransportUDP TransportMode = iota
	TransportTCP               // Interleaved
)

// interleaveHeaderLen is the `$`, channel and 16-bit length that prefix an RTP
// packet on a TCP-interleaved connection. Packets are marshalled at this offset
// so the header and the payload go out in a single write.
const interleaveHeaderLen = 4

// direction selects which of a client's transports a packet belongs to.
type direction int

const (
	videoChannel direction = iota
	audioChannel
)

func (d direction) String() string {
	if d == videoChannel {
		return "video"
	}
	return "audio"
}

func (d direction) conn(c *RTPClient) *net.UDPConn {
	if d == videoChannel {
		return c.videoConn
	}
	return c.audioConn
}

func (d direction) channel(c *RTPClient) byte {
	if d == videoChannel {
		return c.videoRTPChannel
	}
	return c.audioRTPChannel
}

// streamState holds everything one direction of the stream mutates per packet.
// Video and audio arrive on separate goroutines, so each gets its own lock and
// its own scratch buffer rather than contending on one forwarder-wide mutex.
type streamState struct {
	mu sync.Mutex

	tsBase    uint32
	tsStarted bool
	seq       uint16
	logged    bool

	// H264 parameter sets, replayed ahead of a keyframe for late joiners.
	sps *rtp.Packet
	pps *rtp.Packet

	buf      []byte
	snapshot []*RTPClient
}

type RTPForwarder struct {
	mutex   sync.RWMutex
	clients map[string]*RTPClient

	video streamState
	audio streamState

	OnBackchannelAudio func(*rtp.Packet)
}

type RTPClient struct {
	sessionID     string
	transportMode TransportMode

	// UDP transport - Outgoing connections (server -> client)
	videoConn *net.UDPConn // For sending video to client
	audioConn *net.UDPConn // For sending audio to client

	// UDP transport - Client ports
	videoRTPPort int // Client's video receiving port
	audioRTPPort int // Client's audio receiving port

	// UDP backchannel listeners (server side)
	backchannelListener     *net.UDPConn // Server's RTP listener for backchannel
	backchannelRTCPListener *net.UDPConn // Server's RTCP listener for backchannel
	backchannelServerPort   int          // Server's RTP listening port

	// TCP interleaved transport
	tcpConn             net.Conn
	videoRTPChannel     byte
	audioRTPChannel     byte
	backAudioRTPChannel byte
}

func NewRTPForwarder() *RTPForwarder {
	return &RTPForwarder{
		clients: make(map[string]*RTPClient),
	}
}

func dialLocalUDP(port int) (*net.UDPConn, error) {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("localhost:%d", port))
	if err != nil {
		return nil, err
	}
	return net.DialUDP("udp", nil, addr)
}

func (rf *RTPForwarder) AddUDPClient(sessionID string, videoRTPPort, audioRTPPort int) error {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	// Existing client: only fill in transports it does not have yet.
	if client, exists := rf.clients[sessionID]; exists {
		client.videoRTPPort = videoRTPPort
		client.audioRTPPort = audioRTPPort

		if videoRTPPort > 0 && client.videoConn == nil {
			client.videoConn, _ = dialLocalUDP(videoRTPPort)
		}
		if audioRTPPort > 0 && client.audioConn == nil {
			client.audioConn, _ = dialLocalUDP(audioRTPPort)
		}
		return nil
	}

	client := &RTPClient{
		sessionID:     sessionID,
		transportMode: TransportUDP,
		videoRTPPort:  videoRTPPort,
		audioRTPPort:  audioRTPPort,
	}

	if videoRTPPort > 0 {
		conn, err := dialLocalUDP(videoRTPPort)
		if err != nil {
			return fmt.Errorf("failed to create video UDP connection: %w", err)
		}
		client.videoConn = conn
	}

	if audioRTPPort > 0 {
		conn, err := dialLocalUDP(audioRTPPort)
		if err != nil {
			client.close()
			return fmt.Errorf("failed to create audio UDP connection: %w", err)
		}
		client.audioConn = conn
	}

	rf.clients[sessionID] = client

	core.Logger.Trace().Msgf("Added UDP RTP client %s (video port:%d, audio port:%d)",
		sessionID, videoRTPPort, audioRTPPort)
	return nil
}

func (rf *RTPForwarder) SetupUDPBackchannel(sessionID string, clientPort int) (int, error) {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	client, exists := rf.clients[sessionID]
	if !exists {
		return 0, fmt.Errorf("client %s not found", sessionID)
	}

	if client.transportMode != TransportUDP {
		return 0, fmt.Errorf("client %s is not using UDP transport", sessionID)
	}

	// If listeners already exist, return existing port
	if client.backchannelListener != nil {
		return client.backchannelServerPort, nil
	}

	// Allocate consecutive ports for RTP/RTCP
	portPair, err := utils.DefaultPortAllocator.GetConsecutiveUDPPorts(nil, 10)
	if err != nil {
		return 0, fmt.Errorf("failed to allocate UDP ports for backchannel: %w", err)
	}

	client.backchannelListener = portPair.RTPListener
	client.backchannelRTCPListener = portPair.RTCPListener
	client.backchannelServerPort = portPair.RTPPort

	go rf.handleUDPBackchannelRTP(sessionID, client.backchannelListener)
	go drainUDP(client.backchannelRTCPListener)

	core.Logger.Trace().Msgf("Setup UDP backchannel for client %s (client ports:%d-%d, server ports:%d-%d)",
		sessionID, clientPort, clientPort+1, portPair.RTPPort, portPair.RTCPPort)

	return portPair.RTPPort, nil
}

func (rf *RTPForwarder) AddTCPClient(sessionID string, conn net.Conn, videoRTPChannel, audioRTPChannel, backAudioRTPChannel byte) error {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	if existing, exists := rf.clients[sessionID]; exists {
		core.Logger.Trace().Msgf("TCP client %s already exists, updating channels (video:%d->%d, audio:%d->%d, back:%d->%d)",
			sessionID, existing.videoRTPChannel, videoRTPChannel, existing.audioRTPChannel, audioRTPChannel, existing.backAudioRTPChannel, backAudioRTPChannel)
		existing.videoRTPChannel = videoRTPChannel
		existing.audioRTPChannel = audioRTPChannel
		existing.backAudioRTPChannel = backAudioRTPChannel
		return nil
	}

	rf.clients[sessionID] = &RTPClient{
		sessionID:           sessionID,
		transportMode:       TransportTCP,
		tcpConn:             conn,
		videoRTPChannel:     videoRTPChannel,
		audioRTPChannel:     audioRTPChannel,
		backAudioRTPChannel: backAudioRTPChannel,
	}

	core.Logger.Trace().Msgf("Added TCP RTP client %s (video channel:%d, audio channel:%d, back audio channel:%d)",
		sessionID, videoRTPChannel, audioRTPChannel, backAudioRTPChannel)
	return nil
}

func (c *RTPClient) close() {
	for _, conn := range []*net.UDPConn{c.videoConn, c.audioConn, c.backchannelListener, c.backchannelRTCPListener} {
		if conn != nil {
			conn.Close()
		}
	}
}

func (rf *RTPForwarder) RemoveClient(sessionID string) {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	if client, exists := rf.clients[sessionID]; exists {
		client.close()
		delete(rf.clients, sessionID)
		core.Logger.Trace().Msgf("Removed RTP client %s", sessionID)
	}
}

func (rf *RTPForwarder) dropClients(sessionIDs []string) {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	for _, id := range sessionIDs {
		if client, exists := rf.clients[id]; exists {
			client.close()
			delete(rf.clients, id)
			core.Logger.Info().Msgf("Removing dead RTP client %s", id)
		}
	}
}

// snapshotClients copies the current client set into the direction's reusable
// slice, so packets are written without holding the forwarder lock.
func (rf *RTPForwarder) snapshotClients(into []*RTPClient) []*RTPClient {
	rf.mutex.RLock()
	defer rf.mutex.RUnlock()

	into = into[:0]
	for _, client := range rf.clients {
		into = append(into, client)
	}
	return into
}

// isDeadClientError reports whether the peer is gone for good, as opposed to a
// transient write failure worth logging.
func isDeadClientError(err error) bool {
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, net.ErrClosed)
}

func getNALType(packet *rtp.Packet) byte {
	if len(packet.Payload) == 0 {
		return 0
	}
	nalType := packet.Payload[0] & 0x1F
	if nalType == 28 && len(packet.Payload) > 1 {
		// FU-A: real NAL type is in second byte, only on start bit
		if packet.Payload[1]&0x80 != 0 {
			return packet.Payload[1] & 0x1F
		}
		return 0
	}
	return nalType
}

// cacheSTAP records the parameter sets inside a STAP-A aggregate (type 24).
// Callers hold s.mu.
func (s *streamState) cacheSTAP(packet *rtp.Packet) {
	payload := packet.Payload[1:] // skip STAP-A header byte
	for len(payload) > 2 {
		nalSize := int(payload[0])<<8 | int(payload[1])
		payload = payload[2:]
		if nalSize > len(payload) {
			break
		}
		switch payload[0] & 0x1F {
		case 7:
			s.sps = packet.Clone()
		case 8:
			s.pps = packet.Clone()
		}
		payload = payload[nalSize:]
	}
}

// rebase shifts a source timestamp so the session starts near zero, keeping the
// spacing between packets. The source value is what groups the fragments of one
// picture into an access unit, so it must survive: stamping each packet with the
// wall clock instead leaves a decoder with no frame boundaries and it gives up
// after a few packets with "no dts". Callers hold s.mu; uint32 wraps, as RTP
// expects.
func (s *streamState) rebase(timestamp uint32) uint32 {
	if !s.tsStarted {
		s.tsBase = timestamp
		s.tsStarted = true
	}
	return timestamp - s.tsBase
}

// marshal serialises packet into the direction's scratch buffer, leaving room
// for an interleaving header. The RTP bytes start at interleaveHeaderLen.
func (s *streamState) marshal(packet *rtp.Packet) ([]byte, error) {
	size := interleaveHeaderLen + packet.MarshalSize()
	if cap(s.buf) < size {
		s.buf = make([]byte, size)
	}
	s.buf = s.buf[:size]
	if _, err := packet.MarshalTo(s.buf[interleaveHeaderLen:]); err != nil {
		return nil, err
	}
	return s.buf, nil
}

func (rf *RTPForwarder) ForwardVideoPacket(packet *rtp.Packet) {
	s := &rf.video
	s.mu.Lock()
	defer s.mu.Unlock()

	s.snapshot = rf.snapshotClients(s.snapshot)
	if len(s.snapshot) == 0 {
		return
	}

	packet.Timestamp = s.rebase(packet.Timestamp)

	// Cache SPS (7), PPS (8), and STAP-A (24) which may carry both.
	nalType := getNALType(packet)
	switch nalType {
	case 7:
		s.sps = packet.Clone()
	case 8:
		s.pps = packet.Clone()
	case 24:
		s.cacheSTAP(packet)
	}

	// Before an IDR keyframe (5), replay the cached parameter sets. They belong
	// to the access unit they introduce, so they carry its timestamp.
	if nalType == 5 && s.sps != nil && s.pps != nil {
		s.sps.Timestamp = packet.Timestamp
		s.pps.Timestamp = packet.Timestamp
		rf.forward(s, s.sps, videoChannel)
		rf.forward(s, s.pps, videoChannel)
	}

	rf.forward(s, packet, videoChannel)
}

func (rf *RTPForwarder) ForwardAudioPacket(packet *rtp.Packet) {
	s := &rf.audio
	s.mu.Lock()
	defer s.mu.Unlock()

	s.snapshot = rf.snapshotClients(s.snapshot)
	if len(s.snapshot) == 0 {
		return
	}

	packet.Timestamp = s.rebase(packet.Timestamp)
	rf.forward(s, packet, audioChannel)
}

// forward writes one packet to every client in the direction's snapshot. The
// caller holds the direction lock; the forwarder lock is not held, so a slow
// client cannot block a session teardown.
func (rf *RTPForwarder) forward(s *streamState, packet *rtp.Packet, dir direction) {
	// The forwarder is the last hop, so it owns the sequence numbering: replayed
	// parameter sets would otherwise carry the numbers they were cached with and
	// arrive looking like duplicates.
	s.seq++
	packet.SequenceNumber = s.seq

	buf, err := s.marshal(packet)
	if err != nil {
		core.Logger.Error().Err(err).Msgf("Error marshaling %s RTP packet", dir)
		return
	}
	rtpData := buf[interleaveHeaderLen:]

	var dead []string

	for _, client := range s.snapshot {
		var writeErr error

		if client.transportMode == TransportUDP {
			conn := dir.conn(client)
			if conn == nil {
				continue
			}
			_, writeErr = conn.Write(rtpData)
		} else {
			if client.tcpConn == nil {
				continue
			}
			// Interleaved framing in front of the bytes already marshalled, so
			// header and payload leave in a single write.
			buf[0] = '$'
			buf[1] = dir.channel(client)
			buf[2] = byte(len(rtpData) >> 8)
			buf[3] = byte(len(rtpData))
			_, writeErr = client.tcpConn.Write(buf)
		}

		switch {
		case writeErr == nil:
			if !s.logged {
				s.logged = true
				core.Logger.Trace().Msgf("Successfully sent first %s packet to client %s", dir, client.sessionID)
			}
		case isDeadClientError(writeErr):
			dead = append(dead, client.sessionID)
		default:
			core.Logger.Error().Err(writeErr).Msgf("Error forwarding %s packet to client %s", dir, client.sessionID)
		}
	}

	if len(dead) > 0 {
		rf.dropClients(dead)
	}
}

func (rf *RTPForwarder) Stop() {
	rf.mutex.Lock()
	for sessionID, client := range rf.clients {
		client.close()
		delete(rf.clients, sessionID)
	}
	rf.mutex.Unlock()

	for _, s := range []*streamState{&rf.video, &rf.audio} {
		s.mu.Lock()
		s.tsStarted, s.logged = false, false
		s.sps, s.pps = nil, nil
		s.mu.Unlock()
	}

	core.Logger.Trace().Msg("RTPForwarder stopped and all clients cleared")
}

func (rf *RTPForwarder) handleUDPBackchannelRTP(sessionID string, listener *net.UDPConn) {
	defer listener.Close()

	buffer := make([]byte, 1500)

	for {
		n, _, err := listener.ReadFromUDP(buffer)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				core.Logger.Error().Err(err).Msgf("Error reading UDP RTP backchannel for client %s", sessionID)
			}
			return
		}

		if rf.OnBackchannelAudio == nil {
			continue
		}

		packet := &rtp.Packet{}
		if err := packet.Unmarshal(buffer[:n]); err != nil {
			continue
		}
		rf.OnBackchannelAudio(packet)
	}
}

// drainUDP discards everything on a socket that must stay bound but is never
// read, such as the backchannel's RTCP port.
func drainUDP(listener *net.UDPConn) {
	defer listener.Close()

	buffer := make([]byte, 1500)
	for {
		if _, _, err := listener.ReadFromUDP(buffer); err != nil {
			return
		}
	}
}
