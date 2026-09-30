package internal

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/Comcast/gots/v2/packet"
	"github.com/Comcast/gots/v2/psi"
)

// Stream type and descriptor tags not exported by gots/v2.
const (
	pmtStreamTypeMpeg1Layer2 = 3 // ISO/IEC 11172-3 (MPEG-1 audio, MP2)
	pmtStreamTypeMpeg2Audio  = 4 // ISO/IEC 13818-3 (MPEG-2 audio, MP2)
	descTagAC3               = 0x6a
	descTagISO639Language    = 0x0a
	descTagTeletext          = 0x56
	descTagDVBSubtitle       = 0x59
	descTagANC               = 0xc4 // SMPTE-2038 ancillary data descriptor
)

// PCRSample is a single PCR observation: the output packet index and the PCR
// value (27 MHz).
type PCRSample struct {
	PktNr int
	PCR   int64
}

// PassThroughStream is a non-audio, non-video elementary stream (e.g. SMPTE-2038
// ANC data) that the looper copies through unchanged, applying only the per-wrap
// timestamp shift. It is reported by the scan so the operator knows it is kept.
type PassThroughStream struct {
	PID        int    `json:"pid"`
	StreamType uint8  `json:"streamType"`
	Label      string `json:"label"`
}

// TSStream is a single-program MPEG-2 TS with its audio, video, and SCTE-35
// elementary streams identified, used by the mp2ts-loop scan/prepare/loop passes.
type TSStream struct {
	PMTPid             int
	SCTE35Pid          int // -1 if none
	PCRPid             int // -1 until a PCR-bearing PID is seen
	pmt                psi.PMT
	ElStreams          map[int]*ElStream
	PassThrough        []PassThroughStream
	totNrPkts          int
	pcrSamples         []PCRSample
	ContinuityCounters *ContinuityCounters
}

// codecFromStreamType maps a PMT stream_type to the engine Codec.
func codecFromStreamType(st uint8) Codec {
	switch st {
	case psi.PmtStreamTypeMpeg4VideoH264:
		return CODEC_AVC
	case psi.PmtStreamTypeMpeg4VideoH265:
		return CODEC_HEVC
	case psi.PmtStreamTypeMpeg2VideoH262:
		return CODEC_MPEG2V
	case psi.PmtStreamTypeAac:
		return CODEC_AAC
	case psi.PmtStreamTypeAc3:
		return CODEC_AC3
	case psi.PmtStreamTypeEc3:
		return CODEC_EC3
	case pmtStreamTypeMpeg1Layer2, pmtStreamTypeMpeg2Audio:
		return CODEC_MP2
	case psi.PmtStreamTypeScte35:
		return CODEC_SCTE35
	case psi.PmtStreamTypeID3:
		return CODEC_ID3
	case psi.PmtStreamTypePrivateContent:
		return CODEC_PRIVATE
	default:
		return CODEC_UNKNOWN
	}
}

// InitTS reads the PAT and PMT and builds an ElStream for each audio, video, or
// text elementary stream. Only single-program streams are supported.
func InitTS(r io.ReadSeeker) (*TSStream, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	ts := &TSStream{
		SCTE35Pid:          -1,
		PCRPid:             -1,
		ElStreams:          make(map[int]*ElStream),
		ContinuityCounters: NewContinuityCounters(),
	}
	pat, err := psi.ReadPAT(r)
	if err != nil {
		return nil, fmt.Errorf("ReadPAT: %w", err)
	}
	programMaps := pat.ProgramMap()
	if len(programMaps) != 1 {
		return nil, fmt.Errorf("expected exactly one program in PAT, found %d", len(programMaps))
	}
	for _, pid := range programMaps {
		ts.PMTPid = pid
	}
	ts.pmt, err = psi.ReadPMT(r, ts.PMTPid)
	if err != nil {
		return nil, fmt.Errorf("ReadPMT: %w", err)
	}
	for _, e := range ts.pmt.ElementaryStreams() {
		pid := e.ElementaryPid()
		codec := codecFromStreamType(e.StreamType())
		language := ""
		isANC := false
		for _, desc := range e.Descriptors() {
			switch desc.Tag() {
			case descTagAC3:
				codec = CODEC_AC3
			case descTagISO639Language:
				language = desc.DecodeIso639LanguageCode()
			case descTagTeletext:
				codec = CODEC_TELETEXT
			case descTagDVBSubtitle:
				codec = CODEC_DVB_SUBTITLES
			case descTagANC:
				isANC = true
			}
		}
		switch {
		case codec.IsVideo(), codec.IsAudio(), codec == CODEC_TELETEXT, codec == CODEC_DVB_SUBTITLES:
			ts.ElStreams[pid] = NewElStream(pid, codec, language)
			slog.Debug("elementary stream", "pid", pid, "codec", codec.String(), "lang", language)
		case codec == CODEC_SCTE35:
			ts.SCTE35Pid = pid
			slog.Debug("SCTE-35 stream", "pid", pid)
		default:
			label := codec.String()
			if isANC {
				label = "smpte-2038"
			}
			ts.PassThrough = append(ts.PassThrough, PassThroughStream{PID: pid, StreamType: e.StreamType(), Label: label})
			slog.Debug("pass-through stream", "pid", pid, "streamType", e.StreamType(), "label", label)
		}
	}
	return ts, nil
}

