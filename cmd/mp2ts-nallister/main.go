package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/Eyevinn/mp2ts-tools/avc"
	"github.com/Eyevinn/mp2ts-tools/common"
)

var usg = `Usage of %s:

%s generates a list of nalus with information about timestamps, rai, SEI etc.
`

func parseOptions() common.Options {
	opts := common.Options{ShowStreamInfo: true, ShowService: false, ShowPS: false, ShowNALU: true, ShowSEI: false, ShowStatistics: true}
	flag.IntVar(&opts.MaxNrPictures, "max", 0, "max nr pictures to parse")
	flag.BoolVar(&opts.ShowSEI, "sei", false, "print sei messages")
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

func printNALInfo(ctx context.Context, w io.Writer, f io.Reader, o common.Options) error {
	return avc.ParseAll(ctx, w, f, o)
}

func main() {
	o, inFile := common.ParseParams(parseOptions)
	err := common.Execute(os.Stdout, o, inFile, printNALInfo)
	if err != nil {
		log.Fatal(err)
	}
}
