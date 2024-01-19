package app

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/sei"
	"github.com/asticode/go-astits"
)

type streamStatistics struct {
	Type       string  `json:"streamType"`
	Pid        uint16  `json:"pid"`
	FrameRate  float64 `json:"frameRate"`
	TimeStamps []int64 `json:"-"`
	MaxStep    int64   `json:"maxStep,omitempty"`
	MinStep    int64   `json:"minStep,omitempty"`
	AvgStep    int64   `json:"avgStep,omitempty"`
	// RAI-markers
	RAIPTS         []int64 `json:"-"`
	IDRPTS         []int64 `json:"-"`
	RAIGOPDuration int64   `json:"RAIGoPDuration,omitempty"`
	IDRGOPDuration int64   `json:"IDRGoPDuration,omitempty"`
	// Errors
	Errors []string `json:"errors,omitempty"`
}

type naluFrameData struct {
	PID   uint16     `json:"pid"`
	RAI   bool       `json:"rai"`
	PTS   int64      `json:"pts"`
	DTS   int64      `json:"dts,omitempty"`
	NALUS []naluData `json:"nalus,omitempty"`
}

type naluData struct {
	Type string `json:"type"`
	Len  int    `json:"len"`
	Data string `json:"data,omitempty"`
}

func sliceMinMaxAverage(values []int64) (min, max, avg int64) {
	if len(values) == 0 {
		return 0, 0, 0
	}

	min = values[0]
	max = values[0]
	sum := int64(0)
	for _, number := range values {
		if number < min {
			min = number
		}
		if number > max {
			max = number
		}
		sum += number
	}
	avg = sum / int64(len(values))
	return min, max, avg
}

func calculateSteps(timestamps []int64) []int64 {
	if len(timestamps) < 2 {
		return nil
	}

	// PTS/DTS are 33-bit values, so it wraps around after 26.5 hours
	steps := make([]int64, len(timestamps)-1)
	for i := 0; i < len(timestamps)-1; i++ {
		steps[i] = SignedPTSDiff(timestamps[i+1], timestamps[i])
	}
	return steps
}

// Calculate frame rate from DTS or PTS steps
func (s *streamStatistics) calculateFrameRate(timescale int64) {
	if len(s.TimeStamps) < 2 {
		s.Errors = append(s.Errors, "too few timestamps to calculate frame rate")
		return
	}

	steps := calculateSteps(s.TimeStamps)
	minStep, maxStep, avgStep := sliceMinMaxAverage(steps)
	if maxStep != minStep {
		s.Errors = append(s.Errors, "irregular PTS/DTS steps")
		s.MinStep, s.MaxStep, s.AvgStep = minStep, maxStep, avgStep
	}

	// fmt.Printf("Steps: %v\n", steps)
	// fmt.Printf("Average step: %f\n", avgStep)
	s.FrameRate = float64(timescale) / float64(avgStep)
}

func (s *streamStatistics) calculateGoPDuration(timescale int64) {
	if len(s.RAIPTS) < 2 || len(s.IDRPTS) < 2 {
		s.Errors = append(s.Errors, "no GoP duration since less than 2 I-frames")
		return
	}

	// Calculate GOP duration
	RAIPTSSteps := calculateSteps(s.RAIPTS)
	IDRPTSSteps := calculateSteps(s.IDRPTS)
	_, _, RAIGOPStep := sliceMinMaxAverage(RAIPTSSteps)
	_, _, IDRGOPStep := sliceMinMaxAverage(IDRPTSSteps)
	// fmt.Printf("RAIPTSSteps: %v\n", RAIPTSSteps)
	// fmt.Printf("RAIGOPStep: %d\n", RAIGOPStep)
	s.RAIGOPDuration = RAIGOPStep / timescale
	s.IDRGOPDuration = IDRGOPStep / timescale
}

