package internal

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
)

// FieldParity describes whether a coded picture is a full frame or a single
// field (for interlaced / field-coded streams such as field-per-PES HEVC).
type FieldParity int

const (
	FieldUnknown FieldParity = iota
	FieldFrame
	FieldTop
	FieldBottom
)

func (f FieldParity) String() string {
	switch f {
	case FieldFrame:
		return "frame"
	case FieldTop:
		return "top"
	case FieldBottom:
		return "bottom"
	default:
		return "unknown"
	}
}

// PicRecord is the per-coded-picture (or per-field) summary produced by the scan
// pass. It carries everything the loop-point selection needs, including hooks
// for interlace (Field) and GDR (HasRecovery) that are populated as those
// features are implemented.
type PicRecord struct {
	PTS         int64
	DTS         int64
	StartPktNr  uint32
	RAI         bool
	IsIDR       bool
	IsCRA       bool
	IsRAP       bool
	IsRASL      bool // HEVC leading picture; must be dropped at a CRA loop seam
	HasRecovery bool // recovery_point SEI (GDR) — detection TBD
	PSPresent   bool // VPS/SPS/PPS carried in this access unit
	Field       FieldParity
}

// AudioPESRec is the per-audio-PES summary produced by the scan pass.
type AudioPESRec struct {
	PTS        int64
	DTS        int64
	StartPktNr uint32
	PayloadLen int
}

// scanTrack collects per-PES scan records for one elementary stream.
type scanTrack struct {
	pid        int
	codec      Codec
	mediaType  string
	pics       []PicRecord
	audio      []AudioPESRec
	framer     AudioFramer // audio only, codec-specific (AAC for now)
	frames     int         // total audio frames seen
	nonAligned bool        // any audio frame straddles a PES boundary
	splitErrs  int         // audio PES that failed to split
	noPTS      int         // PES without a PTS; such streams are not looped

	// MPEG-2 only: an open-GOP I picture (pics[openIdx]) waiting for the next
	// coded picture to show whether it has leading B-pictures.
	openPending   bool
	openIdx       int
	openSkipField bool // the next PES is the I picture's second field
}

// HandlePES implements PESHandler for the scan pass. It copies the fields it
// needs out of the (reused) PESData; it must not retain p.Data.
func (s *scanTrack) HandlePES(p *PESData, last bool) error {
	if last {
		return nil // the final buffered PES may be truncated at EOF
	}
	if !p.HasPTS {
		s.noPTS++ // counted and skipped: requirePTS refuses to loop the stream
		return nil
	}
	switch s.mediaType {
	case "video":
		au := ScanAU(s.codec, p.Data)
		field := FieldFrame // TODO: detect field parity for interlaced AVC/HEVC
		if s.codec == CODEC_MPEG2V {
			s.resolveMPEG2Leading(au)
			field = au.Field
		}
		s.pics = append(s.pics, PicRecord{
			PTS:        p.PTS,
			DTS:        p.DTS,
			StartPktNr: p.StartPktNr,
			RAI:        p.RAI,
			IsIDR:      au.IsIDR,
			IsCRA:      au.IsCRA,
			IsRAP:      au.IsRAP,
			IsRASL:     au.IsRASL,
			PSPresent:  au.PSComplete(s.codec),
			Field:      field,
		})
		if s.codec == CODEC_MPEG2V && au.IsCRA {
			s.openPending, s.openIdx = true, len(s.pics)-1
			s.openSkipField = au.Field == FieldTop || au.Field == FieldBottom
		}
	case "audio":
		s.audio = append(s.audio, AudioPESRec{
			PTS:        p.PTS,
			DTS:        p.DTS,
			StartPktNr: p.StartPktNr,
			PayloadLen: p.PayloadLength,
		})
		// Frame-accurate audio parsing. Setting p.AlignOffset lets ElStream carry
		// a straddling frame into the next PES.
		if s.framer == nil {
			s.framer = newAudioFramer(s.codec)
		}
		if s.framer != nil {
			frames, missing, err := s.framer.Split(p.Data, p.PayloadLength, p.AlignOffset, p.PTS)
			if err != nil {
				s.splitErrs++
				p.AlignOffset = 0
			} else {
				s.frames += len(frames)
				if missing > 0 {
					s.nonAligned = true
				}
				p.AlignOffset = missing
			}
		}
	}
	return nil
}

