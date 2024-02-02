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

%s filters out some chosen pids from the ts packet.
Drop nothing and list all PIDs if empty pids list is specified (by default).
However, PAT(0) and PMT must not be dropped.
`

func parseOptions() internal.Options {
	opts := internal.Options{ShowStreamInfo: true, Indent: true, FilterPids: true}
	flag.StringVar(&opts.PidsToDrop, "drop", "", "pids to drop in the PMT (split by space), e.g. \"256 257\"")
	flag.StringVar(&opts.OutPutTo, "output", "", "save the TS packets into the given file (filepath) or stdout (-)")
	flag.BoolVar(&opts.Indent, "indent", true, "indent JSON output")
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

func filter(ctx context.Context, w io.Writer, f io.Reader, o internal.Options) error {
	outPutToFile := o.OutPutTo != "-"
	var textOutput io.Writer
	var tsOutput io.Writer
	// If we output to ts files, print analysis to stdout
	if outPutToFile {
		// Remove existing output file
		if err := internal.RemoveFileIfExists(o.OutPutTo); err != nil {
			return err
		}
		file, err := internal.OpenFileAndAppend(o.OutPutTo)
		if err != nil {
			return err
		}
		tsOutput = file
		textOutput = w
		defer file.Close()
	} else { // If we output to stdout, print analysis to stderr
		tsOutput = w
		textOutput = os.Stderr
	}

	return internal.FilterPids(ctx, textOutput, tsOutput, f, o)
}

func main() {
	o, inFile := internal.ParseParams(parseOptions)
	err := internal.Execute(os.Stdout, o, inFile, filter)
	if err != nil {
		log.Fatal(err)
	}
}
