package internal

import "testing"

// simulateAudio runs the per-wrap drift controller the way LoopToSink does and
// returns the global sequence of emitted audio frame PTS values.
func simulateAudio(t *testing.T, frameDur, loopDur int64, wraps int) []int64 {
	t.Helper()
	const start = int64(900000)
	st := newAudioWrapState(start, loopDur)
	// Segment frames: [start, start+loopDur) plus two spare frames past the end.
	var seg []int64
	for p := start; SignedPTSDiff(p, start+loopDur+2*frameDur) < 0; p += frameDur {
		seg = append(seg, p)
	}
	var out []int64
	for w := 0; w < wraps; w++ {
		st.onWrap()
		offset := int64(w) * loopDur
		for _, p := range seg {
			d, keep := st.frame(p)
			if !keep {
				break
			}
			out = append(out, p+offset+d)
		}
		if st.deltaPTS < 0 || st.deltaPTS >= frameDur {
			t.Fatalf("deltaPTS %d out of [0,%d)", st.deltaPTS, frameDur)
		}
	}
	return out
}

func TestAudioDriftContinuousNoDrift(t *testing.T) {
	cases := []struct {
		name              string
		frameDur, loopDur int64
	}{
		{"zero-residual", 1920, 1920 * 60},   // 115200, exact multiple
		{"aac-residual", 1920, 100000},       // not a multiple of 1920
		{"mp2-48k-residual", 2160, 100000},   // not a multiple of 2160
		{"tiny-residual", 2160, 2160*50 + 1}, // off by one tick
		{"near-full-residual", 2160, 2160*50 - 1},
		{"ac3-3frame-pes", 8640, 2610000}, // AC-3 3 frames/PES (step 8640), 720-tick residual
	}
	const wraps = 200
	for _, c := range cases {
		out := simulateAudio(t, c.frameDur, c.loopDur, wraps)
		if len(out) < wraps {
			t.Fatalf("%s: too few frames %d", c.name, len(out))
		}
		// Continuity: every step (including across every seam) is exactly one frame.
		for i := 1; i < len(out); i++ {
			if out[i]-out[i-1] != c.frameDur {
				t.Fatalf("%s: audio discontinuity at frame %d: step %d want %d",
					c.name, i, out[i]-out[i-1], c.frameDur)
			}
		}
		// No accumulated drift: total audio tracks total video within one frame.
		totalAudio := out[len(out)-1] + c.frameDur - out[0]
		totalVideo := int64(wraps) * c.loopDur
		if d := totalAudio - totalVideo; d < -c.frameDur || d > c.frameDur {
			t.Fatalf("%s: audio drifted over %d wraps: audio=%d video=%d diff=%d",
				c.name, wraps, totalAudio, totalVideo, d)
		}
	}
}
