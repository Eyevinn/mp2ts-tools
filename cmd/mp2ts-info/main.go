package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/Eyevinn/mp2ts-tools/common"
	"github.com/asticode/go-astits"
)

var usg = `Usage of %s:

%s lists information about TS files, e.g. pids, bitrates, service, etc
`

func parseOptions() common.Options {
	opts := common.Options{ShowStreamInfo: true}
	flag.BoolVar(&opts.ShowService, "service", false, "show service information")
	flag.BoolVar(&opts.Indent, "indent", false, "indent JSON output")
	flag.BoolVar(&opts.Version, "version", false, "print version")

	flag.Usage = func() {
		parts := strings.Split(os.Args[0], "/")
		name := parts[len(parts)-1]
		fmt.Fprintf(os.Stderr, usg, name, name)
		fmt.Fprintf(os.Stderr, "\nRun as: %s [options] file.ts (- for stdin) with options:\n\n", name)
		flag.PrintDefaults()
	}

	flag.Parse()
	return opts
}

func ParseInfo(ctx context.Context, w io.Writer, f io.Reader, o common.Options) error {
	rd := bufio.NewReaderSize(f, 1000*common.PacketSize)
	dmx := astits.NewDemuxer(ctx, rd)
	pmtPID := -1
	sdtPrinted := false
	jp := &common.JsonPrinter{W: w, Indent: o.Indent}
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
				var streamInfo *common.ElementaryStreamInfo
				switch es.StreamType {
				case astits.StreamTypeH264Video:
					streamInfo = &common.ElementaryStreamInfo{PID: es.ElementaryPID, Codec: "AVC", Type: "video"}
				case astits.StreamTypeAACAudio:
					streamInfo = &common.ElementaryStreamInfo{PID: es.ElementaryPID, Codec: "AAC", Type: "audio"}
				case astits.StreamTypeH265Video:
					streamInfo = &common.ElementaryStreamInfo{PID: es.ElementaryPID, Codec: "HEVC", Type: "video"}
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

		// Exit imediately if we don't want service information
		if !o.ShowService {
			break dataLoop
		}

		// Print service information
		if d.SDT != nil && !sdtPrinted {
			jp.PrintSdtInfo(d.SDT, o.ShowService)
			sdtPrinted = true
			break dataLoop
		}

		// Loop until we have printed service information
	}

	return jp.Error()
}

func main() {
	o, inFile := common.ParseParams(parseOptions)
	err := common.Execute(os.Stdout, o, inFile, ParseInfo)
	if err != nil {
		log.Fatal(err)
	}
}
