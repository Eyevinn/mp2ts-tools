package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/sei"
	"github.com/asticode/go-astits"
)

type Options struct {
	ParameterSets bool
	Version       bool
	MaxNrPictures int
}

const (
	packetSize = 188
)

func Parse(ctx context.Context, w io.Writer, f io.Reader, o Options) error {
	rd := bufio.NewReaderSize(f, 1000*packetSize)
	dmx := astits.NewDemuxer(ctx, rd)
	pmtPID := -1
	nrPics := 0
	sdtPrinted := false
	esKinds := make(map[uint16]string)
	avcPSs := make(map[uint16]*avcPS)
dataLoop:
	for {
		d, err := dmx.NextData()
		if err != nil {
			if err.Error() == "astits: no more packets" {
				break
			}
			return fmt.Errorf("reading next data %w", err)
		}
		if d.SDT != nil && !sdtPrinted {
			parts := make([]string, 0, 4)
			for _, s := range d.SDT.Services {
				parts = append(parts, fmt.Sprintf("service_id: %d", s.ServiceID))
				for _, d := range s.Descriptors {
					switch d.Tag {
					case astits.DescriptorTagService:
						sd := d.Service
						parts = append(parts, fmt.Sprintf("service_name: %s", string(sd.Name)))
						parts = append(parts, fmt.Sprintf("provider_name: %s", string(sd.Provider)))
					}
				}
			}
			fmt.Fprintf(w, "SDT: %s\n", strings.Join(parts, ", "))
			sdtPrinted = true
		}
		if pmtPID < 0 && d.PMT != nil {
			// Loop through elementary streams
			for _, es := range d.PMT.ElementaryStreams {
				switch es.StreamType {
				case astits.StreamTypeH264Video:
					fmt.Fprintf(w, "H264 video detected on PID: %d\n", es.ElementaryPID)
					esKinds[es.ElementaryPID] = "AVC"
				case astits.StreamTypeAACAudio:
					fmt.Fprintf(w, "AAC audio detected on PID: %d\n", es.ElementaryPID)
					esKinds[es.ElementaryPID] = "AAC"
				}
			}
			pmtPID = int(d.PID)
		}
		if pmtPID == -1 {
			continue
		}
		pes := d.PES
		if pes == nil {
			continue
		}
		switch esKinds[d.PID] {
		case "AVC":
			avcPS := avcPSs[d.PID]
			avcPS, err = parseAVCPES(w, d, avcPS, o.ParameterSets)
			if err != nil {
				return err
			}
			if avcPS == nil {
				continue
			}
			if avcPSs[d.PID] == nil {
				avcPSs[d.PID] = avcPS
			}
			nrPics++
			if o.MaxNrPictures > 0 && nrPics == o.MaxNrPictures {
				break dataLoop
			}
		}
	}
	return nil
}

func parseAVCPES(w io.Writer, d *astits.DemuxerData, ps *avcPS, verbose bool) (*avcPS, error) {
	pid := d.PID
	pes := d.PES
	fp := d.FirstPacket
	if pes.Header.OptionalHeader.PTS == nil {
		return nil, fmt.Errorf("no PTS in PES")
	}
	outText := fmt.Sprintf("PID: %d, ", pid)
	if fp != nil {
		af := fp.AdaptationField
		if af != nil {
			outText += fmt.Sprintf("RAI: %t, ", af.RandomAccessIndicator)
		}
	}
	pts := *pes.Header.OptionalHeader.PTS
	data := pes.Data
	outText += fmt.Sprintf("PTS: %d, ", pts.Base)

	dts := pes.Header.OptionalHeader.DTS
	if dts != nil {
		outText += fmt.Sprintf("DTS: %d, ", dts.Base)
	}
	nalus := avc.ExtractNalusFromByteStream(data)
	firstPS := false
	outText += "NALUs: "
	for _, nalu := range nalus {
		var seiMsg string
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
			seiMsg += " "
		}
		outText += fmt.Sprintf("[%s %s%dB]", naluType, seiMsg, len(nalu))
	}
	if ps == nil {
		return nil, nil
	}
	if firstPS {
		for i := range ps.spss {
			printPS(w, fmt.Sprintf("PID %d, SPS", pid), i, ps.spsnalu, ps.spss[i], verbose)
		}
		for i := range ps.ppss {
			printPS(w, fmt.Sprintf("PID %d, PPS", pid), i, ps.ppsnalus[i], ps.ppss[i], verbose)
		}
	}
	fmt.Fprintln(w, outText)
	return ps, nil
}
