package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/Eyevinn/mp2ts-tools/internal"
)

var usg = `Usage of %s:

%s loops a single-program MPEG-2 TS file seamlessly and forever.

It scans the input for the longest loop bounded by a video random-access point
(an IDR picture, a CRA when the stream has no IDR, or an MPEG-2 I picture with a
sequence header), keeping the video at a constant frame rate and constant GOP
duration. Audio is matched to the average
loop duration with no accumulated drift, and PCR is kept perfect for a
constant-rate TS. The output can be written to a file or sent over UDP.

The input must be a seekable file (stdin is not supported, since the tool reads
it more than once), and every audio and video PES must carry a PTS. The supported codecs are H.264, H.265, and MPEG-2 video and
AAC, MP2, and AC-3 audio.
`

// FrameRate is a rational frame rate num/den (frames per second = num/den).
// den is 1 for integer rates and 1001 for the NTSC-derived fractional rates.
type FrameRate struct {
	Num int
	Den int
}

func (fr FrameRate) IsZero() bool { return fr.Num == 0 }

func (fr FrameRate) String() string {
	if fr.Den == 1 {
		return strconv.Itoa(fr.Num)
	}
	return fmt.Sprintf("%d/%d", fr.Num, fr.Den)
}

// Options holds the parsed command-line configuration.
type Options struct {
	Input         string    // input TS file (positional)
	OutFile       string    // -o output file
	UDPAddr       string    // -a output UDP address ip:port
	DurationMS    int       // -d target/cap loop duration in ms (0 = auto longest)
	ScanOnly      bool      // -scan
	WritePrepared string    // -write-prepared path for the single-loop segment
	FPS           FrameRate // -fps frame-rate hint (empty = auto-detect)
	MaxWraps      int       // -m max wraps (-1 = infinite)
	Indent        bool      // -indent JSON output
	Version       bool      // -version
}

// parseFrameRate parses an -fps value such as "25", "50", "30", "60",
// "29.97", "59.94", "23.976", or an explicit "num/den".
func parseFrameRate(s string) (FrameRate, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return FrameRate{}, nil
	}
	if num, den, ok := strings.Cut(s, "/"); ok {
		n, err1 := strconv.Atoi(strings.TrimSpace(num))
		d, err2 := strconv.Atoi(strings.TrimSpace(den))
		if err1 != nil || err2 != nil || d == 0 {
			return FrameRate{}, fmt.Errorf("invalid frame rate %q", s)
		}
		return FrameRate{Num: n, Den: d}, nil
	}
	// Named/decimal rates. The NTSC-derived rates map to exact num/1001 rationals.
	switch s {
	case "23.976", "23.98":
		return FrameRate{24000, 1001}, nil
	case "29.97":
		return FrameRate{30000, 1001}, nil
	case "59.94":
		return FrameRate{60000, 1001}, nil
	case "119.88":
		return FrameRate{120000, 1001}, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return FrameRate{Num: n, Den: 1}, nil
	}
	return FrameRate{}, fmt.Errorf("invalid frame rate %q", s)
}

// newFlagSet builds a flag set whose usage prints the program description.
func newFlagSet() *flag.FlagSet {
	parts := strings.Split(os.Args[0], "/")
	name := parts[len(parts)-1]
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, usg, name, name)
		fmt.Fprintf(os.Stderr, "\nRun as: %s [options] file.ts with options:\n\n", name)
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  %s -scan input.ts                      # report loop-point candidates\n", name)
		fmt.Fprintf(os.Stderr, "  %s -a 239.0.0.1:1234 input.ts          # loop to multicast UDP\n", name)
		fmt.Fprintf(os.Stderr, "  %s -m 3 -o out.ts input.ts             # write 3 loops to a file\n", name)
	}
	return fs
}

