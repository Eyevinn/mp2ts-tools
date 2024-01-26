package internal

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/Comcast/gots/v2"
	"github.com/Comcast/gots/v2/packet"
	"github.com/Comcast/gots/v2/psi"
	"github.com/asticode/go-astits"
	slices "golang.org/x/exp/slices"
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
	ShowSEIDetails bool
	ShowSCTE35     bool
	ShowStatistics bool
	FilterPids     bool
	PidsToDrop     string
	OutPutTo       string
}

func CreateFullOptions(max int) Options {
	return Options{MaxNrPictures: max, ShowStreamInfo: true, ShowService: true, ShowPS: true, ShowNALU: true, ShowSEIDetails: true, ShowStatistics: true}
}

type OptionParseFunc func() Options
type RunableFunc func(ctx context.Context, w io.Writer, f io.Reader, o Options) error

func ReadPMTPackets(r io.Reader, pid int) ([]packet.Packet, psi.PMT, error) {
	packets := []packet.Packet{}
	var pkt = &packet.Packet{}
	var err error
	var pmt psi.PMT

	pmtAcc := packet.NewAccumulator(psi.PmtAccumulatorDoneFunc)
	done := false

	for !done {
		if _, err := io.ReadFull(r, pkt[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil, nil, gots.ErrPMTNotFound
			}
			return nil, nil, err
		}
		currPid := pkt.PID()
		if currPid != pid {
			continue
		}
		packets = append(packets, *pkt)

		_, err = pmtAcc.WritePacket(pkt)
		if err == gots.ErrAccumulatorDone {
			pmt, err = psi.NewPMT(pmtAcc.Bytes())
			if err != nil {
				return nil, nil, err
			}
			if len(pmt.Pids()) == 0 {
				done = false
				pmtAcc = packet.NewAccumulator(psi.PmtAccumulatorDoneFunc)
				continue
			}
			done = true
		} else if err != nil {
			return nil, nil, err
		}
	}

	return packets, pmt, nil
}

func WritePacket(pkt *packet.Packet, w io.Writer) error {
	_, err := w.Write(pkt[:])
	return err
}

func RemoveExistingFile(file string) error {
	// Remove the file if it exists
	_, err := os.Stat(file)
	if err != nil {
		return err
	}

	err = os.Remove(file)
	if err != nil {
		return err
	}

	return nil
}

func OpenFileAndAppend(file string) (*os.File, error) {
	// Create and append to the new file
	fo, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("creating output file %w", err)
	}

	return fo, nil
}

func ParseAstitsElementaryStreamInfo(es *astits.PMTElementaryStream) *ElementaryStreamInfo {
	var streamInfo *ElementaryStreamInfo
	switch es.StreamType {
	case astits.StreamTypeH264Video:
		streamInfo = &ElementaryStreamInfo{PID: es.ElementaryPID, Codec: "AVC", Type: "video"}
	case astits.StreamTypeAACAudio:
		streamInfo = &ElementaryStreamInfo{PID: es.ElementaryPID, Codec: "AAC", Type: "audio"}
	case astits.StreamTypeH265Video:
		streamInfo = &ElementaryStreamInfo{PID: es.ElementaryPID, Codec: "HEVC", Type: "video"}
	case astits.StreamTypeSCTE35:
		streamInfo = &ElementaryStreamInfo{PID: es.ElementaryPID, Codec: "SCTE35", Type: "cue"}
	}

	return streamInfo
}

func ParseElementaryStreamInfo(es psi.PmtElementaryStream) *ElementaryStreamInfo {
	pid := uint16(es.ElementaryPid())
	var streamInfo *ElementaryStreamInfo
	switch es.StreamType() {
	case psi.PmtStreamTypeMpeg4VideoH264:
		streamInfo = &ElementaryStreamInfo{PID: pid, Codec: "AVC", Type: "video"}
	case psi.PmtStreamTypeAac:
		streamInfo = &ElementaryStreamInfo{PID: pid, Codec: "AAC", Type: "audio"}
	case psi.PmtStreamTypeMpeg4VideoH265:
		streamInfo = &ElementaryStreamInfo{PID: pid, Codec: "HEVC", Type: "video"}
	case psi.PmtStreamTypeScte35:
		streamInfo = &ElementaryStreamInfo{PID: pid, Codec: "SCTE35", Type: "cue"}
	}

	return streamInfo
}

func ParsePatcketToPAT(pkt *packet.Packet) (pat psi.PAT, e error) {
	if packet.IsPat(pkt) {
		pay, err := packet.Payload(pkt)
		if err != nil {
			return nil, err
		}

		pat, err = psi.NewPAT(pay)
		if err != nil {
			return nil, err
		}

		return pat, nil
	}

	return nil, fmt.Errorf("unable to parse packet to PAT")
}

func FilterPidsFromPidList(pidsToDrop []int, availablePids []int) []int {
	pidsToKeep := []int{}
	for _, pid := range availablePids {
		if !slices.Contains(pidsToDrop, pid) {
			pidsToKeep = append(pidsToKeep, pid)
		}
	}

	return pidsToKeep
}

func ParsePidsFromString(input string) []int {
	words := strings.Fields(input)
	var pids []int
	for _, word := range words {
		number, err := strconv.Atoi(word)
		if err != nil {
			continue
		}
		pids = append(pids, number)
	}
	return pids
}

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
