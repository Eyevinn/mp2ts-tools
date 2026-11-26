package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// LoopOptions bound the loop mp2ts-loop chooses.
type LoopOptions struct {
	FrameRate     FrameRate // zero: detected from the video DTS
	MaxDurationMS int       // longest loop to consider; 0: no limit
}

// maxLoopDurationMS is a day: far beyond any loop, well within the 33-bit PTS.
const maxLoopDurationMS = 86_400_000

// VideoReport describes the video of the looped program.
type VideoReport struct {
	PID      int    `json:"pid"`
	Codec    string `json:"codec"`
	Pictures int    `json:"pictures"`
	// FrameRate is detected from how long pictures are shown: a field picture
	// is half a frame, a frame that repeats a field for pulldown one and a half.
	FrameRate string `json:"frameRate"`
	// FieldPictures are pictures that hold one field, the other being the next.
	FieldPictures int `json:"fieldPictures,omitempty"`
	// FieldOrder of interlaced frames and field pairs: top-first, bottom-first
	// or mixed; empty when progressive or not signalled.
	FieldOrder string `json:"fieldOrder,omitempty"`
	// PulldownPictures are frames shown for more than one frame period.
	PulldownPictures int            `json:"pulldownPictures,omitempty"`
	AccessPoints     map[string]int `json:"accessPoints"`
}

// AudioReport describes one audio stream of the looped program.
type AudioReport struct {
	PID        int    `json:"pid"`
	Codec      string `json:"codec"`
	SampleRate int    `json:"sampleRate"`
	PES        int    `json:"pes"`
	Frames     int    `json:"frames"`
	CutAt      string `json:"cutAt"` // frame or pes
}

// DataReport describes a data PES stream of the looped program, such as SMPTE
// ST 2038 ANC, teletext or subtitles.
type DataReport struct {
	PID        int    `json:"pid"`
	StreamType uint8  `json:"streamType"`
	Label      string `json:"label"`
	PES        int    `json:"pes"`
	// Timing is "by PTS" when each lap can take its PES by their PTS, or what
	// keeps it where the source sends it.
	Timing string `json:"timing"`
}

// LoopAudioReport describes the audio a lap plays of one stream.
type LoopAudioReport struct {
	PID      int     `json:"pid"`
	PES      int     `json:"pes"`
	Units    int     `json:"units"`    // frames, or PES when cut at PES
	ExcessMS float64 `json:"excessMs"` // beyond the loop's end; negative: a gap at the end of every lap
	// HeadGapMS is how much of the loop's start the file holds no audio for: a
	// gap at the start of every lap.
	HeadGapMS    float64 `json:"headGapMs"`
	SeamOffsetMS float64 `json:"seamOffsetMs"`
}

// LoopDataReport describes the data PES a lap plays of one stream.
type LoopDataReport struct {
	PID int `json:"pid"`
	// PlacedByLap: each lap takes the PES of its own pictures; false: they stay
	// where the source sends them.
	PlacedByLap bool `json:"placedByLap"`
	PES         int  `json:"pes"`
	// MissingAtStart and MissingAtEnd are PES of the loop's first and last
	// pictures that the file lacks.
	MissingAtStart int `json:"missingAtStart"`
	MissingAtEnd   int `json:"missingAtEnd"`
}

// LoopReport describes the chosen loop.
type LoopReport struct {
	AccessPoint string `json:"accessPoint"`
	StartPacket int    `json:"startPacket"`
	EndPacket   int    `json:"endPacket"`
	StartPTS    int64  `json:"startPts"`
	// FirstPTS is the earliest PTS of the loop's pictures, where the interval
	// that every stream loops starts.
	FirstPTS      int64   `json:"firstPts"`
	EndPTS        int64   `json:"endPts"`
	DurationTicks int64   `json:"durationTicks"`
	DurationMS    float64 `json:"durationMs"`
	// Frames is how many frame periods a lap lasts, Pictures how many coded
	// pictures it has: twice as many for field pictures, fewer for pulldown.
	Frames          float64           `json:"frames"`
	Pictures        int               `json:"pictures"`
	GOPs            int               `json:"gops"`
	FrameRate       string            `json:"frameRate"`
	WholeFrames     bool              `json:"wholeFrames"`
	ConstantRate    bool              `json:"constantRate"`
	Bitrate         int64             `json:"bitrate"`
	RateDeviationMS float64           `json:"rateDeviationMs"`
	WindowMS        float64           `json:"windowMs"`
	PacketsPerLap   int               `json:"packetsPerLap"`
	Audio           []LoopAudioReport `json:"audio"`
	Data            []LoopDataReport  `json:"data"`
	SCTE35          []*LoopSCTE35     `json:"scte35,omitempty"`
}

