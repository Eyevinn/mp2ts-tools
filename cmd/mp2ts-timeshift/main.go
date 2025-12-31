package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/Comcast/gots/v2"
	"github.com/Comcast/gots/v2/packet"
	"github.com/Comcast/gots/v2/packet/adaptationfield"
	"github.com/Comcast/gots/v2/pes"
	"github.com/Eyevinn/mp2ts-tools/internal"
)

const (
	ptsWrap = 1 << 33
	pcrWrap = ptsWrap * 300
)

var usg = `Usage of %s:

%s shifts all PTS/DTS/PCR_base values in a transport stream by a specified offset.
The main use-case is to generate TS files with timestamp wrap-around for testing purposes.

The offset is specified in 90kHz units (same as PTS/DTS).
PTS/DTS values are 33-bit and PCR_base is 42-bit (27MHz, derived as offset * 300).
`

type Options struct {
	Offset  int64
	Output  string
	Version bool
}

func parseOptions() Options {
	var opts Options
	flag.Int64Var(&opts.Offset, "offset", 0, "timestamp offset in 90kHz units (can be negative)")
	flag.StringVar(&opts.Output, "output", "-", "output file (- for stdout)")
	flag.BoolVar(&opts.Version, "version", false, "print version")

	flag.Usage = func() {
		parts := strings.Split(os.Args[0], "/")
		name := parts[len(parts)-1]
		fmt.Fprintf(os.Stderr, usg, name, name)
		fmt.Fprintf(os.Stderr, "\nRun as: %s [options] file.ts (- for stdin) with options:\n\n", name)
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  %s -offset 8589934592 input.ts -output output.ts  # shift by 2^33 to cause wrap-around\n", name)
		fmt.Fprintf(os.Stderr, "  %s -offset -9000000 input.ts > output.ts          # shift back by 100 seconds\n", name)
	}

	flag.Parse()
	return opts
}

// rewritePCR shifts the PCR value in packet by the input offset
func rewritePCR(pkt *packet.Packet, offset int64) {
	pcrBytes, err := adaptationfield.PCR(pkt)
	if err != nil {
		return
	}
	pcr := int64(gots.ExtractPCR(pcrBytes))

	// Convert offset from 90kHz to 27MHz (multiply by 300)
	pcrOffset := offset * 300

	// Apply offset with wrap-around
	newPcr := pcr + pcrOffset
	newPcr = newPcr % pcrWrap
	if newPcr < 0 {
		newPcr += pcrWrap
	}

	gots.InsertPCR(pcrBytes, uint64(newPcr))
}

// rewriteTimestamps modifies PTS and DTS in a PES header
func rewriteTimestamps(pesHeaderBytes []byte, offset int64) error {
	pesHeader, err := pes.NewPESHeader(pesHeaderBytes)
	if err != nil {
		return err
	}

	if !pesHeader.HasPTS() {
		return nil
	}

	// Rewrite PTS
	pts := int64(pesHeader.PTS())
	newPTS := (pts + offset) % ptsWrap
	if newPTS < 0 {
		newPTS += ptsWrap
	}
	gots.InsertPTS(pesHeaderBytes[9:14], uint64(newPTS))

	// Rewrite DTS if present
	if pesHeader.HasDTS() {
		pesHeaderBytes[9] = 0x30 | pesHeaderBytes[9]&0x0f // set first 4 bits to 0011
		dts := int64(pesHeader.DTS())
		newDTS := (dts + offset) % ptsWrap
		if newDTS < 0 {
			newDTS += ptsWrap
		}
		gots.InsertPTS(pesHeaderBytes[14:19], uint64(newDTS))
		pesHeaderBytes[14] = 0x10 | pesHeaderBytes[14]&0x0f // set first 4 bits to 0001
	}

	return nil
}

func timeshift(ctx context.Context, reader io.Reader, writer io.Writer, opts Options) error {
	bufReader := bufio.NewReader(reader)

	// Sync to first packet
	_, err := packet.Sync(bufReader)
	if err != nil {
		return fmt.Errorf("syncing with reader: %w", err)
	}

	var pkt packet.Packet
	packetCount := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Read packet
		if _, err := io.ReadFull(bufReader, pkt[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return fmt.Errorf("reading packet: %w", err)
		}

		packetCount++

		// Rewrite PCR if present
		if packet.ContainsAdaptationField(&pkt) && adaptationfield.Length(&pkt) > 0 && adaptationfield.HasPCR(&pkt) {
			rewritePCR(&pkt, opts.Offset)
		}

		// Rewrite PTS/DTS if this is a PES packet
		if packet.PayloadUnitStartIndicator(&pkt) {
			pesHeaderBytes, err := packet.PESHeader(&pkt)
			if err == nil && pesHeaderBytes != nil {
				if err := rewriteTimestamps(pesHeaderBytes, opts.Offset); err != nil {
					log.Printf("Warning: failed to rewrite timestamps in packet %d: %v", packetCount, err)
				}
			}
		}

		// Write modified packet
		if _, err := writer.Write(pkt[:]); err != nil {
			return fmt.Errorf("writing packet: %w", err)
		}
	}

	log.Printf("Processed %d packets with offset %d (90kHz units)", packetCount, opts.Offset)
	return nil
}

func main() {
	opts := parseOptions()

	if opts.Version {
		fmt.Printf("mp2ts-timeshift version %s\n", internal.GetVersion())
		os.Exit(0)
	}

	if len(flag.Args()) < 1 {
		flag.Usage()
		os.Exit(1)
	}

	inFile := flag.Args()[0]

	// Create context for cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Open input file
	var reader io.Reader
	if inFile == "-" {
		reader = os.Stdin
	} else {
		fh, err := os.Open(inFile)
		if err != nil {
			log.Fatal(err)
		}
		defer func() { _ = fh.Close() }()
		reader = fh
	}

	// Open output file
	var writer io.Writer
	if opts.Output == "-" {
		writer = os.Stdout
	} else {
		outputFile, err := os.Create(opts.Output)
		if err != nil {
			log.Fatalf("creating output file: %v", err)
		}
		defer func() { _ = outputFile.Close() }()
		bufWriter := bufio.NewWriter(outputFile)
		defer func() { _ = bufWriter.Flush() }()
		writer = bufWriter
	}

	// Process the stream
	if err := timeshift(ctx, reader, writer, opts); err != nil {
		log.Fatal(err)
	}
}
