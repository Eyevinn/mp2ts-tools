package app

import (
	"bytes"
	"fmt"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/sei"
	"github.com/asticode/go-astits"
)

type hevcPS struct {
	spss       map[uint32]*hevc.SPS
	ppss       map[uint32]*hevc.PPS
	vpsnalu    []byte
	spsnalu    []byte
	ppsnalus   [][]byte
	statistics streamStatistics
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

func parseHEVCPES(jp *jsonPrinter, d *astits.DemuxerData, ps *hevcPS, o Options) (*hevcPS, error) {
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
		ps = &hevcPS{}
	}
	pts := *pes.Header.OptionalHeader.PTS
	nfd.PTS = pts.Base
	ps.statistics.Type = "HEVC"
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
	firstPS := false
	for _, nalu := range avc.ExtractNalusFromByteStream(data) {
		naluType := hevc.GetNaluType(nalu[0])
		// Handle SEI messages separately
		if naluType == hevc.NALU_SEI_PREFIX || naluType == hevc.NALU_SEI_SUFFIX {
			if !o.ShowSEI {
				continue
			}
			var hdrLen = 2
			seiBytes := nalu[hdrLen:]
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
					Type: naluType.String(),
					Len:  len(nalu),
					Data: seiMsg.String(),
				})
			}

			continue
		}

		// Handle other NALUs
		switch naluType {
		case hevc.NALU_SPS:
			if !firstPS {
				err := ps.setSPS(nalu)
				if err != nil {
					return nil, fmt.Errorf("cannot set SPS")
				}
				firstPS = true
			}
		case hevc.NALU_PPS:
			if firstPS {
				err := ps.setPPS(nalu)
				if err != nil {
					return nil, fmt.Errorf("cannot set PPS")
				}
			}
		case hevc.NALU_VPS:
			ps.vpsnalu = nalu
		case hevc.NALU_IDR_W_RADL, hevc.NALU_IDR_N_LP:
			ps.statistics.IDRPTS = append(ps.statistics.IDRPTS, pts.Base)
		}
		nfd.NALUS = append(nfd.NALUS, naluData{
			Type: naluType.String(),
			Len:  len(nalu),
			Data: "",
		})
	}

	if firstPS {
		printPS(jp, pid, "VPS", 0, ps.vpsnalu, nil, o.ParameterSets)
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
