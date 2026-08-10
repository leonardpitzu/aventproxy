package rtsp

import (
	"encoding/binary"
	"math"
	"testing"
)

// pcm16 renders a sine at freq into 16 kHz signed little-endian samples.
func pcm16(freq float64, samples int, amplitude float64) []byte {
	out := make([]byte, samples*2)
	for i := range samples {
		v := int16(amplitude * math.Sin(2*math.Pi*freq*float64(i)/lanAudioRate))
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}

// ulawToLinear is only needed to check what the encoder produced.
func ulawToLinear(b byte) int32 {
	b = ^b
	sign := b & 0x80
	exponent := int32(b>>4) & 0x07
	mantissa := int32(b & 0x0f)
	sample := ((mantissa << 3) + 0x84) << uint(exponent)
	sample -= 0x84
	if sign != 0 {
		return -sample
	}
	return sample
}

func TestEncodeHalvesTheRate(t *testing.T) {
	var e g711Encoder
	// 640 bytes is one 20 ms unit at 16 kHz; 8 kHz G.711 is 160 bytes.
	got := e.encode(pcm16(440, 320, 8000))
	if len(got) != 160 {
		t.Fatalf("encode returned %d bytes, want 160", len(got))
	}
}

func TestEncodeSilenceIsULawSilence(t *testing.T) {
	var e g711Encoder
	got := e.encode(make([]byte, 640))
	for i, b := range got {
		if b != 0xff {
			t.Fatalf("sample %d is %#x, want 0xff: digital zero must compand to u-law silence", i, b)
		}
	}
}

// A 440 Hz tone sits well inside the passband, so it has to survive the
// decimation with its amplitude intact.
func TestEncodePreservesPassbandAmplitude(t *testing.T) {
	var e g711Encoder
	got := e.encode(pcm16(440, 3200, 8000))

	var peak int32
	for _, b := range got {
		if v := ulawToLinear(b); v > peak {
			peak = v
		}
	}
	if peak < 7000 || peak > 9000 {
		t.Fatalf("peak %d, want roughly 8000: the passband should pass unattenuated", peak)
	}
}

// Everything above 5 kHz would alias into the 8 kHz output, so the filter has
// to bury it rather than fold it back. The first outputs are skipped: the tone
// starts abruptly against an empty window, and that step is broadband.
func TestEncodeRejectsWhatWouldAlias(t *testing.T) {
	var e g711Encoder
	got := e.encode(pcm16(6000, 3200, 8000))

	var peak int32
	for _, b := range got[len(decimateTaps):] {
		if v := ulawToLinear(b); v > peak {
			peak = v
		}
	}
	if peak > 400 { // -26 dB of the input, and the filter measures far better
		t.Fatalf("peak %d, want a 6 kHz tone attenuated to near nothing", peak)
	}
}

// The window carries across calls, so splitting a run into units must give the
// same bytes as encoding it in one go.
func TestEncodeIsContinuousAcrossUnits(t *testing.T) {
	tone := pcm16(1000, 1280, 8000)

	var whole g711Encoder
	want := whole.encode(tone)

	var split g711Encoder
	var got []byte
	for i := 0; i < len(tone); i += 640 {
		got = append(got, split.encode(tone[i:i+640])...)
	}

	if len(got) != len(want) {
		t.Fatalf("split encoding gave %d bytes, whole gave %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d differs: split %#x, whole %#x", i, got[i], want[i])
		}
	}
}

func TestALawSilence(t *testing.T) {
	e := g711Encoder{alaw: true}
	got := e.encode(make([]byte, 640))
	for i, b := range got {
		if b != 0xd5 {
			t.Fatalf("sample %d is %#x, want 0xd5: A-law silence", i, b)
		}
	}
}
