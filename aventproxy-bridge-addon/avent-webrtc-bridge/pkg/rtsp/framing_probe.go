package rtsp

import (
	"fmt"
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
type framingProbe struct {
	seen      int
	continued int
	units     int
	lastTS    uint64
	opening   [32]int
	following [32]int
	reported  bool
}

// observe folds one frame into the measurement, reporting once it has enough.
func (p *framingProbe) observe(frame *lan.VideoFrame, camera string) {
	if p.reported || frame == nil || len(frame.NAL) == 0 {
		return
	}

	opensUnit := p.seen == 0 || frame.Timestamp != p.lastTS
	if opensUnit {
		p.units++
		p.lastTS = frame.Timestamp
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
	}
}

func (p *framingProbe) summary() string {
	return fmt.Sprintf(
		"%d messages, %d declare more than they carry, %d access units by timestamp; "+
			"opening a unit: %s; continuing one: %s",
		p.seen, p.continued, p.units, histogram(p.opening), histogram(p.following),
	)
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
