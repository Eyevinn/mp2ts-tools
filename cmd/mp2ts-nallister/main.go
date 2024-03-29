package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/Eyevinn/mp2ts-tools/internal"
)

var usg = `Usage of %s:

%s generates a list of AVC/HEVC nalus with information about timestamps, rai, SEI etc.
It can further be used to generate a list of SMPTE-2038 data.
`

func parseOptions() internal.Options {
	opts := internal.Options{ShowStreamInfo: true, ShowService: false, ShowPS: false, ShowNALU: true, ShowSEIDetails: false, ShowStatistics: true}
	flag.IntVar(&opts.MaxNrPictures, "max", 0, "max nr pictures to parse")
	flag.BoolVar(&opts.ShowSEIDetails, "sei", false, "print detailed sei message information")
	flag.BoolVar(&opts.ShowSMPTE2038, "smpte2038", false, "print details about SMPTE-2038 data")
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

func parseNALUInfo(ctx context.Context, w io.Writer, f io.Reader, o internal.Options) error {
	return internal.ParseAll(ctx, w, f, o)
}

func main() {
	o, inFile := internal.ParseParams(parseOptions)
	err := internal.Execute(os.Stdout, o, inFile, parseNALUInfo)
	if err != nil {
		log.Fatal(err)
	}
}