func parseOptions(args []string) (Options, error) {
	var opts Options
	var fpsStr string

	fs := newFlagSet()
	fs.StringVar(&opts.OutFile, "o", "", "output file path (file output)")
	fs.StringVar(&opts.UDPAddr, "a", "", "output UDP address ip:port (unicast or multicast)")
	fs.IntVar(&opts.DurationMS, "d", 0, "target/maximum loop duration in ms (0 = auto, longest possible)")
	fs.BoolVar(&opts.ScanOnly, "scan", false, "scan only: print loop-point analysis as JSON and exit")
	fs.StringVar(&opts.WritePrepared, "write-prepared", "", "also write the prepared single-loop TS to this file (debug)")
	fs.StringVar(&fpsStr, "fps", "", "frame-rate hint, e.g. 25, 50, 29.97, 59.94, 23.976, or num/den (default: auto)")
	fs.IntVar(&opts.MaxWraps, "m", -1, "maximum number of loop wraps (-1 = infinite)")
	fs.BoolVar(&opts.Indent, "indent", false, "indent JSON output")
	fs.BoolVar(&opts.Version, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if opts.Version {
		return opts, nil
	}

	fr, err := parseFrameRate(fpsStr)
	if err != nil {
		return opts, err
	}
	opts.FPS = fr

	rest := fs.Args()
	if len(rest) < 1 {
		fs.Usage()
		return opts, errors.New("missing input TS file")
	}
	opts.Input = rest[0]
	return opts, nil
}

// run validates options and dispatches to the scan or loop pipeline.
func run(ctx context.Context, opts Options) error {
	if opts.Input == "-" {
		return errors.New("stdin is not supported; the input must be a seekable file")
	}
	if !opts.ScanOnly && opts.OutFile == "" && opts.UDPAddr == "" && opts.WritePrepared == "" {
		return errors.New("no output selected: use -o (file), -a (UDP), -write-prepared, or -scan")
	}

	if opts.ScanOnly {
		return scan(ctx, opts)
	}
	return prepareAndLoop(ctx, opts)
}

// scan runs the analysis pass and reports loop-point candidates and stream
// statistics as JSON.
func scan(ctx context.Context, opts Options) error {
	rep, err := internal.Scan(ctx, opts.Input, opts.FPS.Num, opts.FPS.Den, opts.DurationMS)
	if err != nil {
		return err
	}
	var b []byte
	if opts.Indent {
		b, err = json.MarshalIndent(rep, "", "  ")
	} else {
		b, err = json.Marshal(rep)
	}
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// prepareAndLoop scans, prepares the in-memory loopable segment, and loops it to
// the selected output(s).
func prepareAndLoop(ctx context.Context, opts Options) error {
	seg, plan, err := internal.PrepareLoop(ctx, opts.Input, opts.FPS.Num, opts.FPS.Den, opts.DurationMS)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "loop: %s, %d GOPs, %.0f ms, %d packets/wrap\n",
		plan.LoopPointType, plan.NumGOPs, plan.LoopDurMS, seg.NumPackets)
	for _, wmsg := range plan.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", wmsg)
	}

	if opts.WritePrepared != "" {
		f, err := os.Create(opts.WritePrepared)
		if err != nil {
			return err
		}
		bw := bufio.NewWriterSize(f, 1000*internal.PacketSize)
		if err := internal.LoopToWriter(ctx, seg, bw, 1); err != nil {
			_ = f.Close()
			return err
		}
		if err := bw.Flush(); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote prepared single-loop segment to %s\n", opts.WritePrepared)
	}

	if opts.OutFile == "" && opts.UDPAddr == "" {
		return nil // -write-prepared only
	}
	// A bounded (-m) run is required whenever a file is written, so it does not
	// grow without limit. UDP-only output may loop forever.
	if opts.OutFile != "" && opts.MaxWraps <= 0 {
		return errors.New("file output requires -m <wraps> (a finite number of loops)")
	}

	var fileW io.Writer
	var closeFile = func() error { return nil }
	if opts.OutFile != "" {
		f, err := os.Create(opts.OutFile)
		if err != nil {
			return err
		}
		bw := bufio.NewWriterSize(f, 1000*internal.PacketSize)
		fileW = bw
		closeFile = func() error {
			if ferr := bw.Flush(); ferr != nil {
				_ = f.Close()
				return ferr
			}
			return f.Close()
		}
	}
	if opts.UDPAddr != "" {
		bitrate := float64(seg.NumPackets) * internal.PacketSize * 8 / plan.LoopDurMS * 1000
		fmt.Fprintf(os.Stderr, "streaming to udp://%s at %.0f bps (Ctrl-C to stop)\n", opts.UDPAddr, bitrate)
	}

	runErr := internal.LoopToOutputs(ctx, seg, fileW, opts.UDPAddr, opts.MaxWraps)
	if cerr := closeFile(); runErr == nil {
		runErr = cerr
	}
	return runErr
}

func main() {
	opts, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	if opts.Version {
		fmt.Printf("mp2ts-loop version %s\n", internal.GetVersion())
		os.Exit(0)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, opts); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
