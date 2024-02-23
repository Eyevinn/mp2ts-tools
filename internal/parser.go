package internal

import (
	"bufio"
	"context"
	"fmt"
	"io"

	"github.com/Comcast/gots/v2/packet"
	"github.com/Comcast/gots/v2/psi"
	"github.com/Comcast/gots/v2/scte35"
	"github.com/asticode/go-astits"
	slices "golang.org/x/exp/slices"
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
				streamInfo := ParseAstitsElementaryStreamInfo(es)
				if streamInfo != nil {
					esKinds[es.ElementaryPID] = streamInfo.Codec
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
		case "SMPTE-2038":
			if o.ShowSMPTE2038 {
				ParseSMPTE2038(jp, d, o)
			}
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
				streamInfo := ParseAstitsElementaryStreamInfo(es)
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
			streamInfo := ParseElementaryStreamInfo(es)
			if streamInfo != nil {
				if streamInfo.Codec == "SCTE35" {
					scte35PIDs[es.ElementaryPid()] = true
				}

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

func FilterPids(ctx context.Context, textWriter io.Writer, tsWriter io.Writer, f io.Reader, o Options) error {
	pidsToDrop := ParsePidsFromString(o.PidsToDrop)
	if slices.Contains(pidsToDrop, 0) {
		return fmt.Errorf("filtering out PAT is not allowed")
	}

	reader := bufio.NewReader(f)
	_, err := packet.Sync(reader)
	if err != nil {
		return fmt.Errorf("syncing with reader %w", err)
	}

	jp := &JsonPrinter{W: textWriter, Indent: o.Indent}
	statistics := PidFilterStatistics{PidsToDrop: pidsToDrop, TotalPackets: 0, FilteredPackets: 0, PacketsBeforePAT: 0}

	var pkt packet.Packet
	var pat psi.PAT
	foundPAT := false
	hasShownStreamInfo := false
	// Skip packets until PAT
	for {
		// Read packet
		if _, err := io.ReadFull(reader, pkt[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return fmt.Errorf("reading Packet %w", err)
		}
		if packet.IsPat(&pkt) {
			// Found first PAT packet
			foundPAT = true
		}

		// Count PAT packet and non-PMT packets
		statistics.TotalPackets = statistics.TotalPackets + 1
		if !foundPAT {
			// packets before PAT
			statistics.PacketsBeforePAT = statistics.PacketsBeforePAT + 1
			continue
		}

		if packet.IsPat(&pkt) {
			// Parse PAT packet
			pat, err = ParsePacketToPAT(&pkt)
			if err != nil {
				return err
			}

			// Save PAT packet
			if err = WritePacket(&pkt, tsWriter); err != nil {
				return err
			}

			// Handle PMT packet(s)
			pm := pat.ProgramMap()
			for _, pid := range pm {
				if slices.Contains(pidsToDrop, pid) {
					return fmt.Errorf("filtering out PMT is not allowed")
				}

				packets, pmt, err := ReadPMTPackets(reader, pid)
				if err != nil {
					return err
				}
				// Count PMT packets
				statistics.TotalPackets = statistics.TotalPackets + uint32(len(packets))

				// 1. Print stream info only once
				if o.ShowStreamInfo && !hasShownStreamInfo {
					for _, es := range pmt.ElementaryStreams() {
						streamInfo := ParseElementaryStreamInfo(es)
						if streamInfo != nil {
							jp.Print(streamInfo, true)
						}
					}
					hasShownStreamInfo = true
				}

				// 2. Drop pids if exist
				isFilteringOutPids := IsTwoSlicesOverlapping(pmt.Pids(), pidsToDrop)
				pkts := []*packet.Packet{}
				for i := range packets {
					pkts = append(pkts, &packets[i])
				}
				if isFilteringOutPids {
					pidsToKeep := GetDifferenceOfTwoSlices(pmt.Pids(), pidsToDrop)
					pkts, err = psi.FilterPMTPacketsToPids(pkts, pidsToKeep)
					if err != nil {
						return fmt.Errorf("filtering pids %w", err)
					}

					statistics.FilteredPackets = statistics.FilteredPackets + uint32(len(pkts))
				}

				// 3. Save PMT packets
				for _, p := range pkts {
					if err = WritePacket(p, tsWriter); err != nil {
						return err
					}
				}
			}

			// Move on to next packet
			continue
		}

		// Save non-PAT/PMT packets
		if err = WritePacket(&pkt, tsWriter); err != nil {
			return err
		}
	}

	jp.PrintFilter(statistics, true)
	return nil
}
