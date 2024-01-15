package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Eyevinn/mp2ts-tools/cmd/ts-info/app"
	"github.com/Eyevinn/mp2ts-tools/internal"
)

func parseOptions() app.Options {
	opts := app.Options{}
	flag.IntVar(&opts.MaxNrPictures, "max", 0, "max nr pictures to parse")
	flag.BoolVar(&opts.ParameterSets, "ps", false, "print parameter sets")
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

var usg = `Usage of %s:

%s lists information about TS files especially timestamp inside AVC pic_timing SEI NAL units.
It can also present AVC parameter sets in details if the -p option is used.
PTS (and DTS) timestamps are presented as well.
`

func main() {
	o := parseOptions()
	if o.Version {
		fmt.Printf("ts-info version %s\n", internal.GetVersion())
		os.Exit(0)
	}
	if len(flag.Args()) < 1 {
		flag.Usage()
		os.Exit(1)
	}
	inFile := flag.Args()[0]
	err := run(os.Stdout, o, inFile)
	if err != nil {
		log.Fatal(err)
	}
}

func run(w io.Writer, o app.Options, inFile string) error {
	// Create a cancellable context in case you want to stop reading packets/data any time you want
	ctx, cancel := context.WithCancel(context.Background())
	// Handle SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()

	var f io.Reader
	if inFile == "-" {
		f = os.Stdin
	} else {
		var err error
		fh, err := os.Open(inFile)
		if err != nil {
			log.Fatal(err)
		}
		f = fh
		defer fh.Close()
	}

	err := app.Parse(ctx, w, f, o)
	if err != nil {
		return err
	}
	return nil
}
