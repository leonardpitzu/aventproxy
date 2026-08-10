package rtsp

import (
	"encoding/binary"
	"strings"
	"sync"
)

// The monitor's LAN audio is 16 kHz signed 16-bit little-endian PCM, and RTP
// L16 is the same thing in network byte order. Carrying it as it comes beats
// companding it into G.711: go2rtc, which is what Home Assistant puts in front
// of this, packs PCM into FLAC for MSE and HLS and converts it to PCMA itself
// for WebRTC, so the lossy step bought nothing and cost half the bandwidth.
const (
	audioRate           = 16000
	audioPayloadType    = 97
	audioRtpmap         = "L16/16000"
	audioBytesPerSample = 2
)

// pcmLEtoBE swaps the monitor's little-endian samples into the network order
// RTP L16 is defined in.
func pcmLEtoBE(samples []byte) []byte {
	out := make([]byte, len(samples)&^1)
	for i := 0; i+1 < len(out); i += 2 {
		out[i] = samples[i+1]
		out[i+1] = samples[i]
	}
	return out
}

// codecIsALaw reports whether a WebRTC MIME type names A-law rather than u-law.
func codecIsALaw(mimeType string) bool {
	return strings.EqualFold(mimeType, "audio/PCMA")
}

// g711Decoder turns the cloud path's G.711 into the same L16 the local path
// produces, so one description stays true whichever transport a stream took.
//
// The cloud gives 8 kHz against the 16 kHz the description announces, so each
// sample is paired with one interpolated from its predecessor. That invents no
// detail; it stops the stream from playing at half speed.
type g711Decoder struct {
	mu   sync.Mutex
	alaw bool
	last int16
}

func (d *g711Decoder) decode(payload []byte) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]byte, 0, len(payload)*4)
	for _, b := range payload {
		sample := ulawToLinear(b)
		if d.alaw {
			sample = alawToLinear(b)
		}
		midpoint := int16((int32(d.last) + int32(sample)) / 2)
		out = binary.BigEndian.AppendUint16(out, uint16(midpoint))
		out = binary.BigEndian.AppendUint16(out, uint16(sample))
		d.last = sample
	}
	return out
}

// ulawToLinear is the ITU-T G.711 u-law expander.
func ulawToLinear(b byte) int16 {
	const bias = 0x84
	b = ^b
	t := (int32(b&0x0f) << 3) + bias
	t <<= uint((b & 0x70) >> 4)
	if b&0x80 != 0 {
		return int16(bias - t)
	}
	return int16(t - bias)
}

// alawToLinear is the ITU-T G.711 A-law expander. Its sign convention is the
// opposite of u-law's, which is the easy thing to get backwards.
func alawToLinear(b byte) int16 {
	b ^= 0x55
	t := int32(b&0x0f) << 4
	switch segment := (b & 0x70) >> 4; segment {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t += 0x108
		t <<= uint(segment - 1)
	}
	if b&0x80 != 0 {
		return int16(t)
	}
	return int16(-t)
}
