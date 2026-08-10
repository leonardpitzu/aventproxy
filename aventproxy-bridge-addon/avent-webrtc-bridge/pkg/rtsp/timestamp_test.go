package rtsp

import (
	"testing"

	"github.com/pion/rtp"
)

func videoPacket(timestamp uint32, payload ...byte) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, Timestamp: timestamp},
		Payload: payload,
	}
}

// Fragments of one picture share a source timestamp, and that is the only
// thing telling a decoder where the access unit ends.
func TestRebaseKeepsFragmentsTogether(t *testing.T) {
	var s streamState

	first := s.rebase(900000)
	second := s.rebase(900000)
	next := s.rebase(900000 + 4500) // the following picture, 20 fps at 90 kHz

	if first != 0 {
		t.Fatalf("first timestamp = %d, want 0: the session should start at zero", first)
	}
	if second != first {
		t.Fatalf("fragments of one picture got %d and %d, want the same value", first, second)
	}
	if next-first != 4500 {
		t.Fatalf("spacing between pictures = %d, want 4500", next-first)
	}
}

// Parameter sets are replayed ahead of a keyframe, so they belong to that
// access unit rather than to whenever they were cached.
func TestParameterSetsTakeTheKeyframeTimestamp(t *testing.T) {
	rf := NewRTPForwarder()
	rf.clients["s1"] = &RTPClient{sessionID: "s1", transportMode: TransportTCP}

	rf.ForwardVideoPacket(videoPacket(1000, 0x67, 0x42))       // SPS
	rf.ForwardVideoPacket(videoPacket(1000, 0x68, 0xce))       // PPS
	rf.ForwardVideoPacket(videoPacket(5500, 0x65, 0x88, 0x84)) // IDR

	if rf.video.sps.Timestamp != rf.video.pps.Timestamp {
		t.Fatalf("SPS %d and PPS %d disagree", rf.video.sps.Timestamp, rf.video.pps.Timestamp)
	}
	if want := uint32(4500); rf.video.sps.Timestamp != want {
		t.Fatalf("replayed parameter sets carry %d, want the keyframe's %d", rf.video.sps.Timestamp, want)
	}
}

// The forwarder is the last hop, so every packet it writes has to advance the
// sequence number, replayed parameter sets included.
func TestForwarderNumbersEveryPacket(t *testing.T) {
	rf := NewRTPForwarder()
	rf.clients["s1"] = &RTPClient{sessionID: "s1", transportMode: TransportTCP}

	rf.ForwardVideoPacket(videoPacket(1000, 0x67, 0x42))
	rf.ForwardVideoPacket(videoPacket(1000, 0x68, 0xce))
	rf.ForwardVideoPacket(videoPacket(5500, 0x65, 0x88, 0x84))

	// SPS, PPS, then the replayed pair and the IDR itself.
	if want := uint16(5); rf.video.seq != want {
		t.Fatalf("sequence counter = %d, want %d", rf.video.seq, want)
	}
}

// Video and audio are independent streams and must not share a clock.
func TestDirectionsRebaseIndependently(t *testing.T) {
	rf := NewRTPForwarder()
	rf.clients["s1"] = &RTPClient{sessionID: "s1", transportMode: TransportTCP}

	video := videoPacket(90000, 0x41, 0x9a)
	rf.ForwardVideoPacket(video)

	audio := &rtp.Packet{Header: rtp.Header{Version: 2, Timestamp: 160}, Payload: []byte{0xff}}
	rf.ForwardAudioPacket(audio)

	if video.Timestamp != 0 || audio.Timestamp != 0 {
		t.Fatalf("video %d and audio %d, want each rebased to its own zero", video.Timestamp, audio.Timestamp)
	}
}
