package rtsp

import "testing"

func TestSplitLeavesPayloadsThatFitAlone(t *testing.T) {
	var lb LANBridge
	payload := make([]byte, rtpMTU)
	payload[0] = 0x65 // a whole IDR NAL

	got := lb.split(payload)
	if len(got) != 1 || len(got[0]) != rtpMTU {
		t.Fatalf("split a payload that already fits into %d parts", len(got))
	}
}

// Re-wrapping something the monitor already fragmented is the mistake that
// produces a stream which flows and decodes to nothing.
func TestSplitNeverRewrapsAFragment(t *testing.T) {
	var lb LANBridge
	for _, nalType := range []byte{28, 29} {
		payload := make([]byte, rtpMTU*3)
		payload[0] = nalType
		payload[1] = 0x85

		got := lb.split(payload)
		if len(got) != 1 {
			t.Fatalf("NAL type %d was fragmented into %d parts; it is already a fragment", nalType, len(got))
		}
	}
}

func TestSplitFragmentsAnOversizedNAL(t *testing.T) {
	var lb LANBridge
	payload := make([]byte, rtpMTU*3)
	payload[0] = 0x65 // IDR, whole and too big for one packet
	for i := 1; i < len(payload); i++ {
		payload[i] = byte(i)
	}

	got := lb.split(payload)
	if len(got) < 2 {
		t.Fatalf("an oversized NAL came back in %d part(s), want it fragmented", len(got))
	}
	for i, part := range got {
		if len(part) > rtpMTU {
			t.Fatalf("part %d is %d bytes, over the %d limit", i, len(part), rtpMTU)
		}
		if part[0]&0x1f != 28 {
			t.Fatalf("part %d is NAL type %d, want FU-A", i, part[0]&0x1f)
		}
	}
}
