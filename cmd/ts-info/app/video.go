package app

import (
	"encoding/hex"
	"fmt"
	"sort"
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

// Calculate frame rate from DTS or PTS steps
func (s *streamStatistics) update() {
	if len(s.PTSSteps) < 2 && len(s.DTSSteps) < 2 {
		return
	}
	// Use DTS steps if possible, and PTS steps otherwise
	dataRange := s.PTSSteps
	if len(s.DTSSteps) >= 2 {
		dataRange = s.DTSSteps
	}
	// Sort steps in increasing order
	sort.Slice(dataRange, func(i, j int) bool { return dataRange[i] < dataRange[j] })

	// TODO: Handle wrap-around by removing outliers

	// Calculate steps
	steps := make([]int64, len(dataRange)-1)
	for i := 0; i < len(dataRange)-1; i++ {
		steps[i] = dataRange[i+1] - dataRange[i]
	}
	minStep, maxStep, avgStep := sliceMinMaxAverage(steps)
	if maxStep != minStep {
		s.MinStep, s.MaxStep, s.AvgStep = minStep, maxStep, avgStep
	}

	// fmt.Printf("dataRange: %v\n", dataRange)
	// fmt.Printf("Steps: %v\n", steps)
	// fmt.Printf("Average step: %f\n", avgStep)
	s.FrameRate = float64(90000) / float64(avgStep)
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
	if fp != nil {
		af := fp.AdaptationField
		if af != nil {
			nfd.RAI = af.RandomAccessIndicator
		}
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
