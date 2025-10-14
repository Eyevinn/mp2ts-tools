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

%s extracts elementary video streams (PES payloads) from MPEG-2 Transport Stream files.
By default, it waits for parameter sets (VPS/SPS/PPS) before starting extraction.
`

func parseOptions() internal.Options {
	opts := internal.Options{ShowStreamInfo: true, Indent: false, WaitForPS: true}
	flag.IntVar(&opts.ExtractPID, "pid", 0, "PID to extract (if 0, extract first video PID found)")
	flag.StringVar(&opts.OutPutTo, "output", "", "output file path (- for stdout, required)")
	flag.BoolVar(&opts.WaitForPS, "waitps", true, "wait for parameter sets (VPS/SPS/PPS) before extraction")
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

func extract(ctx context.Context, w io.Writer, f io.Reader, o internal.Options) error {
	if o.OutPutTo == "" {
		return fmt.Errorf("output file path is required (use -output)")
	}

	outPutToFile := o.OutPutTo != "-"
	var textOutput io.Writer
	var esOutput io.Writer

	// If we output to file, print info to stdout
	if outPutToFile {
		// Remove existing output file
		if err := internal.RemoveFileIfExists(o.OutPutTo); err != nil {
			return err
		}
		file, err := internal.OpenFileAndAppend(o.OutPutTo)
		if err != nil {
			return err
		}
		esOutput = file
		textOutput = w
		defer file.Close()
	} else { // If we output to stdout, print info to stderr
		esOutput = w
		textOutput = os.Stderr
	}

	return internal.ExtractES(ctx, textOutput, esOutput, f, o)
}

func main() {
	o, inFile := internal.ParseParams(parseOptions)
	err := internal.Execute(os.Stdout, o, inFile, extract)
	if err != nil {
		log.Fatal(err)
	}
}
