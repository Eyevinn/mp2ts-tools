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
	flag.BoolVar(&opts.ShowSCTE35, "scte35", true, "show SCTE35 information")
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

func parse(ctx context.Context, w io.Writer, f io.Reader, o internal.Options) error {
	// Parse either general information, or scte35 (by default)
	if o.ShowService {
		err := internal.ParseInfo(ctx, w, f, o)
		if err != nil {
			return err
		}
	} else if o.ShowSCTE35 {
		err := internal.ParseSCTE35(ctx, w, f, o)
		if err != nil {
			return err
		}
	}

	return nil
}

func main() {
	o, inFile := internal.ParseParams(parseOptions)
	err := internal.Execute(os.Stdout, o, inFile, parse)
	if err != nil {
		log.Fatal(err)
	}
}