func parseAVCPES(jp *jsonPrinter, d *astits.DemuxerData, ps *avcPS, o Options) (*avcPS, error) {
	pid := d.PID
	pes := d.PES
	fp := d.FirstPacket
	if pes.Header.OptionalHeader.PTS == nil {
		return nil, fmt.Errorf("no PTS in PES")
	}
	nfd := naluFrameData{
		PID: pid,
	}
	if ps == nil {
		// return empty PS to count picture numbers correctly
		// even if we are not printing NALUs
		ps = &avcPS{}
	}
	pts := *pes.Header.OptionalHeader.PTS
	nfd.PTS = pts.Base
	ps.statistics.Type = "AVC"
	ps.statistics.Pid = pid
	if fp != nil && fp.AdaptationField != nil {
		nfd.RAI = fp.AdaptationField.RandomAccessIndicator
		if nfd.RAI {
			ps.statistics.RAIPTS = append(ps.statistics.IDRPTS, pts.Base)
		}
	}

	dts := pes.Header.OptionalHeader.DTS
	if dts != nil {
		nfd.DTS = dts.Base
	} else {
		// Use PTS as DTS in statistics if DTS is not present
		nfd.DTS = pts.Base
	}
	ps.statistics.TimeStamps = append(ps.statistics.TimeStamps, nfd.DTS)

	if !o.ShowNALU {
		jp.print(nfd)
		return ps, jp.error()
	}

	data := pes.Data
	nalus := avc.ExtractNalusFromByteStream(data)
	firstPS := false
	for _, nalu := range nalus {
		seiMsg := ""
		naluType := avc.GetNaluType(nalu[0])
		switch naluType {
		case avc.NALU_SPS:
			if !firstPS {
				err := ps.setSPS(nalu)
				if err != nil {
					return nil, fmt.Errorf("cannot set SPS")
				}
				firstPS = true
			}
		case avc.NALU_PPS:
			if firstPS {
				err := ps.setPPS(nalu)
				if err != nil {
					return nil, fmt.Errorf("cannot set PPS")
				}
			}
		case avc.NALU_SEI:
			if !o.ShowSEI {
				continue
			}
			var sps *avc.SPS
			if firstPS {
				sps = ps.getSPS()
			}
			msgs, err := avc.ParseSEINalu(nalu, sps)
			if err != nil {
				return nil, err
			}
			seiTexts := make([]string, 0, len(msgs))
			for _, msg := range msgs {
				if msg.Type() == sei.SEIPicTimingType {
					pt := msg.(*sei.PicTimingAvcSEI)
					seiTexts = append(seiTexts, fmt.Sprintf("Type 1: %s", pt.Clocks[0]))
				}
			}
			seiMsg = strings.Join(seiTexts, ", ")
		case avc.NALU_IDR:
			ps.statistics.IDRPTS = append(ps.statistics.IDRPTS, pts.Base)
		}
		nfd.NALUS = append(nfd.NALUS, naluData{
			Type: naluType.String(),
			Len:  len(nalu),
			Data: seiMsg,
		})
	}

	if firstPS {
		for nr := range ps.spss {
			printPS(jp, pid, "SPS", nr, ps.spsnalu, ps.spss[nr], o.ParameterSets)
		}
		for nr := range ps.ppss {
			printPS(jp, pid, "PPS", nr, ps.ppsnalus[nr], ps.ppss[nr], o.ParameterSets)
		}
	}
	jp.print(nfd)
	return ps, jp.error()
}

type avcPS struct {
	spss       map[uint32]*avc.SPS
	ppss       map[uint32]*avc.PPS
	spsnalu    []byte
	ppsnalus   [][]byte
	statistics streamStatistics
}

func (a *avcPS) getSPS() *avc.SPS {
	return a.spss[0]
}

func (a *avcPS) setSPS(nalu []byte) error {
	if a.spss == nil {
		a.spss = make(map[uint32]*avc.SPS, 1)
		a.ppss = make(map[uint32]*avc.PPS, 1)
		a.ppsnalus = make([][]byte, 1)
	}
	sps, err := avc.ParseSPSNALUnit(nalu, true)
	if err != nil {
		return err
	}
	a.spsnalu = nalu
	a.spss[sps.ParameterID] = sps
	if len(a.spss) > 1 {
		return fmt.Errorf("more than one SPS")
	}
	return nil
}

func (a *avcPS) setPPS(nalu []byte) error {
	pps, err := avc.ParsePPSNALUnit(nalu, a.spss)
	if err != nil {
		return err
	}
	a.ppss[pps.PicParameterSetID] = pps
	a.ppsnalus[pps.PicParameterSetID] = nalu
	return nil
}

type psInfo struct {
	PID          uint16 `json:"pid"`
	ParameterSet string `json:"parameterSet"`
	Nr           uint32 `json:"nr"`
	Hex          string `json:"hex"`
	Length       int    `json:"length"`
	Details      any    `json:"details,omitempty"`
}

func printPS(jp *jsonPrinter, pid uint16, psKind string, nr uint32, ps []byte, details any, verbose bool) {
	hexStr := hex.EncodeToString(ps)
	length := len(hexStr) / 2
	psInfo := psInfo{
		PID:          pid,
		ParameterSet: psKind,
		Nr:           nr,
		Hex:          hexStr,
		Length:       length,
	}
	if verbose {
		psInfo.Details = details
	}
	jp.print(psInfo)
}