// LapReport describes how a lap is made from the loop's window.
type LapReport struct {
	Packets               int `json:"packets"`
	StrippedPackets       int `json:"strippedPackets"`
	RemovedNulls          int `json:"removedNulls"`
	AddedNulls            int `json:"addedNulls"`
	AddedPCRPackets       int `json:"addedPcrPackets"`
	SeamMarks             int `json:"seamMarks"`
	AudioHeadCuts         int `json:"audioHeadCuts"`
	SCTE35InjectedPackets int `json:"scte35InjectedPackets,omitempty"`
	PreamblePackets       int `json:"preamblePackets"`
}

// ScanReport is what mp2ts-loop found and chose, as `mp2ts-loop -scan` prints it.
type ScanReport struct {
	Input            string              `json:"input"`
	Packets          int                 `json:"packets"`
	Program          int                 `json:"program"`
	PMTPID           int                 `json:"pmtPid"`
	PCRPID           int                 `json:"pcrPid"`
	Video            *VideoReport        `json:"video,omitempty"`
	Audio            []AudioReport       `json:"audio"`
	Data             []DataReport        `json:"data"`
	PassThrough      []PassThroughStream `json:"passThrough,omitempty"`
	SCTE35PIDs       []int               `json:"scte35Pids,omitempty"`
	ContinuityErrors map[int]int         `json:"continuityErrors"`
	Loop             *LoopReport         `json:"loop,omitempty"`
	Lap              *LapReport          `json:"lap,omitempty"`
	Warnings         []string            `json:"warnings"`
	Error            string              `json:"error,omitempty"`
}

// Loop is a TS prepared for looping: its chosen loop and the lap built from it.
type Loop struct {
	Report *ScanReport
	chosen *loopPlan
	seg    *loopSegment
}

// LoopDurationTicks returns the loop's duration in 90 kHz ticks.
func (l *Loop) LoopDurationTicks() int64 { return l.seg.duration }

// LapPackets returns the packets of one lap at the source rate.
func (l *Loop) LapPackets() int { return l.seg.lapPackets }

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func describeIndex(input string, idx *streamIndex) *ScanReport {
	rep := &ScanReport{
		Input:            input,
		Packets:          idx.packetCount,
		Program:          idx.programNumber,
		PMTPID:           idx.pmtPID,
		PCRPID:           idx.pcrPID,
		Audio:            []AudioReport{},
		Data:             []DataReport{},
		PassThrough:      idx.passThrough,
		SCTE35PIDs:       idx.scte35PIDs,
		ContinuityErrors: make(map[int]int),
		Warnings:         append([]string{}, idx.warnings...),
	}
	aps := make(map[string]int)
	for _, p := range idx.pictures {
		if p.info.accessPoint != apNone {
			aps[p.info.accessPoint.String()]++
		}
	}
	rate, _ := detectFrameRate(idx.pictures)
	rep.Video = &VideoReport{PID: idx.videoPID, Codec: idx.videoCodec.String(), Pictures: len(idx.pictures),
		FrameRate: rate.String(), AccessPoints: aps}
	var topFirst, bottomFirst int
	for i, p := range idx.pictures {
		if p.info.isField() {
			rep.Video.FieldPictures++
		}
		if p.info.fieldPeriods() > 2 {
			rep.Video.PulldownPictures++
		}
		// Frames, and fields that have their second.
		if !p.info.isField() || (i+1 < len(idx.pictures) && idx.pictures[i+1].info.secondField) {
			switch p.info.cadence().first {
			case FieldTop:
				topFirst++
			case FieldBottom:
				bottomFirst++
			}
		}
	}
	switch {
	case topFirst > 0 && bottomFirst > 0:
		rep.Video.FieldOrder = "mixed"
	case topFirst > 0:
		rep.Video.FieldOrder = "top-first"
	case bottomFirst > 0:
		rep.Video.FieldOrder = "bottom-first"
	}
	for _, a := range idx.audio {
		ar := AudioReport{PID: a.pid, Codec: a.codec.String(), SampleRate: a.sampleRate, PES: len(a.pes), CutAt: "pes"}
		if a.frameLevel {
			ar.Frames, ar.CutAt = len(a.frames), "frame"
		}
		rep.Audio = append(rep.Audio, ar)
	}
	for _, d := range idx.data {
		rep.Data = append(rep.Data, DataReport{PID: d.pid, StreamType: d.streamType, Label: d.label, PES: len(d.pes),
			Timing: d.timing.String()})
	}
	for _, e := range idx.continuityErrors {
		rep.ContinuityErrors[e.pid]++
	}
	return rep
}

