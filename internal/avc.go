package internal

import (
	"encoding/hex"
	"fmt"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/sei"
	"github.com/asticode/go-astits"
)

type AvcPS struct {
	spss       map[uint32]*avc.SPS
	ppss       map[uint32]*avc.PPS
	spsnalu    []byte
	ppsnalus   map[uint32][]byte
	lastSPSHex string
	lastPPSHex map[uint32]string
	Statistics StreamStatistics
}

func (a *AvcPS) getSPS() *avc.SPS {
	if len(a.spss) == 0 {
		return nil
	}
	for _, sps := range a.spss {
		return sps
	}
	// Not reachable
	return nil
}

func (a *AvcPS) hasPS() bool {
	return len(a.spss) > 0 && len(a.ppss) > 0
}

func (a *AvcPS) setSPS(nalu []byte) error {
	if a.spss == nil {
		a.spss = make(map[uint32]*avc.SPS, 1)
		a.ppss = make(map[uint32]*avc.PPS, 1)
		a.ppsnalus = make(map[uint32][]byte)
		a.lastPPSHex = make(map[uint32]string)
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

func (a *AvcPS) setPPS(nalu []byte) error {
	pps, err := avc.ParsePPSNALUnit(nalu, a.spss)
	if err != nil {
		return err
	}
	a.ppss[pps.PicParameterSetID] = pps
	a.ppsnalus[pps.PicParameterSetID] = nalu
	return nil
}

func ParseAVCPES(jp *JsonPrinter, d *astits.DemuxerData, ps *AvcPS, o Options) (*AvcPS, error) {
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
		ps = &AvcPS{}
	}
	pts := *pes.Header.OptionalHeader.PTS
	nfd.PTS = pts.Base
	ps.Statistics.Type = "AVC"
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
	nalus := avc.ExtractNalusFromByteStream(data)
	firstPS := false
	for _, nalu := range nalus {
		var data any
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
			sps := ps.getSPS()
			msgs, err := avc.ParseSEINalu(nalu, sps)
			if err != nil {
				return nil, err
			}
			parts := make([]SeiOut, 0, len(msgs))
			for _, msg := range msgs {
				t := sei.SEIType(msg.Type())
				if t == sei.SEIPicTimingType {
					pt := msg.(*sei.PicTimingAvcSEI)
					if o.ShowSEIDetails && sps != nil {
						parts = append(parts, SeiOut{
							Msg:     t.String(),
							Payload: picTimingAvcToOut(pt),
						})
					} else {
						parts = append(parts, SeiOut{Msg: t.String()})
					}
				} else {
					if o.ShowSEIDetails {
						parts = append(parts, SeiOut{Msg: t.String(), Payload: msg})
					} else {
						parts = append(parts, SeiOut{Msg: t.String()})
					}
				}
			}
			data = parts
		case avc.NALU_IDR, avc.NALU_NON_IDR:
			if naluType == avc.NALU_IDR {
				ps.Statistics.IDRPTS = append(ps.Statistics.IDRPTS, pts.Base)
			}
			sliceType, err := avc.GetSliceTypeFromNALU(nalu)
			if err == nil {
				nfd.ImgType = fmt.Sprintf("[%s]", sliceType)
			}
		}
		nfd.NALUS = append(nfd.NALUS, NaluData{
			Type: naluType.String(),
			Len:  len(nalu),
			Data: data,
		})
	}

	if jp == nil {
		return ps, nil
	}
	if firstPS {
		spsHex := hex.EncodeToString(ps.spsnalu)
		if spsHex != ps.lastSPSHex {
			ps.lastSPSHex = spsHex
			for nr := range ps.spss {
				jp.PrintPS(pid, "SPS", nr, ps.spsnalu, ps.spss[nr], o.VerbosePSInfo, o.ShowPS)
			}
		}
		for nr := range ps.ppss {
			ppsHex := hex.EncodeToString(ps.ppsnalus[nr])
			if ppsHex != ps.lastPPSHex[nr] {
				ps.lastPPSHex[nr] = ppsHex
				jp.PrintPS(pid, "PPS", nr, ps.ppsnalus[nr], ps.ppss[nr], o.VerbosePSInfo, o.ShowPS)
			}
		}
	}

	// Skip printing if WaitForPS is enabled and we don't have parameter sets yet
	if o.WaitForPS && !ps.hasPS() {
		return ps, nil
	}

	jp.Print(nfd, o.ShowNALU)
	return ps, jp.Error()
}

// picTimingAvcToOut converts a PicTimingAvcSEI to a richer output struct
// that exposes all clock timestamp fields hidden by MarshalJSON.
func picTimingAvcToOut(pt *sei.PicTimingAvcSEI) PicTimingAvcOut {
	out := PicTimingAvcOut{
		PictStruct: pt.PictStruct,
		Clocks:     make([]ClockTSAvcOut, 0, len(pt.Clocks)),
	}
	if pt.CbpDbpDelay != nil {
		out.CpbRemovalDelay = &pt.CbpDbpDelay.CpbRemovalDelay
		out.DpbOutputDelay = &pt.CbpDbpDelay.DpbOutputDelay
	}
	for _, c := range pt.Clocks {
		co := ClockTSAvcOut{
			ClockTimeStampFlag: c.ClockTimeStampFlag,
		}
		if c.ClockTimeStampFlag {
			co.CtType = &c.CtType
			co.NuitFieldBasedFlag = &c.NuitFieldBasedFlag
			co.CountingType = &c.CountingType
			co.FullTimeStampFlag = &c.FullTimeStampFlag
			co.DiscontinuityFlag = &c.DiscontinuityFlag
			co.CntDroppedFlag = &c.CntDroppedFlag
			co.NFrames = &c.NFrames
			co.Time = fmt.Sprintf("%02d:%02d:%02d:%02d", c.Hours, c.Minutes, c.Seconds, c.NFrames)
			co.TimeOffset = &c.TimeOffsetValue
		}
		out.Clocks = append(out.Clocks, co)
	}
	return out
}
