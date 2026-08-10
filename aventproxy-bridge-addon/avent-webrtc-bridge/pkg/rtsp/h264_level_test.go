package rtsp

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestMinLevelForThisMonitor(t *testing.T) {
	// 1280x720 is 3600 macroblocks, which is exactly level 3.1's ceiling, and
	// 20 fps is well inside its rate. The monitor claims 5.1.
	if got := minLevel(1280, 720, 20); got != 31 {
		t.Fatalf("level for 720p20 = %d, want 31", got)
	}
}

func TestMinLevelGrowsWithThePicture(t *testing.T) {
	for _, tc := range []struct {
		w, h, fps int
		want      byte
	}{
		{640, 480, 15, 22},
		{1280, 720, 20, 31},
		{1280, 720, 60, 32},
		{1920, 1080, 30, 40},
		{3840, 2160, 30, 51},
		{0, 720, 20, 0}, // nothing to go on, so nothing is rewritten
	} {
		if got := minLevel(tc.w, tc.h, tc.fps); got != tc.want {
			t.Errorf("minLevel(%d, %d, %d) = %d, want %d", tc.w, tc.h, tc.fps, got, tc.want)
		}
	}
}

// The SPS as the monitor sends it: NAL header, Main profile, no constraints,
// level 5.1.
func TestCorrectsTheMonitorsOverstatedLevel(t *testing.T) {
	sps, err := hex.DecodeString("274d0033e740")
	if err != nil {
		t.Fatal(err)
	}

	got := correctSPSLevel(sps, 31)
	want, _ := hex.DecodeString("274d001fe740")
	if !bytes.Equal(got, want) {
		t.Fatalf("corrected SPS = % x, want % x", got, want)
	}
	if sps[spsLevelOffset] != 0x33 {
		t.Fatal("the original was modified in place; callers may still hold it")
	}
}

func TestNeverRaisesALevel(t *testing.T) {
	sps, _ := hex.DecodeString("274d001ee740") // already level 3.0
	if got := correctSPSLevel(sps, 41); !bytes.Equal(got, sps) {
		t.Fatalf("level was raised to % x; a level below the real requirement is the dangerous direction", got[spsLevelOffset])
	}
}

func TestLeavesEverythingElseAlone(t *testing.T) {
	idr, _ := hex.DecodeString("6588840f11")
	if got := correctSPSLevel(idr, 31); !bytes.Equal(got, idr) {
		t.Fatal("a coded slice was rewritten")
	}
	if got := correctSPSLevel([]byte{0x27}, 31); len(got) != 1 {
		t.Fatal("a truncated SPS was rewritten")
	}
	if got := correctSPSLevel(idr, 0); !bytes.Equal(got, idr) {
		t.Fatal("rewrote despite having no level to apply")
	}
}
