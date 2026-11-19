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

%s makes a perfect endless loop of a single-program MPEG-2 TS, rewriting as
little as possible.

The loop runs from one video access point (IDR, CRA, an MPEG-2 I picture with a
sequence header, or an AVC/HEVC recovery point) to a later one, preferring a loop
that keeps the picture numbering, a DTS that steps evenly across the seam, a
whole number of frames, audio from its start, and then the longest. Each lap is
the source packets of that window: continuity counters, PCR and PES timestamps
are advanced by the loop duration, and partial PES at the window's edges are
removed. Audio moves across the seam so each lap plays the audio of its video,
up to a frame later and never earlier, without drift. A constant-rate stream
gets null packets added or removed just before the seam and an exactly linear
PCR; a variable-rate stream keeps its source timing. An open-GOP or recovery
point loop point is marked as an edit (HEVC CRA becomes BLA, MPEG-2 gets
broken_link, a recovery point SEI gets broken_link_flag). SCTE-35 cues whose
whole break is inside the loop are kept, with pts_adjustment and event IDs
advanced each lap.

The supported codecs are H.264, H.265 and MPEG-2 video, and AAC (ADTS), MPEG
audio, AC-3 and E-AC-3, cut on frame boundaries; other audio (LATM, S302M, AC-4)
is cut on PES boundaries.
`

// Options holds the parsed command-line configuration.
type Options struct {
	Input         string             // input TS file (positional), "-" for stdin
	OutFile       string             // -o output file, "-" for stdout
	UDPAddr       string             // -a output UDP address host:port
	TTL           int                // -ttl multicast TTL
	DurationMS    int                // -d longest loop in ms (0 = no limit)
	ScanOnly      bool               // -scan
	WritePrepared string             // -write-prepared path for one lap
	FPS           internal.FrameRate // -fps frame rate (zero = detect)
	Laps          int                // -m laps (0 = endless)
	Indent        bool               // -indent JSON output
	Quiet         bool               // -q
	Version       bool               // -version
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
		fmt.Fprintf(os.Stderr, "  %s -scan input.ts                      # the analysis and chosen loop as JSON\n", name)
		fmt.Fprintf(os.Stderr, "  %s -a 239.0.0.1:1234 input.ts          # endless, over UDP, paced by the output PCR\n", name)
		fmt.Fprintf(os.Stderr, "  %s -m 3 -o out.ts input.ts             # three laps to a file\n", name)
		fmt.Fprintf(os.Stderr, "  %s -m 3 -o - input.ts | ffplay -       # three laps to stdout\n", name)
	}
	return fs
}

func parseOptions(args []string) (Options, error) {
	var opts Options
	var fpsStr string

	fs := newFlagSet()
	fs.StringVar(&opts.OutFile, "o", "", `output file ("-" for stdout)`)
	fs.StringVar(&opts.UDPAddr, "a", "", "output UDP address host:port (unicast or multicast), paced by the output PCR")
	fs.IntVar(&opts.TTL, "ttl", 1, "multicast TTL for -a")
	fs.IntVar(&opts.DurationMS, "d", 0, "longest loop to consider, in ms (0 = no limit)")
	fs.BoolVar(&opts.ScanOnly, "scan", false, "print the analysis and chosen loop as JSON and exit")
	fs.StringVar(&opts.WritePrepared, "write-prepared", "", "also write one lap to this file (debug)")
	fs.StringVar(&fpsStr, "fps", "", "frame rate when it cannot be detected: 25, 29.97, 30000/1001, ...")
	fs.IntVar(&opts.Laps, "m", -1, "number of laps (default: 1 for file output only, endless with -a)")
	fs.BoolVar(&opts.Indent, "indent", false, "indent JSON output")
	fs.BoolVar(&opts.Quiet, "q", false, "no report on stderr")
	fs.BoolVar(&opts.Version, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if opts.Version {
		return opts, nil
	}
	if fpsStr != "" {
		fr, err := internal.ParseFrameRate(fpsStr)
		if err != nil {
			return opts, err
		}
		opts.FPS = fr
	}
	if opts.TTL < 0 || opts.TTL > 255 {
		return opts, fmt.Errorf("bad -ttl %d", opts.TTL)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fs.Usage()
		return opts, errors.New("give exactly one input TS file")
	}
	opts.Input = rest[0]
	if !opts.ScanOnly && opts.OutFile == "" && opts.UDPAddr == "" && opts.WritePrepared == "" {
		return opts, errors.New("no output: use -o (file), -a (UDP), -write-prepared, or -scan")
	}
	switch {
	case opts.Laps == 0 || opts.Laps < -1:
		return opts, fmt.Errorf("bad -m %d", opts.Laps)
	case opts.Laps > 0:
	case opts.UDPAddr != "" && opts.OutFile != "":
		// A file written alongside an endless UDP output would grow without limit.
		return opts, errors.New("file output with -a requires -m <laps>")
	case opts.UDPAddr != "":
		opts.Laps = 0
	default:
		opts.Laps = 1
	}
	return opts, nil
}

func printJSON(v any, indent bool) error {
	var b []byte
	var err error
	if indent {
		b, err = json.MarshalIndent(v, "", "  ")
	} else {
		b, err = json.Marshal(v)
	}
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func printSummary(rep *internal.ScanReport) {
	v, lp, lap := rep.Video, rep.Loop, rep.Lap
	structure := ""
	if v.FieldPictures > 0 {
		structure += fmt.Sprintf(", %d field pictures", v.FieldPictures)
	}
	if v.FieldOrder != "" {
		structure += ", " + v.FieldOrder
	}
	if v.PulldownPictures > 0 {
		structure += fmt.Sprintf(", %d repeating fields or frames", v.PulldownPictures)
	}
	fmt.Fprintf(os.Stderr, "video PID %d %s, %d pictures%s; %d audio stream(s)\n", v.PID, v.Codec, v.Pictures,
		structure, len(rep.Audio))
	frames := strconv.FormatFloat(lp.Frames, 'f', -1, 64) + " frames"
	if lp.Frames != float64(lp.Pictures) {
		frames += fmt.Sprintf(" in %d pictures", lp.Pictures)
	}
	whole := ""
	if !lp.WholeFrames {
		whole = ", not whole frames"
	}
	fmt.Fprintf(os.Stderr, "loop: %s %.0f ms (%s at %s fps, %d GOPs%s), packets %d..%d\n",
		lp.AccessPoint, lp.DurationMS, frames, lp.FrameRate, lp.GOPs, whole, lp.StartPacket, lp.EndPacket)
	rate := "variable"
	if lp.ConstantRate {
		rate = "constant"
	}
	fmt.Fprintf(os.Stderr, "rate: %s %d bit/s; lap %d packets (%d stripped, %d nulls removed, %d added, "+
		"%d with an added PCR, %d seam marks)\n", rate, lp.Bitrate, lap.Packets, lap.StrippedPackets,
		lap.RemovedNulls, lap.AddedNulls, lap.AddedPCRPackets, lap.SeamMarks)
	for i, a := range lp.Audio {
		unit := "frame"
		if rep.Audio[i].CutAt == "pes" {
			unit = "PES"
		}
		fmt.Fprintf(os.Stderr, "audio PID %d %s: %d %ss from %+.1f ms of the loop start, %+.1f ms beyond the loop\n",
			a.PID, rep.Audio[i].Codec, a.Units, unit, a.SeamOffsetMS, a.ExcessMS)
	}
	for _, w := range rep.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
}

// fileOutput opens a buffered file (or stdout) output.
func fileOutput(path string) (internal.LapOutput, func() error, error) {
	var w io.WriteCloser = os.Stdout
	if path != "-" {
		f, err := os.Create(path)
		if err != nil {
			return nil, nil, err
		}
		w = f
	}
	bw := bufio.NewWriterSize(w, 1000*internal.PacketSize)
	closeFn := func() error {
		err := bw.Flush()
		if w != os.Stdout {
			if cerr := w.Close(); err == nil {
				err = cerr
			}
		}
		return err
	}
	return internal.WriterOutput{W: bw}, closeFn, nil
}

func run(ctx context.Context, opts Options) error {
	loop, err := internal.PrepareLoop(ctx, opts.Input,
		internal.LoopOptions{FrameRate: opts.FPS, MaxDurationMS: opts.DurationMS})
	if opts.ScanOnly && loop != nil {
		if perr := printJSON(loop.Report, opts.Indent); perr != nil {
			return perr
		}
	}
	if err != nil || opts.ScanOnly {
		return err
	}
	if !opts.Quiet {
		printSummary(loop.Report)
	}

	if opts.WritePrepared != "" {
		out, closeFn, err := fileOutput(opts.WritePrepared)
		if err != nil {
			return err
		}
		_, err = loop.WriteLaps(ctx, []internal.LapOutput{out}, 1)
		if cerr := closeFn(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if !opts.Quiet {
			fmt.Fprintf(os.Stderr, "wrote one lap to %s\n", opts.WritePrepared)
		}
	}
	if opts.OutFile == "" && opts.UDPAddr == "" {
		return nil
	}

	var outputs []internal.LapOutput
	closeFile := func() error { return nil }
	if opts.OutFile != "" {
		out, closeFn, err := fileOutput(opts.OutFile)
		if err != nil {
			return err
		}
		outputs, closeFile = append(outputs, out), closeFn
	}
	if opts.UDPAddr != "" {
		u, err := internal.NewUDPOutput(opts.UDPAddr, opts.TTL)
		if err != nil {
			_ = closeFile()
			return err
		}
		defer func() { _ = u.Close() }()
		outputs = append(outputs, u)
		if !opts.Quiet {
			bitrate := float64(loop.LapPackets()) * internal.PacketSize * 8 * 90_000 / float64(loop.LoopDurationTicks())
			fmt.Fprintf(os.Stderr, "streaming to udp://%s at %.0f bit/s (Ctrl-C to stop)\n", opts.UDPAddr, bitrate)
		}
	}
	stats, runErr := loop.WriteLaps(ctx, outputs, max(opts.Laps, 0))
	if cerr := closeFile(); runErr == nil {
		runErr = cerr
	}
	if runErr == nil && !opts.Quiet {
		fmt.Fprintf(os.Stderr, "%d audio packets moved across seams (%d needed an extra packet), %d audio gaps, "+
			"%d SCTE-35 sections advanced\n", stats.MovedAudioPackets, stats.InsertedAudioPackets, stats.AudioGaps,
			stats.SCTE35Patched)
	}
	return runErr
}

func main() {
	opts, err := parseOptions(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	if opts.Version {
		fmt.Printf("mp2ts-loop version %s\n", internal.Version())
		os.Exit(0)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, opts); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
