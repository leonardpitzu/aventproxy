package lan

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	kcp "github.com/xtaci/kcp-go/v5"
)

// Client streams from one monitor over the local network, without the cloud.
//
// The only credentials it needs are the device's localKey and password, both
// of which come from a single cloud device lookup at setup time and can be
// cached indefinitely.
type Client struct {
	DeviceID string
	LocalKey string
	Password string
	UID      string
	// IP skips discovery when Home Assistant already resolved the monitor.
	IP string

	// OnRawMedia receives every decrypted media message, headers included,
	// before anything is made of it. The framing above the payload can only be
	// established against real hardware, so the `lan` diagnostic captures it
	// here rather than guessing at it.
	OnRawMedia func(msg []byte)

	session *Session
	sock    *net.UDPConn
	router  *packetRouter
	// stream carries control messages, media the video and audio.
	stream *kcp.UDPSession
	media  []*kcp.UDPSession

	mu      sync.Mutex
	closed  bool
	onFrame func(*VideoFrame)
	onAudio func(*AudioFrame)

	statsMu sync.Mutex
	stats   Stats
}

// Stats counts what the media channel delivered, so a stream that is missing
// most of its pictures can be told apart from one that never received them.
type Stats struct {
	Reads      int
	OpenErrors int
	Malformed  int
	Video      int
	Audio      int
	// Frames counts what the assembler emitted. Equal to Video means it never
	// joined anything and is not needed.
	Frames int
	// Raw samples each video message before assembly, as declared/carried and
	// the first payload bytes. Assembly is driven by the declared length, so
	// this is where a wrong reading of it shows.
	Raw []string
	// Other counts commands the dispatcher has no case for. Anything here is
	// media this bridge is discarding.
	Other map[uint32]int
}

// rawSamples is how many messages are recorded verbatim, and rawEvery spaces
// them out: the opening burst is parameter sets and a keyframe, which is the
// least representative part of the stream.
const (
	rawSamples = 12
	rawEvery   = 100
)

func (c *Client) sample(declared int, ts uint64, payload []byte) {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	if len(c.stats.Raw) >= rawSamples || c.stats.Video%rawEvery != 1 {
		return
	}
	c.stats.Raw = append(c.stats.Raw, fmt.Sprintf("#%d d%d/c%d t%d:%s",
		c.stats.Video, declared, len(payload), ts, hex.EncodeToString(payload[:min(len(payload), 4)])))
}

func (c *Client) count(field *int) {
	c.statsMu.Lock()
	*field++
	c.statsMu.Unlock()
}

func (c *Client) countOther(cmd uint32) {
	c.statsMu.Lock()
	if c.stats.Other == nil {
		c.stats.Other = map[uint32]int{}
	}
	c.stats.Other[cmd]++
	c.statsMu.Unlock()
}

// Snapshot returns the counts so far.
func (c *Client) Snapshot() Stats {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	out := c.stats
	out.Other = make(map[uint32]int, len(c.stats.Other))
	for cmd, n := range c.stats.Other {
		out.Other[cmd] = n
	}
	return out
}

// NewClient prepares a client; nothing touches the network until Start.
// Either callback may be nil, in which case that stream is discarded.
func NewClient(deviceID, localKey, password, uid, ip string, onFrame func(*VideoFrame), onAudio func(*AudioFrame)) *Client {
	return &Client{
		DeviceID: deviceID,
		LocalKey: localKey,
		Password: password,
		UID:      uid,
		IP:       ip,
		onFrame:  onFrame,
		onAudio:  onAudio,
	}
}

var candidateRE = regexp.MustCompile(`candidate:(\S+) (\d+) (\S+) (\d+) (\S+) (\d+) typ (\S+)`)

// Start discovers the monitor, negotiates a session and begins streaming.
func (c *Client) Start(ctx context.Context) error {
	ip := c.IP
	if ip == "" {
		dev, err := Discover(c.DeviceID, 12*time.Second)
		if err != nil {
			return err
		}
		if dev.Protocol < 3.5 {
			return fmt.Errorf("lan: %s announces protocol %.1f, local streaming needs 3.5", c.DeviceID, dev.Protocol)
		}
		ip = dev.IP
	}

	var err error
	c.session, err = Dial(ip, c.DeviceID, c.LocalKey)
	if err != nil {
		return err
	}

	sock, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return err
	}
	c.sock = sock

	local, err := newICECreds()
	if err != nil {
		return err
	}
	offer, err := NewOffer(c.UID, c.DeviceID, local.Ufrag, local.Pwd)
	if err != nil {
		return err
	}
	if err := offer.SendOffer(c.session); err != nil {
		return err
	}

	hostIP, err := localAddrFor(&net.UDPAddr{IP: net.ParseIP(ip), Port: Port})
	if err != nil {
		return err
	}
	port := sock.LocalAddr().(*net.UDPAddr).Port
	if err := offer.SendCandidate(c.session, hostCandidate(hostIP, port)); err != nil {
		return err
	}

	remote, aesKey, peer, err := c.collectAnswer(offer)
	if err != nil {
		return err
	}

	connCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := iceConnect(connCtx, sock, peer, local, remote); err != nil {
		return err
	}

	// The monitor splits control and media across two KCP conversations, so a
	// router under both sessions sorts the datagrams by conversation id.
	router := newPacketRouter(sock, peer, aesKey, local)
	c.router = router

	control, err := newKCP(router, convControl, peer)
	if err != nil {
		return err
	}
	c.stream = control
	if err := c.login(aesKey); err != nil {
		return err
	}
	go c.attachMedia(ctx, peer, aesKey)
	go c.keepAlive(ctx)
	return nil
}

