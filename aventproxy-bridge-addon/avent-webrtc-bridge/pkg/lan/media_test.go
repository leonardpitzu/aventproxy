package lan

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// message builds a video media message declaring `declared` bytes of unit while
// carrying `payload`, which is how the monitor announces a split payload.
func message(ts uint64, declared int, payload []byte) []byte {
	msg := make([]byte, mediaHeaderLen+subHeaderLen+len(payload))
	binary.LittleEndian.PutUint32(msg[:4], CmdMediaVideo)
	binary.LittleEndian.PutUint16(msg[26:28], 1280)
	binary.LittleEndian.PutUint16(msg[28:30], 720)
	msg[30] = 20
	sub := msg[mediaHeaderLen:]
	binary.LittleEndian.PutUint32(sub[:4], uint32(declared+subHeaderOverhead))
	binary.LittleEndian.PutUint64(sub[4:12], ts)
	copy(sub[subHeaderLen:], payload)
	return msg
}

func add(t *testing.T, a *videoAssembler, msg []byte) *VideoFrame {
	t.Helper()
	cmd, ts, declared, payload, ok := mediaPayload(msg)
	if !ok || cmd != CmdMediaVideo {
		t.Fatalf("message did not parse as video")
	}
	return a.add(msg, ts, declared, payload)
}

func TestAssemblerPassesWholePayloadsStraightThrough(t *testing.T) {
	var a videoAssembler
	payload := []byte{0x7c, 0x85, 0x01, 0x02, 0x03}

	frame := add(t, &a, message(1000, len(payload), payload))
	if frame == nil {
		t.Fatal("a payload that fits in one message should come out at once")
	}
	if !bytes.Equal(frame.NAL, payload) {
		t.Fatalf("payload = % x, want % x", frame.NAL, payload)
	}
	if frame.Width != 1280 || frame.Height != 720 || frame.FPS != 20 {
		t.Fatalf("description lost: %dx%d at %d", frame.Width, frame.Height, frame.FPS)
	}
}

func TestAssemblerJoinsASplitPayload(t *testing.T) {
	var a videoAssembler
	head := []byte{0x7c, 0x85, 0xaa, 0xbb}
	tail := []byte{0xcc, 0xdd, 0xee}
	whole := append(append([]byte{}, head...), tail...)

	if frame := add(t, &a, message(1000, len(whole), head)); frame != nil {
		t.Fatal("a payload declaring more than it carries is not finished yet")
	}
	frame := add(t, &a, message(1001, len(tail), tail))
	if frame == nil {
		t.Fatal("the continuation should complete the unit")
	}
	if !bytes.Equal(frame.NAL, whole) {
		t.Fatalf("assembled % x, want % x", frame.NAL, whole)
	}
	// The unit belongs to when it started, not when it finished.
	if frame.Timestamp != 1000 {
		t.Fatalf("timestamp = %d, want the opening message's 1000", frame.Timestamp)
	}
}

func TestAssemblerJoinsAcrossSeveralMessages(t *testing.T) {
	var a videoAssembler
	parts := [][]byte{{0x7c, 0x85}, {0x01, 0x02}, {0x03, 0x04}, {0x05}}
	total := 7

	var got *VideoFrame
	for i, part := range parts {
		declared := len(part)
		if i == 0 {
			declared = total
		}
		if frame := add(t, &a, message(uint64(1000+i), declared, part)); frame != nil {
			got = frame
		}
	}
	if got == nil {
		t.Fatal("the unit never completed")
	}
	if want := []byte{0x7c, 0x85, 0x01, 0x02, 0x03, 0x04, 0x05}; !bytes.Equal(got.NAL, want) {
		t.Fatalf("assembled % x, want % x", got.NAL, want)
	}
}

// A unit that is finished mid-message must not swallow what follows it.
func TestAssemblerStopsAtTheDeclaredLength(t *testing.T) {
	var a videoAssembler

	if frame := add(t, &a, message(1000, 6, []byte{0x7c, 0x85, 0xaa})); frame != nil {
		t.Fatal("not finished yet")
	}
	frame := add(t, &a, message(1001, 4, []byte{0xbb, 0xcc, 0xdd, 0xee}))
	if frame == nil {
		t.Fatal("the unit should have completed")
	}
	if len(frame.NAL) != 6 {
		t.Fatalf("assembled %d bytes, want the declared 6", len(frame.NAL))
	}
}

// A corrupt length must not make the assembler buffer without end.
func TestAssemblerRefusesAnImpossibleLength(t *testing.T) {
	var a videoAssembler
	if frame := add(t, &a, message(1000, maxUnitSize+1, []byte{0x7c, 0x85})); frame != nil {
		t.Fatal("an impossible length should be dropped, not assembled")
	}
	if a.declared != 0 {
		t.Fatal("the assembler should stay idle after refusing a unit")
	}
}

// The assembler is reusable: a completed unit must not leak into the next.
func TestAssemblerResetsBetweenUnits(t *testing.T) {
	var a videoAssembler

	add(t, &a, message(1000, 4, []byte{0x7c, 0x85}))
	first := add(t, &a, message(1001, 2, []byte{0x01, 0x02}))
	if first == nil || len(first.NAL) != 4 {
		t.Fatalf("first unit = %v", first)
	}

	second := add(t, &a, message(2000, 3, []byte{0x67, 0x42, 0x00}))
	if second == nil {
		t.Fatal("the next whole payload should come straight out")
	}
	if want := []byte{0x67, 0x42, 0x00}; !bytes.Equal(second.NAL, want) {
		t.Fatalf("second unit = % x, want % x", second.NAL, want)
	}
}
