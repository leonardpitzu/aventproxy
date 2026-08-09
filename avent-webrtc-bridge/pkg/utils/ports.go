package utils

import (
	"fmt"
	"net"
	"sync"
)

type PortAllocator struct {
	mu sync.Mutex
}

func NewPortAllocator() *PortAllocator {
	return &PortAllocator{}
}

type UDPPortPair struct {
	RTPListener  *net.UDPConn
	RTCPListener *net.UDPConn
	RTPPort      int
	RTCPPort     int
}

func (pa *PortAllocator) GetConsecutiveUDPPorts(ip net.IP, maxAttempts int) (*UDPPortPair, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	if ip == nil {
		ip = net.IPv4(0, 0, 0, 0)
	}

	for i := 0; i < maxAttempts; i++ {
		// Get a random even port from the OS
		tempListener, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: 0})
		if err != nil {
			continue
		}

		addr := tempListener.LocalAddr().(*net.UDPAddr)
		basePort := addr.Port
		tempListener.Close()

		// Make it even if it's odd
		if basePort%2 == 1 {
			basePort--
		}

		// Try to bind both ports
		rtpListener, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: basePort})
		if err != nil {
			continue
		}

		rtcpListener, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: basePort + 1})
		if err != nil {
			rtpListener.Close()
			continue
		}

		return &UDPPortPair{
			RTPListener:  rtpListener,
			RTCPListener: rtcpListener,
			RTPPort:      basePort,
			RTCPPort:     basePort + 1,
		}, nil
	}

	return nil, fmt.Errorf("failed to allocate consecutive UDP ports after %d attempts", maxAttempts)
}

var DefaultPortAllocator = NewPortAllocator()
