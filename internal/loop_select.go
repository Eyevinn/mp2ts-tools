package internal

import (
	"fmt"
	"sort"
)

// LoopAudio describes how one audio elementary stream fits the chosen loop.
type LoopAudio struct {
	PID           int    `json:"pid"`
	StartPTS      int64  `json:"startPTS"`
	StartPktNr    uint32 `json:"startPktNr"`
	FrameDurTicks int64  `json:"frameDurTicks"` // estimate from PES step; refined in M3
	WholeFrames   int64  `json:"wholeFrames"`   // audio frames nearest to the loop duration
	ResidualTicks int64  `json:"residualTicks"` // loopDur - wholeFrames*frameDur (per-loop shift)
}

// LoopPCR describes the rate/PCR situation at the loop boundary, including the
// null-stuffing adjustment needed to keep PCR seamless across the wrap.
type LoopPCR struct {
	PID            int   `json:"pid"`
	ConstantRate   bool  `json:"constantRate"`
	BitrateBps     int64 `json:"bitrateBps"`
	SegmentPackets int   `json:"segmentPackets"` // TS packets in [startPkt,endPkt)
	IdealPackets   int64 `json:"idealPackets"`   // packets the loop duration implies at the bitrate
	StuffingDelta  int64 `json:"stuffingDelta"`  // segment-ideal: null pkts to drop(+)/add(-) at the seam
}

// LoopPlan is the chosen loop: the video window plus how audio, rate and PCR
// must be reconciled across the (packet-aligned, not time-aligned) boundary.
type LoopPlan struct {
	LoopPointType string      `json:"loopPointType"`
	NumGOPs       int         `json:"numGops"`
	Frames        int         `json:"frames"` // coded pictures in the loop
	StartPktNr    uint32      `json:"startPktNr"`
	EndPktNr      uint32      `json:"endPktNr"` // exclusive: first packet of the end loop-point AU
	StartPTS      int64       `json:"startPTS"`
	EndPTS        int64       `json:"endPTS"`
	LoopDurTicks  int64       `json:"loopDurTicks"`
	LoopDurMS     float64     `json:"loopDurMs"`
	PSAtStart     bool        `json:"psAtStart"`
	Audio         []LoopAudio `json:"audio,omitempty"`
	PCR           *LoopPCR    `json:"pcr,omitempty"`
	Warnings      []string    `json:"warnings,omitempty"`
}

// Loop-point classes, from cleanest to least clean. MPEG-2 has no IDR or CRA
// pictures, so its closed- and open-GOP I pictures get their own names.
const (
	loopPointIDR     = "IDR"
	loopPointCRA     = "CRA"
	loopPointRAP     = "RAP"
	loopPointClosedI = "I-closed"
	loopPointOpenI   = "I-open"
	loopPointNone    = "none"
)

// loopPointClass picks the class of pictures to loop on: closed-GOP random access
// (IDR), then open-GOP (CRA), then any RAP. A loop needs two points of one class,
// so a class with fewer falls through to the next: encoders that close only the
// first GOP (MPEG-2 without closed GOPs, x265 open-gop) then loop on the open
// GOPs. If no class has two points, the first non-empty one names the result.
func loopPointClass(codec Codec, pics []PicRecord) (string, func(PicRecord) bool) {
	classes := []struct {
		label    string
		eligible func(PicRecord) bool
	}{
		{loopPointIDR, func(p PicRecord) bool { return p.IsIDR }},
		{loopPointCRA, func(p PicRecord) bool { return p.IsCRA }},
		{loopPointRAP, func(p PicRecord) bool { return p.IsRAP }},
	}
	if codec == CODEC_MPEG2V {
		classes[0].label, classes[1].label = loopPointClosedI, loopPointOpenI
	}
	first := -1
	for i, c := range classes {
		n := 0
		for _, p := range pics {
			if c.eligible(p) {
				n++
			}
		}
		if n >= 2 {
			return c.label, c.eligible
		}
		if n > 0 && first < 0 {
			first = i
		}
	}
	if first >= 0 {
		return classes[first].label, classes[first].eligible
	}
	return loopPointNone, func(PicRecord) bool { return false }
}