func describeLoop(rep *ScanReport, idx *streamIndex, plan *loopPlan, seg *loopSegment) {
	lr := &LoopReport{
		AccessPoint:     plan.accessPoint.String(),
		StartPacket:     plan.startPacket,
		EndPacket:       plan.endPacket,
		StartPTS:        int64(wrapAdd(0, idx.pictures[plan.startPicture].pts, ptsWrapU)),
		FirstPTS:        int64(wrapAdd(0, plan.firstPTS, ptsWrapU)),
		EndPTS:          int64(wrapAdd(0, idx.pictures[plan.endPicture].pts, ptsWrapU)),
		DurationTicks:   plan.duration,
		DurationMS:      round3(float64(plan.duration) / 90),
		Frames:          float64(plan.fieldPeriods) / 2,
		Pictures:        plan.frames,
		GOPs:            plan.gops,
		FrameRate:       plan.frameRate.String(),
		WholeFrames:     plan.exact,
		ConstantRate:    plan.constantRate,
		Bitrate:         int64(math.Round(plan.bitrate)),
		RateDeviationMS: round3(float64(plan.deviation) / 27_000),
		WindowMS:        round3(float64(plan.windowTime) / 27_000),
		PacketsPerLap:   plan.outputPackets,
		Audio:           []LoopAudioReport{},
		Data:            []LoopDataReport{},
		SCTE35:          plan.scte35,
	}
	for _, f := range plan.audio {
		a := idx.audio[f.stream]
		lr.Audio = append(lr.Audio, LoopAudioReport{PID: a.pid, PES: f.pesCount, Units: f.frameCount,
			ExcessMS:     round3(float64(f.excess) / float64(a.timeScale) / 90),
			HeadGapMS:    round3(float64(f.headGap) / float64(a.timeScale) / 90),
			SeamOffsetMS: round3(float64(f.seamOffset) / 90)})
	}
	for _, f := range plan.data {
		lr.Data = append(lr.Data, LoopDataReport{PID: idx.data[f.stream].pid, PlacedByLap: f.placedByLap(),
			PES: len(f.members), MissingAtStart: f.missingAtStart, MissingAtEnd: f.missingAtEnd})
	}
	rep.Loop = lr
	rep.Warnings = append(rep.Warnings, plan.warnings...)
	rep.Lap = &LapReport{
		Packets:               seg.packetCount(),
		StrippedPackets:       seg.stripped,
		RemovedNulls:          seg.removedNulls,
		AddedNulls:            seg.addedNulls,
		AddedPCRPackets:       seg.addedPCRPackets,
		SeamMarks:             seg.seamMarks,
		AudioHeadCuts:         seg.audioHeadCuts,
		SCTE35InjectedPackets: seg.scte35Injected,
		PreamblePackets:       len(seg.preamble) / PacketSize,
	}
	rep.Warnings = append(rep.Warnings, seg.warnings...)
}

