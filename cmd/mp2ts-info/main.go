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

%s lists information about TS files, e.g. pids, bitrates, service, etc
`

func parseOptions() internal.Options {
	opts := internal.Options{ShowStreamInfo: true, Indent: true}
	flag.BoolVar(&opts.ShowService, "service", false, "show service information")
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

func parseInfo(ctx context.Context, w io.Writer, f io.Reader, o internal.Options) error {
	return internal.ParseInfo(ctx, w, f, o)
}

func main() {
	o, inFile := internal.ParseParams(parseOptions)
	err := internal.Execute(os.Stdout, o, inFile, parseInfo)
	if err != nil {
		log.Fatal(err)
	}
}
