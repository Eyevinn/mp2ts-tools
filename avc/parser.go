package avc

import (
	"bufio"
	"context"
	"fmt"
	"io"

	"github.com/Eyevinn/mp2ts-tools/common"
	"github.com/Eyevinn/mp2ts-tools/hevc"
	"github.com/asticode/go-astits"
)

func ParseAll(ctx context.Context, w io.Writer, f io.Reader, o common.Options) error {
	rd := bufio.NewReaderSize(f, 1000*common.PacketSize)
	dmx := astits.NewDemuxer(ctx, rd)
	pmtPID := -1
	nrPics := 0
	sdtPrinted := false
	esKinds := make(map[uint16]string)
	avcPSs := make(map[uint16]*AvcPS)
	hevcPSs := make(map[uint16]*hevc.HevcPS)
	jp := &common.JsonPrinter{W: w, Indent: o.Indent}
	statistics := make(map[uint16]*common.StreamStatistics)
dataLoop:
	for {
		// Check if context was cancelled
		select {
		case <-ctx.Done():
			break dataLoop
		default:
		}

		d, err := dmx.NextData()
		if err != nil {
			if err.Error() == "astits: no more packets" {
				break dataLoop
			}
			return fmt.Errorf("reading next data %w", err)
		}

		// Print service information
		if d.SDT != nil && !sdtPrinted {
			jp.PrintSdtInfo(d.SDT, o.ShowService)
			sdtPrinted = true
		}

		// Print PID information
		if pmtPID < 0 && d.PMT != nil {
			// Loop through elementary streams
			for _, es := range d.PMT.ElementaryStreams {
				var streamInfo *common.ElementaryStreamInfo
				switch es.StreamType {
				case astits.StreamTypeH264Video:
					streamInfo = &common.ElementaryStreamInfo{PID: es.ElementaryPID, Codec: "AVC", Type: "video"}
					esKinds[es.ElementaryPID] = "AVC"
				case astits.StreamTypeAACAudio:
					streamInfo = &common.ElementaryStreamInfo{PID: es.ElementaryPID, Codec: "AAC", Type: "audio"}
					esKinds[es.ElementaryPID] = "AAC"
				case astits.StreamTypeH265Video:
					streamInfo = &common.ElementaryStreamInfo{PID: es.ElementaryPID, Codec: "HEVC", Type: "video"}
					esKinds[es.ElementaryPID] = "HEVC"
				}

				if streamInfo != nil {
					jp.Print(streamInfo, o.ShowStreamInfo)
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
			avcPS, err = ParseAVCPES(jp, d, avcPS, o)
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
			statistics[d.PID] = &avcPS.Statistics
		case "HEVC":
			hevcPS := hevcPSs[d.PID]
			hevcPS, err = hevc.ParseHEVCPES(jp, d, hevcPS, o)
			if err != nil {
				return err
			}
			if hevcPS == nil {
				continue
			}
			if hevcPSs[d.PID] == nil {
				hevcPSs[d.PID] = hevcPS
			}
			nrPics++
			statistics[d.PID] = &hevcPS.Statistics
		default:
			// Skip unknown elementary streams
			continue
		}

		// Keep looping if MaxNrPictures equals 0
		if o.MaxNrPictures > 0 && nrPics >= o.MaxNrPictures {
			break dataLoop
		}
	}

	for _, s := range statistics {
		jp.PrintStatistics(*s, o.ShowStatistics)
	}

	return jp.Error()
}
