package rtsp

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"avent-webrtc-bridge/pkg/core"
	"avent-webrtc-bridge/pkg/lan"
)

// framingSample is how many video messages a probe watches before reporting.
// Ten seconds of a 20 fps stream is a few hundred, so this covers a couple of
// seconds: enough to see several pictures, short enough to stay out of the way.
const framingSample = 300

// framingProbe measures how the monitor frames its video, once per session.
//
// The bridge currently treats every message as a finished RTP payload. The
// first bytes say otherwise, and whether a picture spans several messages can
// only be answered against real hardware, so the answer is measured here and
// logged rather than assumed. Remove this once the framing is implemented.
// framingSamples is how many units' leading bytes are reported verbatim. The
// counts say what the payloads are not; only the bytes say what they are.
const framingSamples = 8

type framingProbe struct {
	seen       int
	continued  int
	byClock    int
	byPicture  int
	sliceStart int // fragments that begin a coded slice
	firstMB    int // and how many of those say first_mb_in_slice is zero
	units      accessUnits
	lastTS     uint64
	opening    [32]int
	following  [32]int
	fuHeaders  map[byte]int
	heads      []string
	reported   bool
}

// observe folds one frame into the measurement, reporting once it has enough.
func (p *framingProbe) observe(frame *lan.VideoFrame, camera string, client *lan.Client) {
	if p.reported || frame == nil || len(frame.NAL) == 0 {
		return
	}

	opensUnit := p.seen == 0 || frame.Timestamp != p.lastTS
	if opensUnit {
		p.byClock++
		p.lastTS = frame.Timestamp
	}
	if p.units.starts(frame.NAL) {
		p.byPicture++
	}

	// A picture is meant to begin at a slice whose first macroblock is zero.
	// When that never holds, the boundary is somewhere else entirely.
	if nalType, begins := nalTypeOf(frame.NAL); begins && isVCL(nalType) {
		p.sliceStart++
		if firstMBIsZero(frame.NAL) {
			p.firstMB++
		}
	}

	// The FU header says which NAL a fragment belongs to and whether it opens or
	// closes it. Without an end bit nothing marks the last packet of a picture.
	if frame.NAL[0]&0x1f == 28 && len(frame.NAL) > 1 {
		if p.fuHeaders == nil {
			p.fuHeaders = map[byte]int{}
		}
		p.fuHeaders[frame.NAL[1]]++
	}

	if len(p.heads) < framingSamples {
		p.heads = append(p.heads, fmt.Sprintf("%s(%d)", hex.EncodeToString(frame.NAL[:min(len(frame.NAL), 6)]), len(frame.NAL)))
	}

	nalType := frame.NAL[0] & 0x1f
	if opensUnit {
		p.opening[nalType]++
	} else {
		p.following[nalType]++
	}
	if frame.Declared > len(frame.NAL) {
		p.continued++
	}

	p.seen++
	if p.seen >= framingSample {
		p.reported = true
		core.Logger.Info().Msgf("LAN video framing for %s: %s", camera, p.summary())
		if client != nil {
			core.Logger.Info().Msgf("LAN media channel for %s: %s", camera, readLoopCounts(client.Snapshot()))
		}
	}
}

// readLoopCounts says how much of what arrived was used. A stream that is
// missing most of its pictures looks very different here from one whose
// pictures never arrived at all.
func readLoopCounts(s lan.Stats) string {
	var other []string
	for cmd, n := range s.Other {
		other = append(other, fmt.Sprintf("%#08x=%d", cmd, n))
	}
	slices.Sort(other)
	discarded := "none"
	if len(other) > 0 {
		discarded = strings.Join(other, " ")
	}
	return fmt.Sprintf(
		"%d messages read, %d undecryptable, %d malformed, %d video, %d audio; "+
			"discarded commands: %s; raw video: %s",
		s.Reads, s.OpenErrors, s.Malformed, s.Video, s.Audio, discarded,
		strings.Join(s.Raw, " "),
	)
}

func (p *framingProbe) summary() string {
	return fmt.Sprintf(
		"%d messages, %d declare more than they carry, %d access units by picture "+
			"(%d by the monitor's clock), %d slice starts of which %d at macroblock zero; "+
			"opening a unit: %s; continuing one: %s; FU headers: %s; first units: %s",
		p.seen, p.continued, p.byPicture, p.byClock, p.sliceStart, p.firstMB,
		histogram(p.opening), histogram(p.following), fuHeaders(p.fuHeaders),
		strings.Join(p.heads, " "),
	)
}

// fuHeaders renders the FU header counts, decoding the bits that matter so the
// line can be read without a calculator.
func fuHeaders(counts map[byte]int) string {
	if len(counts) == 0 {
		return "none"
	}
	headers := make([]byte, 0, len(counts))
	for h := range counts {
		headers = append(headers, h)
	}
	slices.SortFunc(headers, func(a, b byte) int { return counts[b] - counts[a] })

	var parts []string
	for i, h := range headers {
		if i == 6 {
			parts = append(parts, "...")
			break
		}
		flags := ""
		if h&0x80 != 0 {
			flags += "S"
		}
		if h&0x40 != 0 {
			flags += "E"
		}
		if flags == "" {
			flags = "-"
		}
		parts = append(parts, fmt.Sprintf("%02x[%s,nal%d]=%d", h, flags, h&0x1f, counts[h]))
	}
	return strings.Join(parts, " ")
}

// histogram renders the NAL-type counts, naming the types that would be valid
// so a glance says whether these are payloads or arbitrary bytes.
func histogram(counts [32]int) string {
	names := map[int]string{1: "slice", 5: "IDR", 6: "SEI", 7: "SPS", 8: "PPS", 24: "STAP-A", 28: "FU-A"}
	var parts []string
	var other int
	for t, n := range counts {
		if n == 0 {
			continue
		}
		if name, ok := names[t]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", name, n))
		} else {
			other += n
		}
	}
	if other > 0 {
		parts = append(parts, fmt.Sprintf("not-a-NAL-type=%d", other))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}
