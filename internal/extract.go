package internal

import (
	"bufio"
	"context"
	"fmt"
	"io"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/asticode/go-astits"
)

// ExtractES extracts elementary stream from a TS file
func ExtractES(ctx context.Context, textWriter io.Writer, esWriter io.Writer, f io.Reader, o Options) error {
	rd := bufio.NewReaderSize(f, 1000*PacketSize)
	dmx := astits.NewDemuxer(ctx, rd)
	pmtPID := -1
	esKinds := make(map[uint16]string)
	targetPID := uint16(0)
	extracting := false
	hasAVCPS := false
	hasHEVCPS := false
	jp := &JsonPrinter{W: textWriter, Indent: o.Indent}

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

		// Print PID information
		if pmtPID < 0 && d.PMT != nil {
			// Loop through elementary streams
			for _, es := range d.PMT.ElementaryStreams {
				streamInfo := ParseAstitsElementaryStreamInfo(es)
				if streamInfo != nil {
					esKinds[es.ElementaryPID] = streamInfo.Codec
					jp.Print(streamInfo, o.ShowStreamInfo)

					// Select target PID
					if targetPID == 0 && (streamInfo.Codec == "AVC" || streamInfo.Codec == "HEVC") {
						if o.ExtractPID == 0 {
							// Auto-select first video PID
							targetPID = es.ElementaryPID
						} else if int(es.ElementaryPID) == o.ExtractPID {
							// User-specified PID
							targetPID = es.ElementaryPID
						}
					}
				}
			}
			pmtPID = int(d.PID)

			if targetPID == 0 {
				if o.ExtractPID == 0 {
					return fmt.Errorf("no video PID found in stream")
				}
				return fmt.Errorf("specified PID %d not found or not a video stream", o.ExtractPID)
			}
		}

		if pmtPID == -1 {
			continue
		}

		pes := d.PES
		if pes == nil || d.PID != targetPID {
			continue
		}

		codec := esKinds[d.PID]
		data := pes.Data

		// Check for parameter sets based on codec
		switch codec {
		case "AVC":
			if o.WaitForPS && !hasAVCPS {
				// Check if this PES contains SPS and PPS
				nalus := avc.ExtractNalusFromByteStream(data)
				hasSPS := false
				hasPPS := false
				for _, nalu := range nalus {
					naluType := avc.GetNaluType(nalu[0])
					if naluType == avc.NALU_SPS {
						hasSPS = true
					} else if naluType == avc.NALU_PPS {
						hasPPS = true
					}
				}
				if hasSPS && hasPPS {
					hasAVCPS = true
					extracting = true
				}
			} else if !o.WaitForPS {
				extracting = true
			}

		case "HEVC":
			if o.WaitForPS && !hasHEVCPS {
				// Check if this PES contains SPS and PPS (and optionally VPS)
				nalus := avc.ExtractNalusFromByteStream(data)
				hasSPS := false
				hasPPS := false
				for _, nalu := range nalus {
					naluType := hevc.GetNaluType(nalu[0])
					if naluType == hevc.NALU_SPS {
						hasSPS = true
					} else if naluType == hevc.NALU_PPS {
						hasPPS = true
					}
				}
				if hasSPS && hasPPS {
					hasHEVCPS = true
					extracting = true
				}
			} else if !o.WaitForPS {
				extracting = true
			}

		default:
			// For non-video codecs, start extracting immediately
			extracting = true
		}

		// Write PES payload to output if we're extracting
		if extracting {
			_, err := esWriter.Write(data)
			if err != nil {
				return fmt.Errorf("writing elementary stream data: %w", err)
			}
		}
	}

	if !extracting {
		return fmt.Errorf("no parameter sets found in stream, extraction did not start")
	}

	return jp.Error()
}
