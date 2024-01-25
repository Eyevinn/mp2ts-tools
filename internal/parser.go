package internal

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/Comcast/gots/v2/packet"
	"github.com/Comcast/gots/v2/psi"
	"github.com/Comcast/gots/v2/scte35"
	"github.com/asticode/go-astits"
)

func ParseAll(ctx context.Context, w io.Writer, f io.Reader, o Options) error {
	rd := bufio.NewReaderSize(f, 1000*PacketSize)
	dmx := astits.NewDemuxer(ctx, rd)
	pmtPID := -1
	nrPics := 0
	sdtPrinted := false
	esKinds := make(map[uint16]string)
	avcPSs := make(map[uint16]*AvcPS)
	hevcPSs := make(map[uint16]*HevcPS)
	jp := &JsonPrinter{W: w, Indent: o.Indent}
	statistics := make(map[uint16]*StreamStatistics)
dataLoop:
	for {
		// Check if context was cancelled
		select {
		case <-ctx.Done():
			break dataLoop
		default:
		}

		d, err := dmx.NextData()
		if err != nil {
			if err.Error() == "astits: no more packets" {
				break dataLoop
			}
			return fmt.Errorf("reading next data %w", err)
		}

		// Print service information
		if d.SDT != nil && !sdtPrinted {
			jp.PrintSdtInfo(d.SDT, o.ShowService)
			sdtPrinted = true
		}

		// Print PID information
		if pmtPID < 0 && d.PMT != nil {
			// Loop through elementary streams
			for _, es := range d.PMT.ElementaryStreams {
				var streamInfo *ElementaryStreamInfo
				switch es.StreamType {
				case astits.StreamTypeH264Video:
					streamInfo = &ElementaryStreamInfo{PID: es.ElementaryPID, Codec: "AVC", Type: "video"}
					esKinds[es.ElementaryPID] = "AVC"
				case astits.StreamTypeAACAudio:
					streamInfo = &ElementaryStreamInfo{PID: es.ElementaryPID, Codec: "AAC", Type: "audio"}
					esKinds[es.ElementaryPID] = "AAC"
				case astits.StreamTypeH265Video:
					streamInfo = &ElementaryStreamInfo{PID: es.ElementaryPID, Codec: "HEVC", Type: "video"}
					esKinds[es.ElementaryPID] = "HEVC"
				}

				if streamInfo != nil {
					jp.Print(streamInfo, o.ShowStreamInfo)
				}
			}
			pmtPID = int(d.PID)
		}
		if pmtPID == -1 {
			continue
		}
		pes := d.PES
		if pes == nil {
			continue
		}

		switch esKinds[d.PID] {
		case "AVC":
			avcPS := avcPSs[d.PID]
			avcPS, err = ParseAVCPES(jp, d, avcPS, o)
			if err != nil {
				return err
			}
			if avcPS == nil {
				continue
			}
			if avcPSs[d.PID] == nil {
				avcPSs[d.PID] = avcPS
			}
			nrPics++
			statistics[d.PID] = &avcPS.Statistics
		case "HEVC":
			hevcPS := hevcPSs[d.PID]
			hevcPS, err = ParseHEVCPES(jp, d, hevcPS, o)
			if err != nil {
				return err
			}
			if hevcPS == nil {
				continue
			}
			if hevcPSs[d.PID] == nil {
				hevcPSs[d.PID] = hevcPS
			}
			nrPics++
			statistics[d.PID] = &hevcPS.Statistics
		default:
			// Skip unknown elementary streams
			continue
		}

		// Keep looping if MaxNrPictures equals 0
		if o.MaxNrPictures > 0 && nrPics >= o.MaxNrPictures {
			break dataLoop
		}
	}

	for _, s := range statistics {
		jp.PrintStatistics(*s, o.ShowStatistics)
	}

	return jp.Error()
}

func ParseInfo(ctx context.Context, w io.Writer, f io.Reader, o Options) error {
	rd := bufio.NewReaderSize(f, 1000*PacketSize)
	dmx := astits.NewDemuxer(ctx, rd)
	pmtPID := -1
	jp := &JsonPrinter{W: w, Indent: o.Indent}
dataLoop:
	for {
		// Check if context was cancelled
		select {
		case <-ctx.Done():
			break dataLoop
		default:
		}

		d, err := dmx.NextData()
		if err != nil {
			if err.Error() == "astits: no more packets" {
				break dataLoop
			}
			return fmt.Errorf("reading next data %w", err)
		}

		// Print PID information
		if pmtPID < 0 && d.PMT != nil {
			// Loop through elementary streams
			for _, es := range d.PMT.ElementaryStreams {
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

				if streamInfo != nil {
					jp.Print(streamInfo, o.ShowStreamInfo)
				}
			}
			pmtPID = int(d.PID)
		}
		if pmtPID == -1 {
			continue
		}

		// Exit imediately if we don't want service information
		if !o.ShowService {
			break dataLoop
		}

		// Print service information
		if d.SDT != nil {
			jp.PrintSdtInfo(d.SDT, o.ShowService)
			break dataLoop
		}

		// Loop until we have printed service information
	}

	return jp.Error()
}

