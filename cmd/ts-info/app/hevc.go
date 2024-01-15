package app

import (
	"bytes"
	"fmt"

	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/sei"
	"github.com/asticode/go-astits"
)

type hevcPS struct {
	spss     map[uint32]*hevc.SPS
	ppss     map[uint32]*hevc.PPS
	spsnalu  []byte
	ppsnalus [][]byte
}

func (a *hevcPS) getSPS() *hevc.SPS {
	return a.spss[0]
}

func (a *hevcPS) setSPS(nalu []byte) error {
	if a.spss == nil {
		a.spss = make(map[uint32]*hevc.SPS, 1)
		a.ppss = make(map[uint32]*hevc.PPS, 1)
		a.ppsnalus = make([][]byte, 1)
	}
	sps, err := hevc.ParseSPSNALUnit(nalu)
	if err != nil {
		return err
	}
	a.spsnalu = nalu
	a.spss[uint32(sps.SpsID)] = sps
	if len(a.spss) > 1 {
		return fmt.Errorf("more than one SPS")
	}
	return nil
}

func (a *hevcPS) setPPS(nalu []byte) error {
	pps, err := hevc.ParsePPSNALUnit(nalu, a.spss)
	if err != nil {
		return err
	}
	a.ppss[pps.PicParameterSetID] = pps
	a.ppsnalus[pps.PicParameterSetID] = nalu
	return nil
}

func parseHEVCPES(jp *jsonPrinter, d *astits.DemuxerData, ps *hevcPS, verbose bool) (*hevcPS, error) {
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
	firstPS := false

	for _, spsNalu := range hevc.ExtractNalusOfTypeFromByteStream(hevc.NALU_SPS, data, true) {
		if ps == nil && !firstPS {
			ps = &hevcPS{}
			err := ps.setSPS(spsNalu)
			if err != nil {
				return nil, fmt.Errorf("cannot set SPS")
			}
			firstPS = true
		}
	}

	for _, ppsNalu := range hevc.ExtractNalusOfTypeFromByteStream(hevc.NALU_PPS, data, true) {
		if firstPS {
			err := ps.setPPS(ppsNalu)
			if err != nil {
				return nil, fmt.Errorf("cannot set PPS")
			}
		}
	}

	var hdrLen = 2
	for _, seiNalu := range hevc.ExtractNalusOfTypeFromByteStream(hevc.NALU_SEI_PREFIX, data, true) {
		seiBytes := seiNalu[hdrLen:]
		buf := bytes.NewReader(seiBytes)
		seiDatas, err := sei.ExtractSEIData(buf)
		if err != nil {
			return nil, err
		}

		for _, seiData := range seiDatas {
			var seiMsg sei.SEIMessage
			seiMsg, err = sei.DecodeSEIMessage(&seiData, sei.HEVC)
			if err != nil {
				fmt.Printf("SEI: Got error %q\n", err)
				continue
			}

			nfd.NALUS = append(nfd.NALUS, naluData{
				Type: hevc.NALU_SEI_PREFIX.String(),
				Len:  len(seiNalu),
				Data: seiMsg.String(),
			})
		}
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