// selectLoop chooses the longest loop (or the longest within durCapMS) between two
// eligible loop points, and computes the audio fit and PCR/stuffing budget.
// fpsNum/fpsDen is an optional frame-rate hint (0 = detect from the DTS steps).
//
// A perfect loop is a whole number of nominal frame periods. At 59.94 fps a frame
// is 1501.5 ticks, so only an even frame count gives an integer duration; at
// 23.976 (3753.75 ticks) the count must be a multiple of four. Any other loop is
// up to half a tick off the nominal rate, and the source's 1501/1502 timestamp
// dither breaks at every seam. Such loops are used only if no exact one exists.
func selectLoop(vid *scanTrack, audios []*scanTrack, fpsNum, fpsDen, durCapMS, pcrPid int, bitrate int64, cbr bool) *LoopPlan {
	if vid == nil || len(vid.pics) == 0 {
		return nil
	}

	loopType, eligible := loopPointClass(vid.codec, vid.pics)

	type point struct {
		pts int64
		pkt uint32
		ps  bool
		idx int // index in vid.pics (decode order)
	}
	var elig []point
	for i, p := range vid.pics {
		if eligible(p) {
			elig = append(elig, point{p.PTS, p.StartPktNr, p.PSPresent, i})
		}
	}

	plan := &LoopPlan{LoopPointType: loopType}
	if len(elig) < 2 {
		plan.Warnings = append(plan.Warnings, "fewer than two eligible loop points; cannot loop")
		return plan
	}
	sort.Slice(elig, func(i, j int) bool { return elig[i].pts < elig[j].pts })

	num, den := fpsNum, fpsDen
	if num <= 0 || den <= 0 {
		num, den = 0, 0
		dts := make([]int64, len(vid.pics))
		for i, p := range vid.pics {
			dts[i] = p.DTS
		}
		if i := nominalFrameRate(statOf(CalculateSteps(dts)).Avg); i >= 0 {
			num, den = nominalFrameRates[i].num, nominalFrameRates[i].den
		}
	}
	// offNominal is how far (in ticks, times num) a loop of the given duration and
	// frame count is from a whole number of nominal frame periods.
	offNominal := func(dur int64, frames int) int64 {
		if num <= 0 {
			return 0 // unknown rate: no constraint
		}
		return dur*int64(num) - int64(frames)*90000*int64(den)
	}

	// Pick the longest loop within the cap, preferring an exact one. Ties go to
	// the earliest start.
	capTicks := int64(durCapMS) * 90
	si, ei, bestExact := -1, -1, false
	for i := 0; i < len(elig); i++ {
		for j := i + 1; j < len(elig); j++ {
			dur := elig[j].pts - elig[i].pts
			if durCapMS > 0 && dur > capTicks {
				break
			}
			ex := offNominal(dur, elig[j].idx-elig[i].idx) == 0
			if si < 0 || (ex && !bestExact) || (ex == bestExact && dur > elig[ei].pts-elig[si].pts) {
				si, ei, bestExact = i, j, ex
			}
		}
	}
	if si < 0 {
		plan.Warnings = append(plan.Warnings, "duration cap is shorter than one GOP; using one GOP")
		si, ei = 0, 1
		bestExact = offNominal(elig[1].pts-elig[0].pts, elig[1].idx-elig[0].idx) == 0
	}
	start, end := elig[si], elig[ei]

	plan.StartPTS, plan.EndPTS = start.pts, end.pts
	plan.StartPktNr, plan.EndPktNr = start.pkt, end.pkt
	plan.LoopDurTicks = end.pts - start.pts
	plan.LoopDurMS = float64(plan.LoopDurTicks) / 90.0
	plan.NumGOPs = ei - si // eligible points are consecutive GOP boundaries
	plan.Frames = end.idx - start.idx
	plan.PSAtStart = start.ps
	if !bestExact {
		off := float64(offNominal(plan.LoopDurTicks, plan.Frames)) / float64(num)
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"no loop is a whole number of %s frame periods; each wrap is %+.2f ticks off nominal",
			frLabel(num, den), off))
	}
	if !start.ps {
		plan.Warnings = append(plan.Warnings, "start loop point does not carry parameter sets; they must be inserted at the seam")
	}
	if loopType == loopPointOpenI {
		plan.Warnings = append(plan.Warnings, "open-GOP loop point: the B-pictures leading each seam predict from the previous wrap's tail (marked broken_link); expect a brief visual transient")
	}

	for _, a := range audios {
		la := LoopAudio{PID: a.pid, StartPTS: -1}
		for _, ap := range a.audio {
			if SignedPTSDiff(ap.PTS, start.pts) >= 0 {
				la.StartPTS, la.StartPktNr = ap.PTS, ap.StartPktNr
				break
			}
		}
		la.FrameDurTicks = a.audioFrameDur()
		if la.FrameDurTicks > 0 {
			la.WholeFrames = (plan.LoopDurTicks + la.FrameDurTicks/2) / la.FrameDurTicks
			la.ResidualTicks = plan.LoopDurTicks - la.WholeFrames*la.FrameDurTicks
		}
		plan.Audio = append(plan.Audio, la)
	}
	sort.Slice(plan.Audio, func(i, j int) bool { return plan.Audio[i].PID < plan.Audio[j].PID })

	if pcrPid >= 0 && bitrate > 0 {
		segPk := int(end.pkt - start.pkt)
		ideal := int64(float64(plan.LoopDurTicks)/90000.0*float64(bitrate)/8.0/float64(PacketSize) + 0.5)
		plan.PCR = &LoopPCR{
			PID:            pcrPid,
			ConstantRate:   cbr,
			BitrateBps:     bitrate,
			SegmentPackets: segPk,
			IdealPackets:   ideal,
			StuffingDelta:  int64(segPk) - ideal,
		}
	}
	return plan
}

// audioFrameDurEstimate estimates the audio frame duration (90 kHz ticks) from
// the smallest positive gap between consecutive audio PES PTS values. This is
// exact when audio is one frame per PES; it is refined by ADTS parsing in M3.
func audioFrameDurEstimate(a *scanTrack) int64 {
	if len(a.audio) < 2 {
		return 0
	}
	min := int64(1) << 62
	for i := 1; i < len(a.audio); i++ {
		d := SignedPTSDiff(a.audio[i].PTS, a.audio[i-1].PTS)
		if d > 0 && d < min {
			min = d
		}
	}
	if min == int64(1)<<62 {
		return 0
	}
	return min
}
