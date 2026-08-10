package rtsp

import (
	"encoding/binary"
	"testing"
)

// Reference values taken from ffmpeg's own decoders. The expanders were checked
// against it for all 256 codewords; these are the corners worth pinning.
func TestExpandersAgreeWithTheReference(t *testing.T) {
	for _, tc := range []struct {
		codeword byte
		ulaw     int16
		alaw     int16
	}{
		{0x00, -32124, -5504},
		{0x7f, 0, -848},
		{0x80, 32124, 5504},
		{0xd5, 716, 8},
		{0xff, 0, 848},
	} {
		if got := ulawToLinear(tc.codeword); got != tc.ulaw {
			t.Errorf("ulawToLinear(%#x) = %d, want %d", tc.codeword, got, tc.ulaw)
		}
		if got := alawToLinear(tc.codeword); got != tc.alaw {
			t.Errorf("alawToLinear(%#x) = %d, want %d", tc.codeword, got, tc.alaw)
		}
	}
}

func TestPCMByteOrderIsSwapped(t *testing.T) {
	// -2 and 300 little-endian, which RTP L16 wants the other way round.
	got := pcmLEtoBE([]byte{0xfe, 0xff, 0x2c, 0x01})
	if want := []byte{0xff, 0xfe, 0x01, 0x2c}; string(got) != string(want) {
		t.Fatalf("got % x, want % x", got, want)
	}
	for i := 0; i+1 < len(got); i += 2 {
		if binary.BigEndian.Uint16(got[i:]) != binary.LittleEndian.Uint16([]byte{0xfe, 0xff, 0x2c, 0x01}[i:]) {
			t.Fatalf("sample %d did not survive the swap", i/2)
		}
	}
}

// A trailing odd byte is not half a sample; it must not be emitted.
func TestPCMByteOrderDropsAStrayByte(t *testing.T) {
	if got := pcmLEtoBE([]byte{0x01, 0x02, 0x03}); len(got) != 2 {
		t.Fatalf("got %d bytes, want 2", len(got))
	}
}

// The cloud gives 8 kHz against the 16 kHz the description announces, so the
// decoder has to produce two samples per codeword or the stream plays slow.
func TestDecoderDoublesTheRate(t *testing.T) {
	d := g711Decoder{}
	got := d.decode([]byte{0xff, 0xff, 0xff, 0xff})
	if want := 4 * 4; len(got) != want {
		t.Fatalf("got %d bytes for 4 codewords, want %d", len(got), want)
	}
}

// Every second sample is the real one; only the samples between are invented.
func TestDecoderKeepsTheOriginalSamples(t *testing.T) {
	d := g711Decoder{}
	codewords := []byte{0x80, 0x00, 0xff}
	got := d.decode(codewords)

	for i, codeword := range codewords {
		want := ulawToLinear(codeword)
		actual := int16(binary.BigEndian.Uint16(got[i*4+2:]))
		if actual != want {
			t.Fatalf("codeword %d decoded to %d, want %d", i, actual, want)
		}
	}
}

func TestDecoderHonoursALaw(t *testing.T) {
	d := g711Decoder{alaw: true}
	got := d.decode([]byte{0xd5})
	if want := alawToLinear(0xd5); int16(binary.BigEndian.Uint16(got[2:])) != want {
		t.Fatalf("A-law codeword decoded as u-law")
	}
}

func TestCodecIsALaw(t *testing.T) {
	for mime, want := range map[string]bool{
		"audio/PCMA": true,
		"audio/pcma": true,
		"audio/PCMU": false,
		"audio/opus": false,
		"":           false,
	} {
		if got := codecIsALaw(mime); got != want {
			t.Errorf("codecIsALaw(%q) = %v, want %v", mime, got, want)
		}
	}
}
