package lan

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"time"
)

// Application-layer constants for the media channel.
const (
	// mediaMagic prefixes every control message. Media messages omit it and
	// start with their command instead.
	mediaMagic = 0x12345678

	CmdMediaVideo = 0x00010003
	CmdMediaAudio = 0x00010005

	macLen         = sha1.Size // trailing HMAC on every datagram
	mediaHeaderLen = 32        // command + stream description
	subHeaderLen   = 16        // length + timestamp + flags
	// subHeaderOverhead is what the length field counts besides the payload:
	// the 8-byte timestamp and the 4-byte flags.
	subHeaderOverhead = 12
)

// startupMessages are the control messages the app sends straight after the
// login to make the monitor start streaming. They carry no session-specific
// data, so they are replayed verbatim.
var startupMessages = []string{
	"7856341200000000000000000a0000000400000000000100",
	"785634120200000000000000020000000400000000000000",
	"78563412040001000000000009000000080000000000000002000000",
	"78563412030001000000000006000000080000000000000000000000",
	"78563412050001000000000006000400080000000000000004000000",
	"785634120600000000000000050000000400000000000000",
}

// LoginToken is the credential the monitor's P2P layer expects.
//
// md5(devicePassword + "||" + localKey). The password comes from the RTC
// config and the local key from the device object, so a deployment needs both
// cloud lookups once and neither of them again.
func LoginToken(devicePassword, localKey string) string {
	sum := md5.Sum([]byte(devicePassword + "||" + localKey))
	return hex.EncodeToString(sum[:])
}

// buildLogin renders the cmd-1 message: fixed 32-byte username and token
// fields, then 32 reserved bytes the monitor expects but never populates.
func buildLogin(token string) []byte {
	msg := make([]byte, 8+32+32+32)
	binary.LittleEndian.PutUint32(msg[0:4], mediaMagic)
	binary.LittleEndian.PutUint32(msg[4:8], 1)
	copy(msg[8:40], "admin")
	copy(msg[40:72], token)
	return msg
}

// seal encrypts one application message: a random IV followed by AES-CBC.
func seal(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+pad)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	out := make([]byte, aes.BlockSize+len(padded))
	if _, err := rand.Read(out[:aes.BlockSize]); err != nil {
		return nil, err
	}
	cipher.NewCBCEncrypter(block, out[:aes.BlockSize]).CryptBlocks(out[aes.BlockSize:], padded)
	return out, nil
}

// open reverses seal.
func open(key, data []byte) ([]byte, error) {
	if len(data) < 2*aes.BlockSize || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("lan: media payload is not a whole number of blocks")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data)-aes.BlockSize)
	cipher.NewCBCDecrypter(block, data[:aes.BlockSize]).CryptBlocks(out, data[aes.BlockSize:])
	if n := int(out[len(out)-1]); n >= 1 && n <= aes.BlockSize && n <= len(out) {
		out = out[:len(out)-n]
	}
	return out, nil
}

// macConn adapts an ICE connection to the datagram shape kcp expects, adding
// and checking the trailing HMAC the monitor requires.
//
// Every datagram on this channel is a KCP segment followed by
// HMAC-SHA1(aes-key, segment). Sending without it produces no reply at all.
type macConn struct {
	inner net.Conn
	key   []byte
	raddr net.Addr
}

func (m *macConn) ReadFrom(p []byte) (int, net.Addr, error) {
	buf := make([]byte, len(p)+macLen)
	n, err := m.inner.Read(buf)
	if err != nil {
		return 0, m.raddr, err
	}
	if n < macLen {
		return 0, m.raddr, errors.New("lan: datagram shorter than its MAC")
	}
	body := buf[:n-macLen]
	if !hmac.Equal(mac(m.key, body), buf[n-macLen:n]) {
		return 0, m.raddr, errors.New("lan: datagram MAC mismatch")
	}
	return copy(p, body), m.raddr, nil
}

func (m *macConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	out := append(append([]byte{}, p...), mac(m.key, p)...)
	if _, err := m.inner.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (m *macConn) Close() error                       { return m.inner.Close() }
func (m *macConn) LocalAddr() net.Addr                { return m.inner.LocalAddr() }
func (m *macConn) SetDeadline(t time.Time) error      { return m.inner.SetDeadline(t) }
func (m *macConn) SetReadDeadline(t time.Time) error  { return m.inner.SetReadDeadline(t) }
func (m *macConn) SetWriteDeadline(t time.Time) error { return m.inner.SetWriteDeadline(t) }

func mac(key, body []byte) []byte {
	h := hmac.New(sha1.New, key)
	h.Write(body)
	return h.Sum(nil)
}

// VideoFrame is one decoded unit of the monitor's video stream.
type VideoFrame struct {
	Width     int
	Height    int
	FPS       int
	Timestamp uint64
	// NAL is a raw H.264 unit with no start code; the caller adds framing.
	NAL []byte
}

// parseMedia turns a decrypted media message into a frame.
func parseMedia(msg []byte) (*VideoFrame, bool) {
	if len(msg) < mediaHeaderLen+subHeaderLen {
		return nil, false
	}
	if binary.LittleEndian.Uint32(msg[:4]) != CmdMediaVideo {
		return nil, false
	}
	sub := msg[mediaHeaderLen:]
	length := int(binary.LittleEndian.Uint32(sub[:4]))
	if length < subHeaderOverhead {
		return nil, false
	}
	payload := sub[subHeaderLen:]
	if n := length - subHeaderOverhead; n >= 0 && n <= len(payload) {
		payload = payload[:n]
	}
	if len(payload) == 0 {
		return nil, false
	}
	return &VideoFrame{
		Width:     int(binary.LittleEndian.Uint16(msg[26:28])),
		Height:    int(binary.LittleEndian.Uint16(msg[28:30])),
		FPS:       int(msg[30]),
		Timestamp: binary.LittleEndian.Uint64(sub[4:12]),
		NAL:       payload,
	}, true
}

// startupSequence returns the login followed by the stream-start messages.
func startupSequence(token string) ([][]byte, error) {
	out := [][]byte{buildLogin(token)}
	for _, h := range startupMessages {
		raw, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("lan: bad startup template: %w", err)
		}
		out = append(out, raw)
	}
	return out, nil
}
