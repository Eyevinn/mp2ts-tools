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
	"github.com/Eyevinn/mp2ts-tools/internal/avc"
)

var usg = `Usage of %s:

%s lists parameter sets in TS files
`

func parseOptions() internal.Options {
	opts := internal.Options{ShowStreamInfo: true, ShowService: false, ShowPS: true, ShowNALU: false, ShowSEI: false, ShowStatistics: false}
	flag.IntVar(&opts.MaxNrPictures, "max", 0, "max nr pictures to parse")
	flag.BoolVar(&opts.VerbosePSInfo, "ps", false, "show verbose information")
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

func parsePSInfo(ctx context.Context, w io.Writer, f io.Reader, o internal.Options) error {
	return avc.ParseAll(ctx, w, f, o)
}

func main() {
	o, inFile := internal.ParseParams(parseOptions)
	err := internal.Execute(os.Stdout, o, inFile, parsePSInfo)
	if err != nil {
		log.Fatal(err)
	}
}
