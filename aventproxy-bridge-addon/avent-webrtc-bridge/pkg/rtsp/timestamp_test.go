package rtsp

import (
	"testing"

	"github.com/pion/rtp"
)

// video builds a packet the way LANBridge does, with marker meaning "this
// payload completes the picture".
func video(payload []byte, marker bool) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, Marker: marker},
		Payload: payload,
	}
}

// Every packet of one picture must carry one timestamp. Stamping each fragment
// as it leaves dated the same picture at a spread of instants, and a decoder
// reading that literally sees one frame per fragment and assembles none.
func TestOnePictureCarriesOneTimestamp(t *testing.T) {
	rf := NewRTPForwarder()

	first := []*rtp.Packet{
		video([]byte{0x7c, 0x85, 0x11}, false), // FU-A start, IDR
		video([]byte{0x7c, 0x05, 0x22}, false), // continuation
		video([]byte{0x7c, 0x45, 0x33}, true),  // end bit: picture complete
	}
	second := []*rtp.Packet{
		video([]byte{0x7c, 0x81, 0x44}, false),
		video([]byte{0x7c, 0x41, 0x55}, true),
	}

	for _, p := range append(append([]*rtp.Packet{}, first...), second...) {
		rf.ForwardVideoPacket(p)
	}

	for _, p := range first[1:] {
		if p.Timestamp != first[0].Timestamp {
			t.Fatalf("fragments of one picture differ: %d vs %d", p.Timestamp, first[0].Timestamp)
		}
	}
	if second[1].Timestamp != second[0].Timestamp {
		t.Fatalf("fragments of the second picture differ: %d vs %d", second[1].Timestamp, second[0].Timestamp)
	}
	if second[0].Timestamp == first[0].Timestamp {
		t.Fatal("the marker bit did not close the picture; both share a timestamp")
	}
	if second[0].Timestamp < first[0].Timestamp {
		t.Fatal("the clock went backwards")
	}
}

// Parameter sets describe the picture that follows, so they belong to its
// access unit and must be dated with it.
func TestParameterSetsShareTheKeyframeTimestamp(t *testing.T) {
	rf := NewRTPForwarder()
	if err := rf.AddTCPClient("s", nil, 0, 2, 4); err != nil {
		t.Fatal(err)
	}

	rf.ForwardVideoPacket(video([]byte{0x67, 0x42, 0x00, 0x1e}, false)) // SPS
	rf.ForwardVideoPacket(video([]byte{0x68, 0xce, 0x3c, 0x80}, false)) // PPS
	idr := video([]byte{0x65, 0x88, 0x84}, true)
	rf.ForwardVideoPacket(idr)

	if rf.video.sps.Timestamp != idr.Timestamp {
		t.Fatalf("SPS dated %d, keyframe %d", rf.video.sps.Timestamp, idr.Timestamp)
	}
	if rf.video.pps.Timestamp != idr.Timestamp {
		t.Fatalf("PPS dated %d, keyframe %d", rf.video.pps.Timestamp, idr.Timestamp)
	}
}

// An audio timestamp marks when the audio was sampled, not when it arrived, so
// it advances by the samples carried and nothing else.
func TestAudioClockCountsSamples(t *testing.T) {
	rf := NewRTPForwarder()

	const samples = 320 // 20 ms of 16 kHz L16, the monitor's unit
	stamps := make([]uint32, 0, 5)
	for range cap(stamps) {
		p := &rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: audioPayloadType},
			Payload: make([]byte, samples*audioBytesPerSample),
		}
		rf.ForwardAudioPacket(p)
		stamps = append(stamps, p.Timestamp)
	}

	for i, a := range stamps[:len(stamps)-1] {
		if step := stamps[i+1] - a; step != samples {
			t.Fatalf("packet %d advanced the clock by %d, want %d", i, step, samples)
		}
	}
}
