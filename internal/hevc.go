package internal

import (
	"fmt"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/sei"
	"github.com/asticode/go-astits"
)

type HevcPS struct {
	spss       map[uint32]*hevc.SPS
	ppss       map[uint32]*hevc.PPS
	vpsnalu    []byte
	spsnalu    []byte
	ppsnalus   map[uint32][]byte
	Statistics StreamStatistics
}

func (a *HevcPS) setSPS(nalu []byte) error {
	if a.spss == nil {
		a.spss = make(map[uint32]*hevc.SPS, 1)
		a.ppss = make(map[uint32]*hevc.PPS, 1)
		a.ppsnalus = make(map[uint32][]byte, 1)
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

func (a *HevcPS) setPPS(nalu []byte) error {
	pps, err := hevc.ParsePPSNALUnit(nalu, a.spss)
	if err != nil {
		return err
	}
	a.ppss[pps.PicParameterSetID] = pps
	a.ppsnalus[pps.PicParameterSetID] = nalu
	return nil
}

func ParseHEVCPES(jp *JsonPrinter, d *astits.DemuxerData, ps *HevcPS, o Options) (*HevcPS, error) {
	pid := d.PID
	pes := d.PES
	fp := d.FirstPacket
	if pes.Header.OptionalHeader.PTS == nil {
		return nil, fmt.Errorf("no PTS in PES")
	}
	nfd := NaluFrameData{
		PID: pid,
	}
	if ps == nil {
		// return empty PS to count picture numbers correctly
		// even if we are not printing NALUs
		ps = &HevcPS{}
	}
	pts := *pes.Header.OptionalHeader.PTS
	nfd.PTS = pts.Base
	ps.Statistics.Type = "HEVC"
	ps.Statistics.Pid = pid
	if fp != nil && fp.AdaptationField != nil {
		nfd.RAI = fp.AdaptationField.RandomAccessIndicator
		if nfd.RAI {
			ps.Statistics.RAIPTS = append(ps.Statistics.IDRPTS, pts.Base)
		}
	}

	dts := pes.Header.OptionalHeader.DTS
	if dts != nil {
		nfd.DTS = dts.Base
	} else {
		// Use PTS as DTS in statistics if DTS is not present
		nfd.DTS = pts.Base
	}
	ps.Statistics.TimeStamps = append(ps.Statistics.TimeStamps, nfd.DTS)

	data := pes.Data
	firstPS := false
	for _, nalu := range avc.ExtractNalusFromByteStream(data) {
		naluType := hevc.GetNaluType(nalu[0])
		switch naluType {
		case hevc.NALU_VPS:
			ps.vpsnalu = nalu
			firstPS = true
		case hevc.NALU_SPS:
			if firstPS {
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
		case hevc.NALU_SEI_PREFIX, hevc.NALU_SEI_SUFFIX:

			var seiData any
			if o.ShowSEIDetails && len(ps.spss) > 0 {
				sps := ps.spss[0]
				seiMessages, err := hevc.ParseSEINalu(nalu, sps)
				if err != nil {
					return nil, fmt.Errorf("cannot parse SEI NALU")
				}
				parts := make([]SeiOut, 0, len(seiMessages))
				for _, seiMsg := range seiMessages {
					var payload any
					switch seiMsg.Type() {
					case sei.SEIPicTimingType:
						payload = seiMsg.(*sei.PicTimingHevcSEI)
					}
					parts = append(parts, SeiOut{Msg: sei.SEIType(seiMsg.Type()).String(), Payload: payload})
				}
				seiData = parts
			} else {
				seiData = nil // hex.EncodeToString(nalu)
			}
			nfd.NALUS = append(nfd.NALUS, NaluData{
				Type: naluType.String(),
				Len:  len(nalu),
				Data: seiData,
			})
			continue
		case hevc.NALU_IDR_W_RADL, hevc.NALU_IDR_N_LP:
			ps.Statistics.IDRPTS = append(ps.Statistics.IDRPTS, pts.Base)
		}
		nfd.NALUS = append(nfd.NALUS, NaluData{
			Type: naluType.String(),
			Len:  len(nalu),
			Data: nil,
		})
	}

	if firstPS {
		jp.PrintPS(pid, "VPS", 0, ps.vpsnalu, nil, o.VerbosePSInfo, o.ShowPS)
		for nr := range ps.spss {
			jp.PrintPS(pid, "SPS", nr, ps.spsnalu, ps.spss[nr], o.VerbosePSInfo, o.ShowPS)
		}
		for nr := range ps.ppss {
			jp.PrintPS(pid, "PPS", nr, ps.ppsnalus[nr], ps.ppss[nr], o.VerbosePSInfo, o.ShowPS)
		}
	}

	jp.Print(nfd, o.ShowNALU)
	return ps, jp.Error()
}