// resolveMPEG2Leading settles a pending open-GOP I picture once the next coded
// picture au is known. closed_gop=0 only matters if B-pictures follow the I
// picture in coding order: they display before it and predict from the previous
// GOP, which at a loop seam is the previous wrap's tail. If the next picture is
// not a B-picture there are none, and the I picture is a clean loop point.
func (s *scanTrack) resolveMPEG2Leading(au AUInfo) {
	if !s.openPending || !au.IsVCL {
		return
	}
	if s.openSkipField {
		s.openSkipField = false
		return
	}
	s.openPending = false
	if au.PicType != mpeg2PicB {
		p := &s.pics[s.openIdx]
		p.IsIDR, p.IsCRA = true, false
	}
}

// audioFrameDur returns the audio frame duration in 90 kHz ticks, preferring the
// exact value from the framer and falling back to the PES-step estimate.
func (s *scanTrack) audioFrameDur() int64 {
	if s.framer != nil && s.framer.FrameDurTicks() > 0 {
		return s.framer.FrameDurTicks()
	}
	return audioFrameDurEstimate(s)
}

// Stat is a min/max/average summary of a set of integer steps.
type Stat struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
	Avg int64 `json:"avg"`
}

// VideoScan summarizes the video elementary stream and its loop points.
type VideoScan struct {
	PID               int    `json:"pid"`
	Codec             string `json:"codec"`
	Pictures          int    `json:"pictures"`
	FrameRate         string `json:"frameRate"`
	ConstantFrameRate bool   `json:"constantFrameRate"`
	DTSStepTicks      Stat   `json:"dtsStepTicks"`
	Field             string `json:"field"`
	PESWithoutPTS     int    `json:"pesWithoutPTS,omitempty"`
	LoopPointType     string `json:"loopPointType"` // IDR | CRA | RAP | I-closed | I-open | none
	LoopPoints        int    `json:"loopPoints"`
	GOPDurationTicks  Stat   `json:"gopDurationTicks"`
	ConstantGOP       bool   `json:"constantGop"`
	FirstPTS          int64  `json:"firstPTS"`
	LastPTS           int64  `json:"lastPTS"`
	DurationTicks     int64  `json:"durationTicks"`
	MaxLoopTicks      int64  `json:"maxLoopTicks"`
	MaxLoopGOPs       int64  `json:"maxLoopGops"`
}

// AudioScan summarizes one audio elementary stream.
type AudioScan struct {
	PID           int    `json:"pid"`
	Codec         string `json:"codec"`
	PESCount      int    `json:"pesCount"`
	Frames        int    `json:"frames"`
	SampleRate    int    `json:"sampleRate"`
	FrameDurTicks int64  `json:"frameDurTicks"`
	NonPESAligned bool   `json:"nonPESAligned"`
	PESWithoutPTS int    `json:"pesWithoutPTS,omitempty"`
	PESStepTicks  Stat   `json:"pesStepTicks"`
	FirstPTS      int64  `json:"firstPTS"`
	LastPTS       int64  `json:"lastPTS"`
	DurationTicks int64  `json:"durationTicks"`
}

// ScanReport is the result of the scan pass, emitted by `mp2ts-loop -scan`.
type ScanReport struct {
	File         string              `json:"file"`
	TotalPackets int                 `json:"totalPackets"`
	Video        *VideoScan          `json:"video,omitempty"`
	Audio        []*AudioScan        `json:"audio,omitempty"`
	PassThrough  []PassThroughStream `json:"passThrough,omitempty"`
	Loop         *LoopPlan           `json:"loop,omitempty"`
	Note         string              `json:"note,omitempty"`
}

