package internal

// audioWrapState keeps an audio elementary stream drift-free across loop wraps.
//
// When the loop duration is not an exact integer number of audio frames, the
// audio cannot tile the loop exactly. Instead of letting a gap/overlap appear at
// every seam (or letting audio drift away from video), each wrap carries a small
// deltaPTS: the fractional overshoot of the frame that crosses the loop boundary.
// The audio is emitted shifted by deltaPTS so the frame cadence stays continuous
// across the seam, and deltaPTS stays bounded in [0, frameDur) so it never
// accumulates — the audio tracks the video loop with at most a sub-frame offset.
type audioWrapState struct {
	startPTS int64 // PTS of the first audio frame of the loop
	loopDur  int64 // video loop duration in 90 kHz ticks
	deltaPTS int64 // carried sub-frame offset for the current wrap
	ended    bool  // audio budget for this wrap is exhausted
}

func newAudioWrapState(startPTS, loopDur int64) *audioWrapState {
	return &audioWrapState{startPTS: startPTS, loopDur: loopDur}
}

// onWrap starts a new wrap: the ended flag clears but deltaPTS carries over.
func (a *audioWrapState) onWrap() { a.ended = false }

// frame decides what to do with the audio frame whose original PTS is p. It
// returns the extra offset to add (on top of the wrap's ptsOffset) when the
// frame is kept, and keep=false when the frame belongs to the next wrap (and
// must be dropped). When a frame is dropped, its overshoot becomes the deltaPTS
// for the next wrap.
func (a *audioWrapState) frame(p int64) (extraOffset int64, keep bool) {
	if a.ended {
		return 0, false
	}
	sinceStart := SignedPTSDiff(p, a.startPTS)
	endDelta := SignedPTSDiff(sinceStart+a.deltaPTS, a.loopDur)
	if endDelta >= 0 {
		a.deltaPTS = endDelta
		a.ended = true
		return 0, false
	}
	return a.deltaPTS, true
}
