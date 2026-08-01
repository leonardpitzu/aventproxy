package lan

import (
	"context"
	"encoding/binary"
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

	session *Session
	sock    *net.UDPConn
	router  *packetRouter
	// stream carries control messages, media the video.
	stream *kcp.UDPSession
	media  *kcp.UDPSession

	mu      sync.Mutex
	closed  bool
	onFrame func(*VideoFrame)
}

// NewClient prepares a client; nothing touches the network until Start.
func NewClient(deviceID, localKey, password, uid, ip string, onFrame func(*VideoFrame)) *Client {
	return &Client{
		DeviceID: deviceID,
		LocalKey: localKey,
		Password: password,
		UID:      uid,
		IP:       ip,
		onFrame:  onFrame,
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
	media, err := newKCP(router, convMedia, peer)
	if err != nil {
		return err
	}
	c.stream = control
	c.media = media
	if err := c.login(aesKey); err != nil {
		return err
	}
	go c.readLoop(ctx, aesKey)
	go c.keepAlive(ctx)
	return nil
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

func (c *Client) readLoop(ctx context.Context, aesKey []byte) {
	buf := make([]byte, 65535)
	for {
		if ctx.Err() != nil {
			return
		}
		if err := c.media.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return
		}
		n, err := c.media.Read(buf)
		if err != nil {
			debugf("kcp: read ended: %v", err)
			return
		}
		msg, err := open(aesKey, buf[:n])
		if err != nil {
			debugf("kcp: undecryptable %d-byte message", n)
			continue
		}
		if Debugf != nil {
			debugf("kcp: message cmd=%#x len=%d", binary.LittleEndian.Uint32(msg[4:8]), len(msg))
		}
		if frame, ok := parseMedia(msg); ok && c.onFrame != nil {
			c.onFrame(frame)
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
	if c.media != nil {
		c.media.Close()
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