// runScan opens the file, identifies the streams, and runs the analysis pass,
// returning the TSStream plus the per-track scan records (video + audio).
func runScan(ctx context.Context, path string) (*TSStream, *scanTrack, []*scanTrack, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = fh.Close() }()

	ts, err := InitTS(fh)
	if err != nil {
		return nil, nil, nil, err
	}
	tracks := make(map[int]*scanTrack)
	for pid, es := range ts.ElStreams {
		if es.MediaType != "video" && es.MediaType != "audio" {
			continue
		}
		st := &scanTrack{pid: pid, codec: es.Codec, mediaType: es.MediaType}
		es.SetPESHandler(st)
		tracks[pid] = st
	}
	if err := ts.ProcessTSFile(ctx, fh); err != nil {
		return nil, nil, nil, err
	}
	var vid *scanTrack
	var auds []*scanTrack
	for _, st := range tracks {
		switch st.mediaType {
		case "video":
			vid = st
		case "audio":
			auds = append(auds, st)
		}
	}
	sort.Slice(auds, func(i, j int) bool { return auds[i].pid < auds[j].pid })
	return ts, vid, auds, nil
}

// loopBitrate returns the measured average bitrate and whether the stream is CBR.
func loopBitrate(ts *TSStream) (int64, bool) {
	if avg, _, _, constant, ok := ts.PCRBitrate(); ok {
		return avg, constant
	}
	return 0, false
}

// Scan analyzes a TS file and returns a ScanReport. fpsNum/fpsDen are an optional
// frame-rate hint (0 = auto-detect); durCapMS caps the loop length (0 = longest).
func Scan(ctx context.Context, path string, fpsNum, fpsDen, durCapMS int) (*ScanReport, error) {
	ts, vid, auds, err := runScan(ctx, path)
	if err != nil {
		return nil, err
	}
	rep := &ScanReport{File: path, TotalPackets: ts.TotalNrPackets()}
	if vid != nil {
		rep.Video = analyzeVideo(vid, fpsNum, fpsDen)
	}
	for _, a := range auds {
		rep.Audio = append(rep.Audio, analyzeAudio(a))
	}
	rep.PassThrough = ts.PassThrough
	if err := requirePTS(vid, auds); err != nil {
		rep.Note = "cannot loop: " + err.Error()
		return rep, nil
	}
	bitrate, cbr := loopBitrate(ts)
	rep.Loop = selectLoop(vid, auds, fpsNum, fpsDen, durCapMS, ts.PCRPid, bitrate, cbr)
	finishPlan(ts, rep.Loop)
	return rep, nil
}

// requirePTS returns an error if any video or audio PES lacked a PTS. The loop
// engine selects, trims, and shifts every PES by its PTS, so it does not loop
// such streams.
func requirePTS(vid *scanTrack, auds []*scanTrack) error {
	for _, st := range append([]*scanTrack{vid}, auds...) {
		if st != nil && st.noPTS > 0 {
			return fmt.Errorf("%s PID %d has %d PES packets without a PTS; every audio and video PES must carry one",
				st.mediaType, st.pid, st.noPTS)
		}
	}
	return nil
}

func analyzeVideo(st *scanTrack, fpsNum, fpsDen int) *VideoScan {
	v := &VideoScan{PID: st.pid, Codec: st.codec.String(), Pictures: len(st.pics), Field: "frame",
		PESWithoutPTS: st.noPTS}
	if len(st.pics) == 0 {
		return v
	}
	dts := make([]int64, len(st.pics))
	pts := make([]int64, len(st.pics))
	for i, p := range st.pics {
		dts[i] = p.DTS
		pts[i] = p.PTS
	}
	v.DTSStepTicks = statOf(CalculateSteps(dts))
	v.ConstantFrameRate = v.DTSStepTicks.Max-v.DTSStepTicks.Min <= 2
	if fpsNum > 0 && fpsDen > 0 {
		v.FrameRate = frLabel(fpsNum, fpsDen)
	} else {
		v.FrameRate = classifyFrameRateLabel(v.DTSStepTicks.Avg)
	}

	var eligible func(PicRecord) bool
	v.LoopPointType, eligible = loopPointClass(st.codec, st.pics)
	var loopPTS []int64
	for _, p := range st.pics {
		if eligible(p) {
			loopPTS = append(loopPTS, p.PTS)
		}
	}
	v.LoopPoints = len(loopPTS)
	if len(loopPTS) >= 2 {
		sort.Slice(loopPTS, func(i, j int) bool { return loopPTS[i] < loopPTS[j] })
		v.GOPDurationTicks = statOf(CalculateSteps(loopPTS))
		v.ConstantGOP = v.GOPDurationTicks.Max-v.GOPDurationTicks.Min <= 2
		v.MaxLoopTicks = loopPTS[len(loopPTS)-1] - loopPTS[0]
		if v.GOPDurationTicks.Avg > 0 {
			v.MaxLoopGOPs = v.MaxLoopTicks / v.GOPDurationTicks.Avg
		}
	}

	v.FirstPTS, v.LastPTS = minMax(pts)
	v.DurationTicks = UnsignedPTSDiff(v.LastPTS, v.FirstPTS)
	return v
}