func ParseSCTE35(ctx context.Context, w io.Writer, f io.Reader, o Options) error {
	reader := bufio.NewReader(f)
	_, err := packet.Sync(reader)
	if err != nil {
		return fmt.Errorf("syncing with reader %w", err)
	}
	pat, err := psi.ReadPAT(reader)
	if err != nil {
		return fmt.Errorf("reading PAT %w", err)
	}

	var pmts []psi.PMT
	pm := pat.ProgramMap()
	for _, pid := range pm {
		pmt, err := psi.ReadPMT(reader, pid)
		if err != nil {
			return fmt.Errorf("reading PMT %w", err)
		}
		pmts = append(pmts, pmt)
	}

	jp := &JsonPrinter{W: w, Indent: o.Indent}
	scte35PIDs := make(map[int]bool)
	for _, pmt := range pmts {
		for _, es := range pmt.ElementaryStreams() {
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
				scte35PIDs[es.ElementaryPid()] = true
			}

			if streamInfo != nil {
				jp.Print(streamInfo, o.ShowStreamInfo)
			}
		}
	}

	// Print SCTE35
	for {
		var pkt packet.Packet
		if _, err := io.ReadFull(reader, pkt[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return fmt.Errorf("reading Packet %w", err)
		}

		currPID := packet.Pid(&pkt)
		if scte35PIDs[currPID] {
			pay, err := packet.Payload(&pkt)
			if err != nil {
				return fmt.Errorf("cannot get payload for packet on PID %d Error=%s\n", currPID, err)
			}
			msg, err := scte35.NewSCTE35(pay)
			if err != nil {
				return fmt.Errorf("cannot parse SCTE35 Error=%v\n", err)
			}
			scte35 := toSCTE35(uint16(currPID), msg)
			jp.Print(scte35, o.ShowSCTE35)
		}
	}

	return jp.Error()
}

func FilterPids(ctx context.Context, w io.Writer, f io.Reader, o Options) error {
	reader := bufio.NewReader(f)
	_, err := packet.Sync(reader)
	if err != nil {
		return fmt.Errorf("syncing with reader %w", err)
	}
	pat, err := psi.ReadPAT(reader)
	if err != nil {
		return fmt.Errorf("reading PAT %w", err)
	}

	var pmts []psi.PMT
	pm := pat.ProgramMap()
	for _, pid := range pm {
		pmt, err := psi.ReadPMT(reader, pid)
		if err != nil {
			return fmt.Errorf("reading PMT %w", err)
		}
		pmts = append(pmts, pmt)
	}

	// Print stream info
	jp := &JsonPrinter{W: w, Indent: o.Indent}
	for _, pmt := range pmts {
		for _, es := range pmt.ElementaryStreams() {
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

			if streamInfo != nil {
				jp.Print(streamInfo, o.ShowStreamInfo)
			}
		}
	}

	// Remove the output file if it exists
	if _, err := os.Stat(o.OutputFile); err == nil {
		if err := os.Remove(o.OutputFile); err != nil {
			return fmt.Errorf("removing existing file %w", err)
		}
	}

	// Create and append to the new file
	fo, err := os.OpenFile(o.OutputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer fo.Close()
	pidsToKeep := ParsePidsFromString(o.PidsToKeep)
	for {
		var pkt packet.Packet
		if _, err := io.ReadFull(reader, pkt[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return fmt.Errorf("reading Packet %w", err)
		}

		isPMT, err := psi.IsPMT(&pkt, pat)
		if err != nil {
			return fmt.Errorf("parsing the PID of packet %w", err)
		}
		pkts := []*packet.Packet{&pkt}
		if isPMT {
			pkts, err = psi.FilterPMTPacketsToPids(pkts, pidsToKeep)
			if err != nil {
				return fmt.Errorf("filtering pids %w", err)
			}
		}

		if len(pkts) >= 1 {
			p := pkts[0]
			if _, err := fo.Write((*p)[:]); err != nil {
				return fmt.Errorf("writing to file %w", err)
			}
		}
	}

	return nil
}
