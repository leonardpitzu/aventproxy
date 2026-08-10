package rtsp

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func testPacket(payload ...byte) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SSRC: 1},
		Payload: payload,
	}
}

// TestForwardIsRaceFreeWhileClientsChurn drives both stream directions while
// clients come and go. Under -race this fails if the forward path mutates
// shared state or the client map without exclusive access, which is how the
// forwarder used to crash with "concurrent map writes" once a second viewer
// disconnected mid-stream.
func TestForwardIsRaceFreeWhileClientsChurn(t *testing.T) {
	rf := NewRTPForwarder()
	defer rf.Stop()

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Drain the interleaved side so writes never block.
	go io.Copy(io.Discard, client) //nolint:errcheck // test sink

	if err := rf.AddTCPClient("tcp-session", server, 0, 1, 2); err != nil {
		t.Fatalf("add tcp client: %v", err)
	}

	var wg sync.WaitGroup
	deadline := time.Now().Add(200 * time.Millisecond)

	wg.Add(3)
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			rf.ForwardVideoPacket(testPacket(0x65, 0x88)) // IDR slice
		}
	}()
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			rf.ForwardAudioPacket(testPacket(0x00, 0x01))
		}
	}()
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			if err := rf.AddUDPClient("udp-session", "127.0.0.1", 0, 0); err != nil {
				t.Errorf("add udp client: %v", err)
				return
			}
			rf.RemoveClient("udp-session")
		}
	}()

	wg.Wait()
}

// TestForwardCachesParameterSetsForLateJoiners checks that an SPS/PPS pair seen
// before a keyframe is replayed, which is what lets a client that connects
// mid-stream decode the first IDR it receives.
func TestForwardCachesParameterSetsForLateJoiners(t *testing.T) {
	rf := NewRTPForwarder()
	defer rf.Stop()

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	frames := make(chan int, 16)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, err := client.Read(buf)
			if err != nil {
				close(frames)
				return
			}
			frames <- n
		}
	}()

	if err := rf.AddTCPClient("session", server, 0, 1, 2); err != nil {
		t.Fatalf("add tcp client: %v", err)
	}

	rf.ForwardVideoPacket(testPacket(0x67, 0x42)) // SPS
	rf.ForwardVideoPacket(testPacket(0x68, 0xCE)) // PPS
	rf.ForwardVideoPacket(testPacket(0x65, 0x88)) // IDR

	// SPS, PPS, then SPS + PPS replayed ahead of the IDR, then the IDR itself.
	for i := 0; i < 5; i++ {
		select {
		case n, ok := <-frames:
			if !ok {
				t.Fatalf("stream closed after %d writes", i)
			}
			if n < interleaveHeaderLen {
				t.Fatalf("write %d is shorter than an interleaved header: %d bytes", i, n)
			}
		case <-time.After(time.Second):
			t.Fatalf("only %d of 5 expected writes arrived", i)
		}
	}
}

// TestRTPTicksStaysExactOverLongSessions guards the integer timestamp maths
// against the overflow a naive elapsed*clockRate product would hit.
func TestRTPTicksStaysExactOverLongSessions(t *testing.T) {
	cases := []struct {
		elapsed time.Duration
		want    uint32
	}{
		{0, 0},
		{time.Second, 90000},
		{time.Millisecond, 90},
		{time.Hour, 324000000},
		// 100 h is 32,400,000,000 ticks, which wraps the 32-bit field as RTP intends.
		{100 * time.Hour, 2335228928},
	}

	for _, tc := range cases {
		if got := rtpTicks(tc.elapsed, 90000); got != tc.want {
			t.Errorf("rtpTicks(%s) = %d, want %d", tc.elapsed, got, tc.want)
		}
	}
}

// TestDialClientUDPTargetsTheGivenHost pins the destination of a UDP media
// socket to the address passed in. It used to resolve "localhost" regardless,
// so RTP over UDP silently went nowhere for any client not on this host, and a
// test that only counted errors scored that as a clean run.
func TestDialClientUDPTargetsTheGivenHost(t *testing.T) {
	// TEST-NET-1 (RFC 5737): never routable, so nothing leaves the machine.
	conn, err := dialClientUDP("192.0.2.10", 5004)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if got := conn.RemoteAddr().String(); got != "192.0.2.10:5004" {
		t.Errorf("RTP destination is %s, want 192.0.2.10:5004", got)
	}
}

// TestUDPClientReceivesOffHost forwards a packet to a socket bound to a real
// non-loopback address of this machine, which is the case the old code lost.
func TestUDPClientReceivesOffHost(t *testing.T) {
	host := nonLoopbackIPv4(t)

	listener, err := net.ListenPacket("udp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listen on %s: %v", host, err)
	}
	defer listener.Close()

	port := listener.LocalAddr().(*net.UDPAddr).Port

	rf := NewRTPForwarder()
	defer rf.Stop()

	if err := rf.AddUDPClient("udp-session", host, port, 0); err != nil {
		t.Fatalf("add udp client: %v", err)
	}
	rf.ForwardVideoPacket(testPacket(0x65, 0x88)) // IDR

	buf := make([]byte, 2048)
	if err := listener.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	n, _, err := listener.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no RTP arrived at %s:%d: %v", host, port, err)
	}
	if n == 0 {
		t.Fatal("received an empty datagram")
	}
}

func TestClientHostReadsThePeerAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()

	dialed, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer dialed.Close()

	conn, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	defer conn.Close()

	if got := clientHost(conn); got != "127.0.0.1" {
		t.Errorf("clientHost = %q, want 127.0.0.1", got)
	}
	if got := clientHost(nil); got != "" {
		t.Errorf("clientHost(nil) = %q, want empty", got)
	}
}

func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("no interface list available: %v", err)
	}
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if ip := ipnet.IP.To4(); ip != nil {
			return ip.String()
		}
	}
	t.Skip("host has no non-loopback IPv4 address")
	return ""
}
