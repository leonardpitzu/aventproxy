package lan

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
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
	agent   *ice.Agent
	stream  *kcp.UDPSession

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

	agent, err := ice.NewAgent(&ice.AgentConfig{
		NetworkTypes: []ice.NetworkType{ice.NetworkTypeUDP4},
		Lite:         false,
	})
	if err != nil {
		return err
	}
	c.agent = agent

	localUfrag, localPwd, err := agent.GetLocalUserCredentials()
	if err != nil {
		return err
	}

	offer, err := NewOffer(c.UID, c.DeviceID, localUfrag, localPwd)
	if err != nil {
		return err
	}

	candidates := make(chan string, 16)
	if err := agent.OnCandidate(func(cand ice.Candidate) {
		if cand == nil {
			return
		}
		select {
		case candidates <- cand.Marshal():
		default:
		}
	}); err != nil {
		return err
	}
	if err := agent.GatherCandidates(); err != nil {
		return err
	}

	if err := offer.SendOffer(c.session); err != nil {
		return err
	}
	go func() {
		for cand := range candidates {
			if err := offer.SendCandidate(c.session, cand); err != nil {
				return
			}
		}
	}()

	remoteUfrag, remotePwd, aesKey, remote, err := c.collectAnswer(offer)
	if err != nil {
		return err
	}

	for _, cand := range remote {
		if err := agent.AddRemoteCandidate(cand); err != nil {
			return err
		}
	}

	connCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	iceConn, err := agent.Dial(connCtx, remoteUfrag, remotePwd)
	if err != nil {
		return fmt.Errorf("lan: ICE failed: %w", err)
	}

	// kcp carries the application messages; the MAC wrapper sits underneath so
	// kcp itself never sees the monitor's trailing HMAC.
	wrapper := &macConn{inner: iceConn, key: aesKey, raddr: iceConn.RemoteAddr()}
	stream, err := kcp.NewConn3(0, iceConn.RemoteAddr(), nil, 0, 0, wrapper)
	if err != nil {
		return err
	}
	stream.SetStreamMode(false) // preserve message boundaries
	stream.SetWindowSize(512, 512)
	stream.SetNoDelay(1, 10, 2, 1)
	c.stream = stream

	if err := c.login(aesKey); err != nil {
		return err
	}
	go c.readLoop(ctx, aesKey)
	go c.keepAlive(ctx)
	return nil
}

// collectAnswer waits for the monitor's answer and its host candidates.
func (c *Client) collectAnswer(offer *Offer) (ufrag, pwd string, aesKey []byte, remote []ice.Candidate, err error) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		frame, err := ReadSignal(c.session, time.Until(deadline))
		if err != nil {
			return "", "", nil, nil, err
		}
		switch frame.Header.Type {
		case TypeAnswer:
			ufrag = sdpAttr(frame.Msg.SDP, "ice-ufrag")
			pwd = sdpAttr(frame.Msg.SDP, "ice-pwd")
			raw, decErr := hex.DecodeString(sdpAttr(frame.Msg.SDP, "aes-key"))
			if decErr != nil {
				return "", "", nil, nil, fmt.Errorf("lan: bad aes-key in answer: %w", decErr)
			}
			aesKey = raw
		case TypeCandidate:
			cand, parseErr := parseCandidate(frame.Msg.Candidate)
			if parseErr == nil {
				remote = append(remote, cand)
			}
		case TypeDisconnect:
			return "", "", nil, nil, errors.New("lan: monitor refused the session")
		}
		if ufrag != "" && len(remote) > 0 {
			return ufrag, pwd, aesKey, remote, nil
		}
	}
	return "", "", nil, nil, errors.New("lan: monitor never completed the answer")
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
		if err := c.stream.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return
		}
		n, err := c.stream.Read(buf)
		if err != nil {
			return
		}
		msg, err := open(aesKey, buf[:n])
		if err != nil {
			continue
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
	if c.agent != nil {
		c.agent.Close()
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

func parseCandidate(line string) (ice.Candidate, error) {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "a="))
	if !candidateRE.MatchString(line) {
		return nil, errors.New("lan: unusable candidate")
	}
	return ice.UnmarshalCandidate(line)
}