func analyzeAudio(st *scanTrack) *AudioScan {
	a := &AudioScan{
		PID: st.pid, Codec: st.codec.String(), PESCount: len(st.audio),
		Frames: st.frames, NonPESAligned: st.nonAligned, PESWithoutPTS: st.noPTS,
	}
	if st.framer != nil {
		a.SampleRate = st.framer.SampleRate()
		a.FrameDurTicks = st.framer.FrameDurTicks()
	}
	if len(st.audio) == 0 {
		return a
	}
	pts := make([]int64, len(st.audio))
	for i, p := range st.audio {
		pts[i] = p.PTS
	}
	a.PESStepTicks = statOf(CalculateSteps(pts))
	a.FirstPTS = pts[0]
	a.LastPTS = pts[len(pts)-1]
	a.DurationTicks = UnsignedPTSDiff(a.LastPTS, a.FirstPTS)
	return a
}

func statOf(vals []int64) Stat {
	if len(vals) == 0 {
		return Stat{}
	}
	mn, mx, sum := vals[0], vals[0], int64(0)
	for _, v := range vals {
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
		sum += v
	}
	return Stat{Min: mn, Max: mx, Avg: sum / int64(len(vals))}
}

func minMax(vals []int64) (int64, int64) {
	if len(vals) == 0 {
		return 0, 0
	}
	mn, mx := vals[0], vals[0]
	for _, v := range vals {
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
	}
	return mn, mx
}

// nominalFrameRates are the frame rates the scan recognizes: label, exact
// rational rate num/den, and the DTS step (90 kHz ticks, rounded) they show as.
var nominalFrameRates = []struct {
	label    string
	num, den int
	step     int64
}{
	{"23.976", 24000, 1001, 3754}, {"24", 24, 1, 3750}, {"25", 25, 1, 3600},
	{"29.97", 30000, 1001, 3003}, {"30", 30, 1, 3000}, {"50", 50, 1, 1800},
	{"59.94", 60000, 1001, 1501}, {"60", 60, 1, 1500},
}

// nominalFrameRate returns the index in nominalFrameRates closest to a measured
// average DTS step, or -1 if none is within 15 ticks.
func nominalFrameRate(avgStep int64) int {
	best, bestDiff := -1, int64(1)<<62
	for i, f := range nominalFrameRates {
		d := avgStep - f.step
		if d < 0 {
			d = -d
		}
		if d < bestDiff {
			best, bestDiff = i, d
		}
	}
	if bestDiff > 15 || avgStep == 0 {
		return -1
	}
	return best
}

// classifyFrameRateLabel maps a measured average DTS step (90 kHz ticks) to a
// frame-rate label, including the fractional NTSC-derived rates.
func classifyFrameRateLabel(avgStep int64) string {
	if i := nominalFrameRate(avgStep); i >= 0 {
		return nominalFrameRates[i].label
	}
	if avgStep == 0 {
		return "unknown"
	}
	return fmt.Sprintf("~%.3f", 90000.0/float64(avgStep))
}

func frLabel(num, den int) string {
	if den == 1 {
		return strconv.Itoa(num)
	}
	switch {
	case num == 24000 && den == 1001:
		return "23.976"
	case num == 30000 && den == 1001:
		return "29.97"
	case num == 60000 && den == 1001:
		return "59.94"
	case num == 120000 && den == 1001:
		return "119.88"
	}
	return fmt.Sprintf("%d/%d", num, den)
}
