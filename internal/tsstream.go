package internal

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"

	"github.com/Comcast/gots/v2/packet"
	"github.com/Comcast/gots/v2/packet/adaptationfield"
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

// PCRSample is a single PCR observation: the packet index and the PCR value
// (27 MHz). Discontinuity marks a signalled time-base discontinuity
// (discontinuity_indicator): this PCR starts a new time base.
type PCRSample struct {
	PktNr         int
	PCR           int64
	Discontinuity bool
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
	PCRPid             int // the PMT's PCR_PID; else the first PCR-bearing PID; -1 if none
	pmt                psi.PMT
	ElStreams          map[int]*ElStream
	PassThrough        []PassThroughStream
	totNrPkts          int
	pcrSamples         []PCRSample
	discontinuities    []int            // packets with discontinuity_indicator on the PCR, audio, or video PID
	scte35             sectionAssembler // SCTE-35 sections seen by ProcessTSFile
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
	if ts.PCRPid, err = declaredPCRPid(r, ts.PMTPid); err != nil {
		return nil, err
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

// declaredPCRPid returns the PCR_PID of the first PMT section, or -1 if the PMT
// declares none (0x1fff). gots does not expose it, so it is read from the packet.
func declaredPCRPid(r io.ReadSeeker, pmtPid int) (int, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return -1, err
	}
	var b [PacketSize]byte
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return -1, nil
		}
		if tsPID(b[:]) != pmtPid || !tsPUSI(b[:]) {
			continue
		}
		o := tsPayloadOffset(b[:])
		if o >= PacketSize {
			continue
		}
		o += 1 + int(b[o]) // pointer_field
		if o+10 > PacketSize || b[o] != 0x02 {
			continue
		}
		if pid := int(b[o+8]&0x1f)<<8 | int(b[o+9]); pid != stuffingPID {
			return pid, nil
		}
		return -1, nil
	}
}

// ProcessTSFile reads the whole file once, building continuity-counter history
// and delivering each completed PES to the elementary streams' PES handlers.
func (t *TSStream) ProcessTSFile(ctx context.Context, ifh io.ReadSeeker) error {
	if _, err := ifh.Seek(0, io.SeekStart); err != nil {
		return err
	}
	t.totNrPkts = 0
	t.scte35 = sectionAssembler{}
	t.pcrSamples, t.discontinuities = nil, nil
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
		disc := packet.ContainsAdaptationField(&pkt) && adaptationfield.Length(&pkt) > 0 &&
			adaptationfield.IsDiscontinuous(&pkt)
		if pcr, ok := packetPCR(&pkt); ok {
			if t.PCRPid == -1 {
				t.PCRPid = pid
			}
			if pid == t.PCRPid {
				t.pcrSamples = append(t.pcrSamples, PCRSample{PktNr: pktNr, PCR: pcr, Discontinuity: disc})
			}
		}
		if es, ok := t.ElStreams[pid]; disc && (pid == t.PCRPid || ok && es.MediaType != "text") {
			t.discontinuities = append(t.discontinuities, pktNr)
		}
		if pid == t.SCTE35Pid {
			t.scte35.add(pkt[:], pktNr*PacketSize)
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
// here within 0.5%). Intervals across a signalled discontinuity (a new time
// base) are skipped. ok is false if there are too few samples.
func (t *TSStream) PCRBitrate() (avg, min, max int64, constant, ok bool) {
	s := t.pcrSamples
	const pcrFull = int64(1) << 33 * 300
	minF, maxF := 1e30, 0.0
	var totPk, totPCR int64
	for k := 1; k < len(s); k++ {
		if s[k].Discontinuity {
			continue
		}
		dpk := int64(s[k].PktNr - s[k-1].PktNr)
		dpcr := s[k].PCR - s[k-1].PCR
		if dpcr < 0 {
			dpcr += pcrFull
		}
		if dpcr <= 0 {
			continue
		}
		totPk += dpk
		totPCR += dpcr
		br := float64(dpk*PacketSize*8) * 27_000_000.0 / float64(dpcr)
		if br < minF {
			minF = br
		}
		if br > maxF {
			maxF = br
		}
	}
	if totPCR <= 0 {
		return 0, 0, 0, false, false
	}
	avgF := float64(totPk*PacketSize*8) * 27_000_000.0 / float64(totPCR)
	constant = (maxF - minF) <= avgF*0.005
	return int64(avgF), int64(minF), int64(maxF), constant, true
}

// PCRDeviationMs returns how far, at most, the PCR samples are from the straight
// line a constant-rate stream would follow (least squares over packet index), in
// milliseconds. Each stretch between signalled discontinuities is fitted on its
// own. It is near 0 for a constant-rate stream.
func (t *TSStream) PCRDeviationMs() float64 {
	dev := 0.0
	s := t.pcrSamples
	for a := 0; a < len(s); {
		b := a + 1
		for b < len(s) && !s[b].Discontinuity {
			b++
		}
		dev = math.Max(dev, pcrLineDeviation(s[a:b]))
		a = b
	}
	return dev / 27_000
}

// pcrLineDeviation returns the largest distance (27 MHz ticks) of PCR samples of
// one time base from their least-squares line over packet index.
func pcrLineDeviation(s []PCRSample) float64 {
	if len(s) < 3 {
		return 0
	}
	const pcrFull = int64(1) << 33 * 300
	xs := make([]float64, len(s))
	ys := make([]float64, len(s))
	var unwrap int64
	for k := range s {
		if k > 0 && s[k].PCR < s[k-1].PCR {
			unwrap += pcrFull
		}
		xs[k], ys[k] = float64(s[k].PktNr), float64(s[k].PCR+unwrap)
	}
	var mx, my float64
	for k := range xs {
		mx += xs[k]
		my += ys[k]
	}
	mx /= float64(len(xs))
	my /= float64(len(ys))
	var sxy, sxx float64
	for k := range xs {
		sxy += (xs[k] - mx) * (ys[k] - my)
		sxx += (xs[k] - mx) * (xs[k] - mx)
	}
	if sxx == 0 {
		return 0
	}
	slope := sxy / sxx
	dev := 0.0
	for k := range xs {
		dev = math.Max(dev, math.Abs(ys[k]-(my+slope*(xs[k]-mx))))
	}
	return dev
}

// requireConstantRate returns an error unless the stream has a usable PCR and a
// constant rate. The loop is re-timed with a linear PCR at that rate, which only
// keeps the source timing of a constant-rate stream; variable-rate streams are
// not supported.
func (t *TSStream) requireConstantRate() error {
	_, _, _, constant, ok := t.PCRBitrate()
	switch {
	case !ok:
		return fmt.Errorf("no usable PCR")
	case !constant:
		return fmt.Errorf("not a constant-rate stream: its PCR is up to %.0f ms from a constant rate "+
			"(variable-rate streams are not supported)", t.PCRDeviationMs())
	}
	return nil
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
