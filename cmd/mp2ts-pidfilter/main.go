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

%s filter out some irrelevant pids from the PMT.
Filter nothing and save the original packets if empty pids list is given (by default).
`

func parseOptions() internal.Options {
	opts := internal.Options{ShowStreamInfo: true, Indent: true, FilterPids: true}
	flag.StringVar(&opts.PidsToKeep, "keep", "", "pids to keep in the PMT")
	flag.StringVar(&opts.OutputFile, "output", "", "path of the output file")
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
	return internal.FilterPids(ctx, w, f, o)
}

func main() {
	o, inFile := internal.ParseParams(parseOptions)
	err := internal.Execute(os.Stdout, o, inFile, filter)
	if err != nil {
		log.Fatal(err)
	}
}
