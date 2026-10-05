package internal

import "sort"

// audioWrapState keeps an audio elementary stream drift-free across loop wraps.
//
// When the loop duration is not an exact integer number of audio frames, the
// audio cannot tile the loop exactly. Instead of letting a gap/overlap appear at
// every seam (or letting audio drift away from video), each wrap carries a small
// deltaPTS: the fractional overshoot of the frame that crosses the loop boundary.
// The audio is emitted shifted by deltaPTS so the frame cadence stays continuous
// across the seam, and deltaPTS stays bounded in [0, frameDur) so it never
// accumulates — the audio tracks the video loop with at most a sub-frame offset.
//
// Each wrap's deltaPTS follows from the previous one and the segment's audio
// frames alone, so the decision for a frame does not depend on the order the
// frames are sent in. That matters because audio PES are sent in the wrap next
// to the one they belong to when they are moved across the loop boundary.
type audioWrapState struct {
	startPTS int64   // PTS of the first audio frame of the loop
	loopDur  int64   // video loop duration in 90 kHz ticks
	offsets  []int64 // the segment's audio PES PTS relative to startPTS, sorted
	firstL   int     // logical wrap of deltas[0]
	deltas   []int64 // deltaPTS per logical wrap, from firstL
}

// newAudioWrapState sets up the controller for audio whose first loop frame is
// at startPTS and whose segment PES have the PTS values pts.
func newAudioWrapState(startPTS, loopDur int64, pts []int64) *audioWrapState {
	a := &audioWrapState{startPTS: startPTS, loopDur: loopDur, deltas: []int64{0}}
	for _, p := range pts {
		a.offsets = append(a.offsets, SignedPTSDiff(p, startPTS))
	}
	sort.Slice(a.offsets, func(i, j int) bool { return a.offsets[i] < a.offsets[j] })
	return a
}

// delta returns the deltaPTS of logical wrap L (0 for wrap 0): the overshoot of
// the first frame of wrap L-1 that no longer fits in its loop. Only the last few
// wraps are remembered, since frames are only ever sent a wrap early or late.
func (a *audioWrapState) delta(L int) int64 {
	for a.firstL+len(a.deltas) <= L {
		d := a.deltas[len(a.deltas)-1]
		next := d
		for _, o := range a.offsets {
			if o+d >= a.loopDur {
				next = o + d - a.loopDur
				break
			}
		}
		a.deltas = append(a.deltas, next)
		if len(a.deltas) > 4 {
			a.deltas = a.deltas[1:]
			a.firstL++
		}
	}
	return a.deltas[L-a.firstL]
}

// frame decides about the audio frame (PES) with original PTS p in logical wrap
// L. It returns the extra offset to add on top of the wrap's L*loopDur when the
// frame is kept, and keep=false when it does not fit in the wrap, because with
// this wrap's deltaPTS it starts at or after the loop end.
func (a *audioWrapState) frame(L int, p int64) (extraOffset int64, keep bool) {
	d := a.delta(L)
	if SignedPTSDiff(p, a.startPTS)+d >= a.loopDur {
		return 0, false
	}
	return d, true
}
