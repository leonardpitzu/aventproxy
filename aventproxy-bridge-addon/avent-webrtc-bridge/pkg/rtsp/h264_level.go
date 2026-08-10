package rtsp

// The monitor declares level 5.1 on a 1280x720 20 fps stream, which is not
// remotely what it sends: that picture fits exactly inside level 3.1. Safari
// sets its decoder up from the level WebRTC negotiated and refuses a stream
// claiming more, so the picture never arrives and Home Assistant falls back to
// HLS; Chrome ignores the claim and decodes anyway. Correcting the declaration
// costs nothing and changes not one coded bit.

// levelLimits is H.264 Annex A table A-1: the frame size and macroblock rate
// each level admits.
var levelLimits = []struct {
	level   byte
	maxFS   int // macroblocks per frame
	maxMBPS int // macroblocks per second
}{
	{10, 99, 1485},
	{11, 396, 3000},
	{12, 396, 6000},
	{13, 396, 11880},
	{20, 396, 11880},
	{21, 792, 19800},
	{22, 1620, 20250},
	{30, 1620, 40500},
	{31, 3600, 108000},
	{32, 5120, 216000},
	{40, 8192, 245760},
	{41, 8192, 245760},
	{42, 8704, 522240},
	{50, 22080, 589824},
	{51, 36864, 983040},
	{52, 36864, 2073600},
}

// minLevel is the lowest level that still admits this picture size and rate.
// Zero means the stream is outside the table and nothing should be rewritten.
func minLevel(width, height, fps int) byte {
	if width <= 0 || height <= 0 {
		return 0
	}
	if fps <= 0 {
		fps = 30
	}

	frame := ((width + 15) / 16) * ((height + 15) / 16)
	rate := frame * fps
	for _, limit := range levelLimits {
		if frame <= limit.maxFS && rate <= limit.maxMBPS {
			return limit.level
		}
	}
	return 0
}

// level_idc sits after the NAL header, profile_idc and the constraint flags.
const spsLevelOffset = 3

// correctSPSLevel lowers an SPS's declared level to what the stream needs. It
// never raises one: a level below the real requirement is the single change
// here that could break a decoder trusting it.
func correctSPSLevel(nal []byte, level byte) []byte {
	if level == 0 || len(nal) <= spsLevelOffset {
		return nal
	}
	if nal[0]&0x1f != 7 || nal[spsLevelOffset] <= level {
		return nal
	}

	corrected := make([]byte, len(nal))
	copy(corrected, nal)
	corrected[spsLevelOffset] = level
	return corrected
}