// ProcessTSFile reads the whole file once, building continuity-counter history
// and delivering each completed PES to the elementary streams' PES handlers.
func (t *TSStream) ProcessTSFile(ctx context.Context, ifh io.ReadSeeker) error {
	if _, err := ifh.Seek(0, io.SeekStart); err != nil {
		return err
	}
	t.totNrPkts = 0
	var pkt packet.Packet
	pktNr := -1
Loop:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if _, err := io.ReadFull(ifh, pkt[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break Loop
			}
			return err
		}
		pktNr++
		t.totNrPkts++
		pid := packet.Pid(&pkt)
		if pid == stuffingPID {
			continue
		}
		step := t.ContinuityCounters.CalcStep(pid, pktNr, int(packet.ContinuityCounter(&pkt)))
		if step > 1 {
			slog.Warn("packet loss", "pkt", pktNr, "pid", pid, "step", step)
		}
		if pcr, ok := packetPCR(&pkt); ok {
			if t.PCRPid == -1 {
				t.PCRPid = pid
			}
			if pid == t.PCRPid {
				t.pcrSamples = append(t.pcrSamples, PCRSample{PktNr: pktNr, PCR: pcr})
			}
		}
		es, ok := t.ElStreams[pid]
		if !ok || es.GetPESHandler() == nil {
			continue
		}
		if err := es.AddPacket(&pkt, uint32(pktNr)); err != nil {
			return err
		}
	}
	for _, es := range t.ElStreams {
		if es.GetPESHandler() != nil {
			if err := es.HandleLastPES(); err != nil {
				return fmt.Errorf("handleLastPES pid %d: %w", es.PID, err)
			}
		}
	}
	return nil
}

// TotalNrPackets returns the number of TS packets seen by the last ProcessTSFile.
func (t *TSStream) TotalNrPackets() int {
	return t.totNrPkts
}

// PCRSamples returns the PCR observations captured during ProcessTSFile.
func (t *TSStream) PCRSamples() []PCRSample {
	return t.pcrSamples
}

// PCRBitrate computes the TS bitrate (bits/s) from PCR samples: the overall
// average plus the per-interval min/max, and whether the rate is constant (CBR,
// here within 0.5%). ok is false if there are too few samples.
func (t *TSStream) PCRBitrate() (avg, min, max int64, constant, ok bool) {
	s := t.pcrSamples
	if len(s) < 2 {
		return 0, 0, 0, false, false
	}
	const pcrFull = int64(1) << 33 * 300
	minF, maxF := 1e30, 0.0
	for k := 1; k < len(s); k++ {
		dpk := int64(s[k].PktNr - s[k-1].PktNr)
		dpcr := s[k].PCR - s[k-1].PCR
		if dpcr < 0 {
			dpcr += pcrFull
		}
		if dpcr <= 0 {
			continue
		}
		br := float64(dpk*PacketSize*8) * 27_000_000.0 / float64(dpcr)
		if br < minF {
			minF = br
		}
		if br > maxF {
			maxF = br
		}
	}
	dpk := int64(s[len(s)-1].PktNr - s[0].PktNr)
	dpcr := s[len(s)-1].PCR - s[0].PCR
	if dpcr < 0 {
		dpcr += pcrFull
	}
	if dpcr <= 0 {
		return 0, 0, 0, false, false
	}
	avgF := float64(dpk*PacketSize*8) * 27_000_000.0 / float64(dpcr)
	constant = (maxF - minF) <= avgF*0.005
	return int64(avgF), int64(minF), int64(maxF), constant, true
}

// FindFirstPTS returns the first PTS of every audio and video elementary stream.
func (t *TSStream) FindFirstPTS(ctx context.Context, ifh io.ReadSeeker) (map[int]int64, error) {
	if _, err := ifh.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	startPTS := make(map[int]int64)
	for pid, es := range t.ElStreams {
		if es.MediaType == "video" || es.MediaType == "audio" {
			startPTS[pid] = -1
		}
	}
	found := 0
	var pkt packet.Packet
	for {
		select {
		case <-ctx.Done():
			return startPTS, ctx.Err()
		default:
		}
		if _, err := io.ReadFull(ifh, pkt[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}
		pid := packet.Pid(&pkt)
		if want, ok := startPTS[pid]; ok && want == -1 {
			if pts := GetPTS(&pkt); pts >= 0 {
				startPTS[pid] = pts
				found++
				if found == len(startPTS) {
					return startPTS, nil
				}
			}
		}
	}
	return startPTS, nil
}