// attachMedia starts a reader for each conversation the monitor opens.
//
// Which id carries the video is not fixed: it has been seen as 1 and as 2 on
// the same camera, so the session follows whatever arrives.
func (c *Client) attachMedia(ctx context.Context, peer *net.UDPAddr, aesKey []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		case conv := <-c.router.opened:
			if conv == convControl {
				continue
			}
			stream, err := newKCP(c.router, conv, peer)
			if err != nil {
				debugf("lan: conversation %d could not be opened: %v", conv, err)
				continue
			}
			c.mu.Lock()
			closed := c.closed
			if !closed {
				c.media = append(c.media, stream)
			}
			c.mu.Unlock()
			if closed {
				stream.Close()
				return
			}
			go c.readLoop(ctx, stream, aesKey)
		}
	}
}

// collectAnswer waits for the monitor's answer and its first host candidate.
func (c *Client) collectAnswer(offer *Offer) (remote iceCreds, aesKey []byte, peer *net.UDPAddr, err error) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		frame, err := ReadSignal(c.session, time.Until(deadline))
		if err != nil {
			return iceCreds{}, nil, nil, err
		}
		switch frame.Header.Type {
		case TypeAnswer:
			remote.Ufrag = sdpAttr(frame.Msg.SDP, "ice-ufrag")
			remote.Pwd = sdpAttr(frame.Msg.SDP, "ice-pwd")
			raw, decErr := hex.DecodeString(sdpAttr(frame.Msg.SDP, "aes-key"))
			if decErr != nil {
				return iceCreds{}, nil, nil, fmt.Errorf("lan: bad aes-key in answer: %w", decErr)
			}
			aesKey = raw
		case TypeCandidate:
			if addr, parseErr := parseCandidate(frame.Msg.Candidate); parseErr == nil {
				peer = addr
			}
		case TypeDisconnect:
			return iceCreds{}, nil, nil, errors.New("lan: monitor refused the session")
		}
		if remote.Ufrag != "" && peer != nil {
			return remote, aesKey, peer, nil
		}
	}
	return iceCreds{}, nil, nil, errors.New("lan: monitor never completed the answer")
}

func (c *Client) login(aesKey []byte) error {
	msgs, err := startupSequence(LoginToken(c.Password, c.LocalKey))
	if err != nil {
		return err
	}
	for _, msg := range msgs {
		sealed, err := seal(aesKey, msg)
		if err != nil {
			return err
		}
		if _, err := c.stream.Write(sealed); err != nil {
			return err
		}
		time.Sleep(120 * time.Millisecond)
	}
	return nil
}

func (c *Client) readLoop(ctx context.Context, stream *kcp.UDPSession, aesKey []byte) {
	buf := make([]byte, 65535)
	// One assembler per conversation: a split payload is continued by the next
	// message on the same stream.
	var video videoAssembler
	for {
		if ctx.Err() != nil {
			return
		}
		if err := stream.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return
		}
		n, err := stream.Read(buf)
		if err != nil {
			return
		}
		c.count(&c.stats.Reads)
		msg, err := open(aesKey, buf[:n])
		if err != nil {
			c.count(&c.stats.OpenErrors)
			continue
		}
		if c.OnRawMedia != nil {
			c.OnRawMedia(msg)
		}
		cmd, ts, declared, payload, ok := mediaPayload(msg)
		if !ok {
			c.count(&c.stats.Malformed)
			continue
		}
		switch cmd {
		case CmdMediaVideo:
			c.count(&c.stats.Video)
			c.sample(declared, ts, payload)
			frame := video.add(msg, ts, declared, payload)
			if frame != nil && c.onFrame != nil {
				c.count(&c.stats.Frames)
				c.onFrame(frame)
			}
		case CmdMediaAudio:
			c.count(&c.stats.Audio)
			if c.onAudio != nil {
				c.onAudio(parseAudio(msg, ts, payload))
			}
		default:
			c.countOther(cmd)
		}
	}
}

// keepAlive holds the signalling session open; the monitor drops an idle one.
func (c *Client) keepAlive(ctx context.Context) {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.session.Heartbeat(); err != nil {
				return
			}
		}
	}
}

// Close tears the session down.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.stream != nil {
		c.stream.Close()
	}
	for _, m := range c.media {
		m.Close()
	}
	if c.sock != nil {
		c.sock.Close()
	}
	if c.session != nil {
		return c.session.Close()
	}
	return nil
}

func sdpAttr(sdp, name string) string {
	for _, line := range strings.Split(sdp, "\r\n") {
		if strings.HasPrefix(line, "a="+name+":") {
			return strings.TrimPrefix(line, "a="+name+":")
		}
	}
	return ""
}

func parseCandidate(line string) (*net.UDPAddr, error) {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "a="))
	m := candidateRE.FindStringSubmatch(line)
	if m == nil {
		return nil, errors.New("lan: unusable candidate")
	}
	ip := net.ParseIP(m[5])
	if ip == nil {
		return nil, errors.New("lan: candidate carries no address")
	}
	port, err := strconv.Atoi(m[6])
	if err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: ip, Port: port}, nil
}
