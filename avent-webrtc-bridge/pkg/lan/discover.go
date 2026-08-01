package lan

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DiscoveryPort carries the monitor's periodic broadcast, roughly every 5s.
const DiscoveryPort = 6667

// udpKeySeed is the well-known constant every Tuya device encrypts its
// discovery broadcast with. It is not a secret.
var udpKeySeed = []byte("yGAdlopoPVldABfn")

// Device is what a monitor announces about itself on the LAN.
type Device struct {
	IP        string  `json:"ip"`
	GwID      string  `json:"gwId"`
	ProductID string  `json:"productKey"`
	Version   string  `json:"version"`
	Encrypt   bool    `json:"encrypt"`
	Active    int     `json:"active"`
	Protocol  float64 `json:"-"`
}

// Discover listens for the broadcast of a specific device.
//
// The camera announces its own protocol version here, which is the only
// reliable way to know whether to speak 3.3 or 3.5: guessing 3.3 against a 3.5
// monitor yields a session that connects and then cannot read any frame.
func Discover(deviceID string, timeout time.Duration) (*Device, error) {
	// Home Assistant's own tinytuya listener already holds 6667, and a Tuya
	// announcement is a broadcast every listener may have a copy of.
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var sockErr error
		if err := c.Control(func(fd uintptr) {
			if sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); sockErr != nil {
				return
			}
			sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1)
		}); err != nil {
			return err
		}
		return sockErr
	}}
	conn, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", DiscoveryPort))
	if err != nil {
		return nil, fmt.Errorf("lan: listen %d: %w", DiscoveryPort, err)
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	key := md5.Sum(udpKeySeed)
	buf := make([]byte, 2048)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return nil, fmt.Errorf("lan: %s did not announce itself within %s: %w", deviceID, timeout, err)
		}
		dev, err := parseAnnouncement(key[:], buf[:n])
		if err != nil || dev.GwID != deviceID {
			continue
		}
		return dev, nil
	}
}

func parseAnnouncement(key, frame []byte) (*Device, error) {
	_, payload, err := Unpack6699(key, frame)
	if err != nil {
		return nil, err
	}
	var dev Device
	if err := json.Unmarshal(payload, &dev); err != nil {
		return nil, err
	}
	dev.Protocol = parseProtocol(dev.Version)
	return &dev, nil
}

func parseProtocol(version string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(version), 64)
	if err != nil {
		return 3.3
	}
	return v
}
