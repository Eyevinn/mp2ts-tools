package internal

import "sort"

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

// selectLoop chooses the longest loop (or the longest within durCapMS) ending on
// an eligible loop point, and computes the audio fit and PCR/stuffing budget.
func selectLoop(vid *scanTrack, audios []*scanTrack, durCapMS, pcrPid int, bitrate int64, cbr bool) *LoopPlan {
	if vid == nil || len(vid.pics) == 0 {
		return nil
	}

	loopType, eligible := loopPointClass(vid.codec, vid.pics)

	type point struct {
		pts int64
		pkt uint32
		ps  bool
	}
	var elig []point
	for _, p := range vid.pics {
		if eligible(p) {
			elig = append(elig, point{p.PTS, p.StartPktNr, p.PSPresent})
		}
	}

	plan := &LoopPlan{LoopPointType: loopType}
	if len(elig) < 2 {
		plan.Warnings = append(plan.Warnings, "fewer than two eligible loop points; cannot loop")
		return plan
	}
	sort.Slice(elig, func(i, j int) bool { return elig[i].pts < elig[j].pts })

	start := elig[0]
	endIdx := len(elig) - 1
	if durCapMS > 0 {
		capTicks := int64(durCapMS) * 90
		endIdx = 0
		for i := 1; i < len(elig); i++ {
			if elig[i].pts-start.pts <= capTicks {
				endIdx = i
			} else {
				break
			}
		}
		if endIdx == 0 {
			plan.Warnings = append(plan.Warnings, "duration cap is shorter than one GOP; using one GOP")
			endIdx = 1
		}
	}
	end := elig[endIdx]

	plan.StartPTS, plan.EndPTS = start.pts, end.pts
	plan.StartPktNr, plan.EndPktNr = start.pkt, end.pkt
	plan.LoopDurTicks = end.pts - start.pts
	plan.LoopDurMS = float64(plan.LoopDurTicks) / 90.0
	plan.NumGOPs = endIdx // eligible points are consecutive GOP boundaries
	plan.PSAtStart = start.ps
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
