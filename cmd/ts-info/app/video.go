package app

import (
	"encoding/hex"
	"fmt"
	"math"
	"strings"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/sei"
	"github.com/asticode/go-astits"
)

type streamStatistics struct {
	Type      string  `json:"streamType"`
	Pid       uint16  `json:"pid"`
	FrameRate float64 `json:"frameRate"`
	// skip DTSSteps and PTSSteps in json output
	DTSSteps []int64 `json:"-"`
	PTSSteps []int64 `json:"-"`
	MaxStep  int64   `json:"maxStep,omitempty"`
	MinStep  int64   `json:"minStep,omitempty"`
	AvgStep  int64   `json:"avgStep,omitempty"`
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

func sliceMinMaxAverage(values []int64) (int64, int64, int64) {
	min := values[0] //assign the first element equal to min
	max := values[0] //assign the first element equal to max
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
	avg := sum / int64(len(values))
	return min, max, avg
}

func valuesHigherThanZero(values []int64) bool {
	for i := 0; i < len(values)-1; i++ {
		if values[i] < 0 {
			return false
		}
	}
	return true
}

func calculateStepsInSlice(values []int64) []int64 {
	if len(values) < 2 {
		return nil
	}
	// Note: we assume that the values are monotonically increasing
	// PTS/DTS are 33-bit values, so it wraps around after 26.5 hours
	MAXSTEP := int64(math.Pow(2, 33)) - 1
	steps := make([]int64, len(values)-1)
	for i := 0; i < len(values)-1; i++ {
		rawDifference := values[i+1] - values[i]
		if rawDifference < -MAXSTEP/2 {
			// if the time stamp was wrapped around, we pad it with MAXSTEP
			rawDifference = rawDifference + MAXSTEP
		}
		steps[i] = rawDifference
	}
	return steps
}

// Calculate frame rate from DTS or PTS steps
func (s *streamStatistics) calculateFrameRate(timescale int64) {
	if len(s.PTSSteps) < 2 && len(s.DTSSteps) < 2 {
		s.Errors = append(s.Errors, "Not enough PTS/DTS steps to calculate frame rate")
		return
	}
	// Use DTS steps if possible, and PTS steps otherwise
	dataRange := s.PTSSteps
	if len(s.DTSSteps) >= 2 {
		dataRange = s.DTSSteps
	}

	// Calculate steps
	steps := calculateStepsInSlice(dataRange)
	isMonotonicallyIncreasing := valuesHigherThanZero(steps)
	// dataRange must be monotonically increasing
	if !isMonotonicallyIncreasing {
		s.Errors = append(s.Errors, "PTS/DTS steps are not monotonically increasing")
		// fmt.Printf("DataRange: %v\n", dataRange)
		// fmt.Printf("Steps: %v\n", steps)
		return
	}

	minStep, maxStep, avgStep := sliceMinMaxAverage(steps)
	if maxStep != minStep {
		s.Errors = append(s.Errors, "PTS/DTS steps are not constant")
		s.MinStep, s.MaxStep, s.AvgStep = minStep, maxStep, avgStep
	}

	// fmt.Printf("dataRange: %v\n", dataRange)
	// fmt.Printf("Steps: %v\n", steps)
	// fmt.Printf("Average step: %f\n", avgStep)
	s.FrameRate = float64(timescale) / float64(avgStep)
}

func (s *streamStatistics) calculateGoPDuration(timescale int64) {
	if len(s.RAIPTS) < 2 || len(s.IDRPTS) < 2 {
		s.Errors = append(s.Errors, "Not enough PTS steps to calculate GOP duration")
		return
	}
	// Calculate GOP duration
	RAIPTSSteps := calculateStepsInSlice(s.RAIPTS)
	IDRPTSSteps := calculateStepsInSlice(s.IDRPTS)

	// PTS must be monotonically increasing
	isMonotonicallyIncreasing := valuesHigherThanZero(RAIPTSSteps) && valuesHigherThanZero(IDRPTSSteps)
	// dataRange must be monotonically increasing
	if !isMonotonicallyIncreasing {
		s.Errors = append(s.Errors, "PTS steps are not monotonically increasing")
		return
	}
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
	ps.statistics.PTSSteps = append(ps.statistics.PTSSteps, pts.Base)
	if fp != nil && fp.AdaptationField != nil {
		nfd.RAI = fp.AdaptationField.RandomAccessIndicator
		if nfd.RAI {
			ps.statistics.RAIPTS = append(ps.statistics.IDRPTS, pts.Base)
		}
	}

	dts := pes.Header.OptionalHeader.DTS
	if dts != nil {
		nfd.DTS = dts.Base
		ps.statistics.DTSSteps = append(ps.statistics.DTSSteps, dts.Base)
	}
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
			if ps != nil {
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
	if ps == nil {
		return nil, nil
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