// planLoopSCTE35 decides per SCTE-35 PID which cues of the chosen loop are kept.
func planLoopSCTE35(idx *streamIndex, plan *loopPlan) {
	vs := int64(wrapAdd(0, idx.pictures[plan.startPicture].pts, ptsWrapU))
	for _, pid := range idx.scte35PIDs {
		ls := planSCTE35(pid, idx.scte35Sections[pid], plan.startPacket, plan.endPacket, vs, plan.duration)
		plan.scte35 = append(plan.scte35, ls)
		if ls.droppedEvents() > 0 {
			plan.warnings = append(plan.warnings, scte35Warning(ls))
		}
	}
}

// PrepareLoop reads a TS ("-" for stdin), scans it, chooses the loop and builds
// its lap. When no loop can be made, it returns the report so far along with
// the error; the report is nil when the input cannot be read as a TS at all.
func PrepareLoop(ctx context.Context, path string, opts LoopOptions) (*Loop, error) {
	if opts.MaxDurationMS < 0 || opts.MaxDurationMS > maxLoopDurationMS {
		return nil, fmt.Errorf("the longest loop must be at most a day, not %d ms", opts.MaxDurationMS)
	}
	data, release, err := readInput(path)
	if err != nil {
		return nil, err
	}
	defer release()
	return prepareLoop(ctx, path, data, opts)
}

// prepareLoop prepares the loop of the TS data, read from input.
func prepareLoop(ctx context.Context, input string, data []byte, opts LoopOptions) (*Loop, error) {
	idx, err := scanStream(ctx, data)
	if err != nil {
		return nil, err
	}
	rep := describeIndex(input, idx)
	l := &Loop{Report: rep}
	plan, err := chooseLoop(idx, loopOptions{frameRate: opts.FrameRate, maxDuration: int64(opts.MaxDurationMS) * 90})
	if err != nil {
		rep.Error = err.Error()
		return l, err
	}
	planLoopSCTE35(idx, plan)
	seg, err := buildSegment(data, idx, plan)
	if err != nil {
		rep.Error = err.Error()
		return l, err
	}
	describeLoop(rep, idx, plan, seg)
	l.chosen, l.seg = plan, seg
	return l, nil
}

// readInput returns the whole input, mapped when it is a file, and a function
// that releases it.
func readInput(path string) ([]byte, func(), error) {
	var data []byte
	release := func() {}
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, release, err = mapFile(path)
	}
	if err != nil {
		return nil, nil, err
	}
	if len(data) == 0 {
		release()
		return nil, nil, fmt.Errorf("%q is empty", path)
	}
	return data, release, nil
}

// LapOutput takes the packets of the laps as they are written.
type LapOutput interface {
	// Write takes packets, 188 bytes each; due(i) is when packet i is due, in
	// 27 MHz ticks from the start of the output.
	Write(ctx context.Context, packets []byte, due func(i int) int64) error
	Flush() error
}

// WriterOutput writes the laps as fast as they come.
type WriterOutput struct{ W io.Writer }

func (o WriterOutput) Write(_ context.Context, packets []byte, _ func(int) int64) error {
	_, err := o.W.Write(packets)
	return err
}

func (o WriterOutput) Flush() error { return nil }

// WriteLaps writes the preamble (PAT and PMT, then audio and data the first
// lap's video is sent with), laps laps (0: until ctx ends), and for a bounded
// run the audio and data of the last lap that the next lap would carry, to every
// output in turn.
func (l *Loop) WriteLaps(ctx context.Context, outputs []LapOutput, laps int) (LapStatistics, error) {
	if l.seg == nil {
		return LapStatistics{}, errors.New("no loop to write")
	}
	w := newLapWriter(l.seg, laps)
	emit := func(packets []byte) error {
		for _, o := range outputs {
			if err := o.Write(ctx, packets, w.sendTime); err != nil {
				return err
			}
		}
		return nil
	}
	err := emit(w.writePreamble())
	for lap := 0; err == nil && (laps <= 0 || lap < laps) && ctx.Err() == nil; lap++ {
		err = emit(w.writeLap())
	}
	if err == nil && w.lap > 0 && ctx.Err() == nil {
		err = emit(w.writeEnding())
	}
	for _, o := range outputs {
		if ferr := o.Flush(); err == nil {
			err = ferr
		}
	}
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return w.stats, err
}
