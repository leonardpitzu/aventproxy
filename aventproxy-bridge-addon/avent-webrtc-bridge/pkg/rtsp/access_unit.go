package rtsp

// Access-unit detection, as H.264 7.4.1.2 and RFC 6184 define it rather than as
// the monitor's clock suggests: its timestamp counts sends, not pictures, so
// 300 messages carry 300 distinct values and cannot be grouped by it.

// nalTypeOf returns the NAL type a payload carries and whether the payload
// begins that NAL. A fragment that continues one begins nothing.
func nalTypeOf(payload []byte) (nalType byte, begins bool) {
	if len(payload) < 1 {
		return 0, false
	}
	switch t := payload[0] & 0x1f; t {
	case 28, 29: // FU-A, FU-B: the real type and the start bit are in the FU header
		if len(payload) < 2 {
			return 0, false
		}
		return payload[1] & 0x1f, payload[1]&0x80 != 0
	case 24: // STAP-A: the first aggregated NAL decides
		if len(payload) < 4 {
			return 0, false
		}
		return payload[3] & 0x1f, true
	default:
		return t, true
	}
}

// isVCL reports whether a NAL type carries coded picture data.
func isVCL(nalType byte) bool {
	return nalType >= 1 && nalType <= 5
}

// firstMBIsZero reports whether a slice starts at macroblock zero, which is
// what makes it the first slice of a picture. first_mb_in_slice is the leading
// ue(v) of the slice header, and ue(v) zero is a single set bit.
func firstMBIsZero(payload []byte) bool {
	switch payload[0] & 0x1f {
	case 28, 29:
		// FU indicator, FU header, then the slice header.
		return len(payload) > 2 && payload[2]&0x80 != 0
	default:
		return len(payload) > 1 && payload[1]&0x80 != 0
	}
}

// accessUnits decides where one picture ends and the next begins.
type accessUnits struct {
	open      bool // an access unit has been opened
	inPicture bool // and a coded picture has begun inside it
}

// starts reports whether this payload opens a new access unit.
func (a *accessUnits) starts(payload []byte) bool {
	nalType, begins := nalTypeOf(payload)
	if !begins {
		return false
	}

	if isVCL(nalType) {
		if a.inPicture {
			// A slice that does not start at macroblock zero continues the
			// picture; one that does is the next picture.
			if !firstMBIsZero(payload) {
				return false
			}
			a.open = true
			return true
		}
		a.inPicture = true
		if a.open {
			// Parameter sets already opened this unit for us.
			return false
		}
		a.open = true
		return true
	}

	// Parameter sets and SEI introduce the picture that follows them, so the
	// first one after a picture opens the next unit and the rest join it.
	if a.inPicture {
		a.open, a.inPicture = true, false
		return true
	}
	if a.open {
		return false
	}
	a.open = true
	return true
}
