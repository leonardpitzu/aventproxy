package rtsp

import (
	"encoding/binary"
	"sync"
)

// The monitor's LAN audio is 16 kHz signed 16-bit little-endian PCM, 20 ms per
// unit, while the RTSP description advertises G.711 at 8 kHz for both
// transports. Converting here keeps one description valid whichever path a
// stream ends up taking.
const (
	lanAudioRate = 16000
	rtpAudioRate = 8000
)

// decimateTaps is a 15-tap Hamming-windowed sinc, 3.4 kHz cutoff at 16 kHz, in
// Q15. Halving the rate without it would fold the 4-8 kHz band back into the
// output; this leaves that band at -12.6 dB at 4 kHz and below -35 dB above 5.
var decimateTaps = [15]int32{9, 215, 202, -922, -1696, 1955, 9667, 13908, 9667, 1955, -1696, -922, 202, 215, 9}

// g711Encoder turns the monitor's PCM into the payload the description
// promises. The filter window persists across units, so a unit boundary does
// not click, and the lock guards it because the LAN read loop is not the only
// goroutine that can reach a bridge.
type g711Encoder struct {
	mu     sync.Mutex
	window [len(decimateTaps)]int16
	phase  int
	alaw   bool
}

// encode halves the sample rate and companders the result, returning one G.711
// payload per call.
func (e *g711Encoder) encode(pcm []byte) []byte {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]byte, 0, len(pcm)/4+1)
	for i := 0; i+1 < len(pcm); i += 2 {
		copy(e.window[:], e.window[1:])
		e.window[len(e.window)-1] = int16(binary.LittleEndian.Uint16(pcm[i:]))

		// Emit for every second input sample: that is the decimation.
		e.phase ^= 1
		if e.phase != 0 {
			continue
		}
		var acc int32
		for j, tap := range decimateTaps {
			acc += tap * int32(e.window[j])
		}
		sample := clip16(acc >> 15)
		if e.alaw {
			out = append(out, linearToALaw(sample))
		} else {
			out = append(out, linearToULaw(sample))
		}
	}
	return out
}

func clip16(v int32) int32 {
	switch {
	case v > 32767:
		return 32767
	case v < -32768:
		return -32768
	default:
		return v
	}
}

// ulawSegEnd and alawSegEnd are the ITU G.711 segment tables the exponent
// searches walk.
var (
	ulawSegEnd = [8]int32{0x3f, 0x7f, 0xff, 0x1ff, 0x3ff, 0x7ff, 0xfff, 0x1fff}
	alawSegEnd = [8]int32{0x1f, 0x3f, 0x7f, 0xff, 0x1ff, 0x3ff, 0x7ff, 0xfff}
)

// linearToULaw is the G.711 u-law compander, in the truncating form ITU-T
// G.711 defines: 14 significant bits, a bias, then a segment and a 4-bit
// mantissa, inverted. It can differ from a round-to-nearest encoder by one
// codeword, which is a fraction of a dB on a logarithmic scale.
func linearToULaw(sample int32) byte {
	const (
		bias = 0x84 >> 2
		clip = 8159
	)

	sample >>= 2
	mask := byte(0xff)
	if sample < 0 {
		sample = -sample
		mask = 0x7f
	}
	if sample > clip {
		sample = clip
	}
	sample += bias

	segment := len(ulawSegEnd)
	for i, end := range ulawSegEnd {
		if sample <= end {
			segment = i
			break
		}
	}
	if segment >= len(ulawSegEnd) {
		return 0x7f ^ mask
	}
	return (byte(segment<<4) | byte((sample>>uint(segment+1))&0x0f)) ^ mask
}

// alawSegEnd is the ITU G.711 segment table the A-law exponent search walks.

// linearToALaw is the G.711 A-law compander. Only reachable on a monitor whose
// skill advertises codec 106, but the payload type and the samples have to
// agree whichever one that is. A-law is defined on 13 bits and inverts a
// different bit mask per sign, so it is not u-law with another constant.
func linearToALaw(sample int32) byte {
	const clip = 32635
	if sample > clip {
		sample = clip
	} else if sample < -clip {
		sample = -clip
	}

	pcm := sample >> 3
	mask := byte(0xd5)
	if pcm < 0 {
		mask = 0x55
		pcm = -pcm - 1
	}

	segment := len(alawSegEnd)
	for i, end := range alawSegEnd {
		if pcm <= end {
			segment = i
			break
		}
	}
	if segment >= len(alawSegEnd) {
		return 0x7f ^ mask
	}

	shift := uint(1)
	if segment >= 2 {
		shift = uint(segment)
	}
	return (byte(segment<<4) | byte((pcm>>shift)&0x0f)) ^ mask
}
