package app

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/sei"
	"github.com/asticode/go-astits"
)

type naluFrameData struct {
	PID   uint16     `json:"pid"`
	RAI   bool       `json:"rai"`
	PTS   int64      `json:"pts"`
	DTS   *int64     `json:"dts,omitempty"`
	NALUS []naluData `json:"nalus"`
}

type naluData struct {
	Type string `json:"type"`
	Len  int    `json:"len"`
	Data string `json:"data,omitempty"`
}

func parseAVCPES(jp *jsonPrinter, d *astits.DemuxerData, ps *avcPS, verbose bool) (*avcPS, error) {
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
	pts := *pes.Header.OptionalHeader.PTS
	nfd.PTS = pts.Base
	dts := pes.Header.OptionalHeader.DTS
	if dts != nil {
		nfd.DTS = &dts.Base
	}
	data := pes.Data
	nalus := avc.ExtractNalusFromByteStream(data)
	firstPS := false
	for _, nalu := range nalus {
		seiMsg := ""
		naluType := avc.GetNaluType(nalu[0])
		switch naluType {
		case avc.NALU_SPS:
			if ps == nil && !firstPS {
				ps = &avcPS{}
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
			printPS(jp, pid, "SPS", nr, ps.spsnalu, ps.spss[nr], verbose)
		}
		for nr := range ps.ppss {
			printPS(jp, pid, "PPS", nr, ps.ppsnalus[nr], ps.ppss[nr], verbose)
		}
	}
	jp.print(nfd)
	return ps, jp.error()
}

type avcPS struct {
	spss     map[uint32]*avc.SPS
	ppss     map[uint32]*avc.PPS
	spsnalu  []byte
	ppsnalus [][]byte
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
