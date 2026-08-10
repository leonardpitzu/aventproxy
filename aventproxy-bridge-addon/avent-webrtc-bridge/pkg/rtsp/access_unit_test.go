package rtsp

import "testing"

// fuA renders a fragment of a NAL: start and end bits, then the slice header
// byte whose top bit says first_mb_in_slice is zero.
func fuA(nalType byte, start, end, firstMB bool) []byte {
	indicator := byte(28)
	header := nalType & 0x1f
	if start {
		header |= 0x80
	}
	if end {
		header |= 0x40
	}
	slice := byte(0x00)
	if firstMB {
		slice = 0x80
	}
	return []byte{indicator, header, slice}
}

// whole renders an unfragmented NAL.
func whole(nalType byte, firstMB bool) []byte {
	slice := byte(0x00)
	if firstMB {
		slice = 0x80
	}
	return []byte{nalType & 0x1f, slice}
}

func TestNALTypeReadsThroughFragmentation(t *testing.T) {
	if got, begins := nalTypeOf(fuA(5, true, false, true)); got != 5 || !begins {
		t.Fatalf("FU-A start: type %d begins %v, want 5 true", got, begins)
	}
	if got, begins := nalTypeOf(fuA(5, false, false, false)); got != 5 || begins {
		t.Fatalf("FU-A middle: type %d begins %v, want 5 false", got, begins)
	}
	if got, _ := nalTypeOf(whole(7, false)); got != 7 {
		t.Fatalf("whole SPS read as type %d", got)
	}
	if got, _ := nalTypeOf([]byte{24, 0x00, 0x02, 0x67, 0x42}); got != 7 {
		t.Fatalf("STAP-A read as type %d, want the first aggregated NAL's 7", got)
	}
}

// One picture arriving as a dozen fragments must count once, which is the whole
// point: the monitor's own timestamps counted 300 for 300 messages.
func TestFragmentsOfOnePictureAreOneAccessUnit(t *testing.T) {
	var a accessUnits
	starts := 0

	for i := range 12 {
		payload := fuA(5, i == 0, i == 11, i == 0)
		if a.starts(payload) {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("a picture in 12 fragments opened %d access units, want 1", starts)
	}
}

func TestEachPictureOpensItsOwn(t *testing.T) {
	var a accessUnits
	starts := 0

	for range 3 {
		for i := range 4 {
			if a.starts(fuA(1, i == 0, i == 3, i == 0)) {
				starts++
			}
		}
	}
	if starts != 3 {
		t.Fatalf("three pictures opened %d access units, want 3", starts)
	}
}

// A second slice of the same picture does not start a new one.
func TestASecondSliceStaysInThePicture(t *testing.T) {
	var a accessUnits

	if !a.starts(whole(1, true)) {
		t.Fatal("the first slice should open a picture")
	}
	if a.starts(whole(1, false)) {
		t.Fatal("a slice that does not start at macroblock zero is the same picture")
	}
	if !a.starts(whole(1, true)) {
		t.Fatal("a slice at macroblock zero opens the next picture")
	}
}

// Parameter sets introduce the picture that follows, so they open the unit
// rather than trailing the previous one.
func TestParameterSetsOpenTheNextUnit(t *testing.T) {
	var a accessUnits

	a.starts(whole(5, true)) // a picture
	if !a.starts(whole(7, false)) {
		t.Fatal("SPS after a picture should open the next access unit")
	}
	if a.starts(whole(8, false)) {
		t.Fatal("PPS following that SPS is still the same access unit")
	}
	if a.starts(whole(6, false)) {
		t.Fatal("SEI following them is still the same access unit")
	}
	if a.starts(fuA(5, true, false, true)) {
		t.Fatal("the picture those parameter sets introduce is that same unit")
	}
}

func TestShortPayloadsAreIgnored(t *testing.T) {
	var a accessUnits
	for _, payload := range [][]byte{{}, {28}, {24, 0x00}} {
		if a.starts(payload) {
			t.Fatalf("payload % x should not open an access unit", payload)
		}
	}
}
