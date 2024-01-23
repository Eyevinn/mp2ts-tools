package internal

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
)

type Options struct {
	MaxNrPictures  int
	Version        bool
	Indent         bool
	ShowStreamInfo bool
	ShowService    bool
	ShowPS         bool
	VerbosePSInfo  bool
	ShowNALU       bool
	ShowSEI        bool
	ShowStatistics bool
}

func CreateFullOptions(max int) Options {
	return Options{MaxNrPictures: max, ShowStreamInfo: true, ShowService: true, ShowPS: true, ShowNALU: true, ShowSEI: true, ShowStatistics: true}
}

type OptionParseFunc func() Options
type RunableFunc func(ctx context.Context, w io.Writer, f io.Reader, o Options) error

func ParseParams(function OptionParseFunc) (o Options, inFile string) {
	o = function()
	if o.Version {
		fmt.Printf("ts-info version %s\n", GetVersion())
		os.Exit(0)
	}
	if len(flag.Args()) < 1 {
		flag.Usage()
		os.Exit(1)
	}
	inFile = flag.Args()[0]
	return o, inFile
}

func Execute(w io.Writer, o Options, inFile string, function RunableFunc) error {
	// Create a cancellable context in case you want to stop reading packets/data any time you want
	ctx, cancel := context.WithCancel(context.Background())
	// Handle SIGTERM signal
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT)
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

	err := function(ctx, w, f, o)
	if err != nil {
		return err
	}
	return nil
}
