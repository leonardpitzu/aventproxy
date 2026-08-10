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
			if err := rf.AddUDPClient("udp-session", 0, 0); err != nil {
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
