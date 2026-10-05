package internal

import "testing"

// simulateAudio runs the per-wrap drift controller the way LoopToSink does and
// returns the sequence of emitted audio frame PTS values. The last `moved`
// frames of the segment (the loop's tail and the spares) are sent at the start
// of the next wrap, as when audio lags video in the multiplex (rot -1); with
// moved < 0, the first -moved frames are sent at the end of the previous wrap,
// as when audio leads video (rot +1).
func simulateAudio(t *testing.T, frameDur, loopDur int64, wraps, moved int) []int64 {
	t.Helper()
	const start = int64(900000)
	// Segment frames: [start, start+loopDur) plus spare frames past the end.
	var seg []int64
	for p := start; SignedPTSDiff(p, start+loopDur+audioSpareFrames*frameDur) < 0; p += frameDur {
		seg = append(seg, p)
	}
	type sent struct {
		pts int64
		rot int
	}
	var order []sent // the segment's frames in the order they are sent
	switch {
	case moved > 0:
		for _, p := range seg[len(seg)-moved:] {
			order = append(order, sent{p, -1})
		}
		for _, p := range seg[:len(seg)-moved] {
			order = append(order, sent{p, 0})
		}
	case moved < 0:
		for _, p := range seg[-moved:] {
			order = append(order, sent{p, 0})
		}
		for _, p := range seg[:-moved] {
			order = append(order, sent{p, 1})
		}
	default:
		for _, p := range seg {
			order = append(order, sent{p, 0})
		}
	}
	st := newAudioWrapState(start, loopDur, seg)
	var out []int64
	for w := 0; w < wraps; w++ {
		for _, f := range order {
			L := w + f.rot
			if L < 0 || L >= wraps {
				continue
			}
			if d, keep := st.frame(L, f.pts); keep {
				if d < 0 || d >= frameDur {
					t.Fatalf("deltaPTS %d out of [0,%d)", d, frameDur)
				}
				out = append(out, f.pts+int64(L)*loopDur+d)
			}
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
		for _, moved := range []int{0, 10, -10} {
			out := simulateAudio(t, c.frameDur, c.loopDur, wraps, moved)
			if len(out) < wraps {
				t.Fatalf("%s moved %d: too few frames %d", c.name, moved, len(out))
			}
			// Continuity in send order: every step (including across every seam,
			// and between moved and unmoved frames) is exactly one frame.
			for i := 1; i < len(out); i++ {
				if out[i]-out[i-1] != c.frameDur {
					t.Fatalf("%s moved %d: audio discontinuity at frame %d: step %d want %d",
						c.name, moved, i, out[i]-out[i-1], c.frameDur)
				}
			}
			// No accumulated drift: the audio covers the wraps within one frame,
			// apart from the moved frames that the first or last wrap cannot send.
			lost := int64(moved)
			if lost < 0 {
				lost = -lost
			}
			totalAudio := out[len(out)-1] + c.frameDur - out[0]
			totalVideo := int64(wraps) * c.loopDur
			if d := totalVideo - totalAudio; d < -c.frameDur || d > (lost+1)*c.frameDur {
				t.Fatalf("%s moved %d: audio drifted over %d wraps: audio=%d video=%d diff=%d",
					c.name, moved, wraps, totalAudio, totalVideo, d)
			}
		}
	}
}
