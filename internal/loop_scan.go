package internal

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"github.com/Comcast/gots/v2"
)

// FieldParity describes whether a coded picture is a full frame or a single
// field.
type FieldParity int

const (
	FieldUnknown FieldParity = iota
	FieldFrame
	FieldTop
	FieldBottom
)

func (f FieldParity) String() string {
	switch f {
	case FieldFrame:
		return "frame"
	case FieldTop:
		return "top"
	case FieldBottom:
		return "bottom"
	default:
		return "unknown"
	}
}

// payloadUnit is a PES packet or PSI section as carried in TS packets. Only
// packets with payload count, so adaptation-field-only packets (PCR carriers)
// never make a unit look longer than it is.
type payloadUnit struct {
	first int // the packet with payload_unit_start_indicator
	last  int
}

type videoPicture struct {
	firstPacket int
	pts, dts    int64 // unwrapped, 90 kHz
	info        pictureInfo
}

// audioFrame is the smallest piece of audio a lap can end on: one coded frame,
// or a whole PES for audio whose frames are not parsed and for a PES whose
// frames cannot be read.
type audioFrame struct {
	start     int64 // unwrapped, in units of 1 / (90 kHz * timeScale)
	duration  int64 // same unit
	endOffset int   // PES byte just past the frame, counted from the PES start code
}

// payloadChunk is where the bytes of a PES lie: the PES byte just past each
// packet's payload.
type payloadChunk struct {
	packet    int
	endOffset int
}

type audioPES struct {
	firstPacket int
	pts         int64 // unwrapped, 90 kHz
	firstFrame  int   // into audioStream.frames
	frameCount  int
	firstChunk  int // into audioStream.chunks
	chunkCount  int
	// damaged: its frames cannot be read, as after a lost packet, so it is one
	// frame that lasts until the next PES.
	damaged bool
}

type audioStream struct {
	pid        int
	streamType byte
	codec      audioCodec
	sampleRate int
	// timeScale: audio times are in units of 1 / (90 kHz * timeScale), the
	// sample rate when frames are parsed, so frame durations are exact, and 1
	// otherwise.
	timeScale int64
	// frameLevel: laps end on frame boundaries; false: on PES boundaries.
	frameLevel bool
	pes        []audioPES
	frames     []audioFrame
	chunks     []payloadChunk
}

type pcrSample struct {
	packet int
	pcr    int64 // unwrapped, 27 MHz
}

// continuityError is a packet whose continuity counter does not follow its
// PID's previous one.
type continuityError struct {
	packet int
	pid    int
}

// dataTiming says whether each lap can take a data stream's PES by their PTS.
type dataTiming uint8

const (
	dataByPTS            dataTiming = iota
	dataUnreadableHeader            // scrambled, split across packets or not at the start of a packet's payload
	dataNoPTS
	dataOutOfOrder // a PES has an earlier PTS than one sent before it
)

func (d dataTiming) String() string {
	switch d {
	case dataByPTS:
		return "by PTS"
	case dataUnreadableHeader:
		return "a PES header that cannot be read"
	case dataNoPTS:
		return "a PES without a PTS"
	case dataOutOfOrder:
		return "a PES with an earlier PTS than one sent before it"
	}
	return "unknown"
}

type dataPES struct {
	firstPacket int
	pts         int64 // unwrapped, 90 kHz
	firstChunk  int   // into dataStream.chunks
	chunkCount  int
}

// dataStream is a PES stream other than video and audio, such as SMPTE ST 2038
// ANC, teletext or subtitles: each PES goes with the picture of its PTS.
type dataStream struct {
	pid        int
	streamType byte
	label      string
	timing     dataTiming
	pes        []dataPES
	chunks     []payloadChunk
}

// PassThroughStream is a stream of the program that is neither looped video,
// audio, a data PES stream nor SCTE-35: sections, or a second video. Its packets
// stay where the source sends them, so it breaks at every seam.
type PassThroughStream struct {
	PID        int    `json:"pid"`
	StreamType uint8  `json:"streamType"`
	Label      string `json:"label"`
}

// streamIndex is everything the loop needs to know about a TS, from one pass
// over it.
type streamIndex struct {
	packetCount   int
	programNumber int
	pmtPID        int
	pcrPID        int
	videoPID      int
	videoCodec    Codec
	pictures      []videoPicture // decode order
	audio         []audioStream
	scte35PIDs    []int
	data          []dataStream
	passThrough   []PassThroughStream
	pcrSamples    []pcrSample
	// discontinuities are packets of the program with discontinuity_indicator set.
	discontinuities []int
	// nullsBefore[i] is the number of null packets among packets [0, i).
	nullsBefore []uint32
	units       map[int][]payloadUnit
	// continuityErrors are in the source, in packet order, on every PID.
	continuityErrors []continuityError
	// scte35Sections are the splice_info_sections per SCTE-35 PID, with their
	// byte positions in the input.
	scte35Sections map[int][]SCTE35Section
	warnings       []string
}

// The scan's limits.
const (
	// maxInvalidPerMillion: a capture with a few corrupt packets still loops;
	// more than this is not TS.
	maxInvalidPerMillion = 10_000
	// btsProbePackets: packets at the start whose sync bytes must all be 204
	// bytes apart for the input to be a BTS.
	btsProbePackets = 8
	btsPacketSize   = 204
)

// isBTS reports an ISDB-T BTS, which follows each TS packet with 16 bytes of
// ISDB-T information and parity, so its sync bytes are 204 bytes apart, not 188.
func isBTS(data []byte) bool {
	if len(data) < btsProbePackets*btsPacketSize {
		return false
	}
	btsAligned, tsAligned := true, true
	for i := 1; i < btsProbePackets; i++ {
		btsAligned = btsAligned && data[i*btsPacketSize] == syncByte
		tsAligned = tsAligned && data[i*PacketSize] == syncByte
	}
	return btsAligned && !tsAligned
}

// psiCollector puts together the first section of a PID's sections.
type psiCollector struct {
	buf  []byte
	open bool
}

// add feeds a packet of the PID and returns a section that completes in it, if
// one does. Only the first section that starts in a packet is read.
func (c *psiCollector) add(b []byte) []byte {
	off := tsPayloadOffset(b)
	if off >= PacketSize {
		return nil
	}
	if tsPUSI(b) {
		start := off + 1 + int(b[off]) // after pointer_field
		if c.open {
			c.buf = append(c.buf, b[off+1:min(start, PacketSize)]...)
			if s := c.complete(); s != nil {
				return s
			}
		}
		if start >= PacketSize {
			c.open = false
			return nil
		}
		c.buf, c.open = append(c.buf[:0], b[start:]...), true
	} else if c.open {
		c.buf = append(c.buf, b[off:]...)
	}
	return c.complete()
}

func (c *psiCollector) complete() []byte {
	if !c.open || len(c.buf) < 3 {
		return nil
	}
	if c.buf[0] == stuffByte {
		c.open = false
		return nil
	}
	n := 3 + int(binary.BigEndian.Uint16(c.buf[1:3])&0x0fff)
	if len(c.buf) < n {
		return nil
	}
	c.open = false
	return c.buf[:n]
}

// validSection reports a current section of the table with section syntax and a
// correct CRC.
func validSection(s []byte, tableID byte) bool {
	return len(s) >= 12 && s[0] == tableID && s[1]&0x80 != 0 && s[5]&0x01 != 0 &&
		bytes.Equal(gots.ComputeCRC(s[:len(s)-4]), s[len(s)-4:])
}

type pmtDescriptor struct {
	tag  byte
	data []byte
}

type pmtStream struct {
	streamType  byte
	pid         int
	descriptors []pmtDescriptor
}

type program struct {
	number        int
	pmtPID        int
	pcrPID        int
	streams       []pmtStream // by PID
	otherPrograms []int       // in the PAT, not looped
}

// parsePMT reads a PMT section. A descriptor that runs past its loop ends that
// loop; the PMT is refused only when its own lengths run past the section.
func parsePMT(s []byte) ([]pmtStream, int, bool) {
	end := len(s) - 4
	pcrPID := int(binary.BigEndian.Uint16(s[8:10]) & 0x1fff)
	pos := 12 + int(binary.BigEndian.Uint16(s[10:12])&0x0fff)
	if pos > end {
		return nil, 0, false
	}
	var streams []pmtStream
	for pos < end {
		if pos+5 > end {
			return nil, 0, false
		}
		es := pmtStream{streamType: s[pos], pid: int(binary.BigEndian.Uint16(s[pos+1:pos+3]) & 0x1fff)}
		infoEnd := pos + 5 + int(binary.BigEndian.Uint16(s[pos+3:pos+5])&0x0fff)
		if infoEnd > end {
			return nil, 0, false
		}
		for d := pos + 5; d+2 <= infoEnd; {
			n := int(s[d+1])
			if d+2+n > infoEnd {
				break
			}
			es.descriptors = append(es.descriptors, pmtDescriptor{tag: s[d], data: s[d+2 : d+2+n]})
			d += 2 + n
		}
		streams = append(streams, es)
		pos = infoEnd
	}
	sort.SliceStable(streams, func(i, j int) bool { return streams[i].pid < streams[j].pid })
	return streams, pcrPID, true
}

// findProgram reads the first program of the PAT and its PMT.
func findProgram(data []byte, n int) (program, error) {
	var p program
	known := false
	var pat, pmt psiCollector
	for i := 0; i < n; i++ {
		b := data[i*PacketSize : (i+1)*PacketSize]
		if b[0] != syncByte {
			continue
		}
		pid := tsPID(b)
		switch {
		case pid == 0 && !known:
			s := pat.add(b)
			if s == nil || !validSection(s, 0x00) {
				continue
			}
			for e := 8; e+4 <= len(s)-4; e += 4 {
				number := int(binary.BigEndian.Uint16(s[e : e+2]))
				if number == 0 { // it points at the NIT
					continue
				}
				if !known {
					p = program{number: number, pmtPID: int(binary.BigEndian.Uint16(s[e+2:e+4]) & 0x1fff)}
					known = true
				} else {
					p.otherPrograms = append(p.otherPrograms, number)
				}
			}
		case known && pid == p.pmtPID:
			s := pmt.add(b)
			if s == nil || !validSection(s, 0x02) || int(binary.BigEndian.Uint16(s[3:5])) != p.number {
				continue
			}
			streams, pcrPID, ok := parsePMT(s)
			if !ok {
				continue
			}
			p.streams, p.pcrPID = streams, pcrPID
			return p, nil
		}
	}
	if known {
		return p, fmt.Errorf("no PMT found")
	}
	return p, fmt.Errorf("no PAT found")
}

func otherProgramsWarning(p program) string {
	var others []string
	for _, n := range p.otherPrograms {
		others = append(others, fmt.Sprint(n))
	}
	one := len(p.otherPrograms) == 1
	which, verb, own := "programs", "are", "their"
	if one {
		which, verb, own = "program", "is", "its"
	}
	return fmt.Sprintf("only program %d of the %d in the PAT is looped; %s %s %s cut at the same points, not %s own, "+
		"so %s video, audio and PCR break at every seam",
		p.number, len(p.otherPrograms)+1, which, strings.Join(others, ", "), verb, own, own)
}

// Stream types and descriptors the scan tells streams apart by.
const (
	stMPEG1Video    = 0x01
	stMPEG2Video    = 0x02
	stMPEG1Audio    = 0x03
	stMPEG2Audio    = 0x04
	stPrivateData   = 0x06
	stADTS          = 0x0f
	stLATM          = 0x11
	stID3           = 0x15
	stAVC           = 0x1b
	stHEVC          = 0x24
	stAC3           = 0x81
	stSCTE35        = 0x86
	stEAC3          = 0x87
	descRegistation = 0x05
	descTeletext    = 0x56
	descSubtitling  = 0x59
	descAC3         = 0x6a
	descEAC3        = 0x7a
	descExtension   = 0x7f
	descANC         = 0xc4 // SMPTE-2038 ancillary data descriptor
	extAC4          = 0x15
)

func videoCodecOf(streamType byte) (Codec, bool) {
	switch streamType {
	case stMPEG1Video, stMPEG2Video: // MPEG-1 video is a subset of H.262
		return CODEC_MPEG2V, true
	case stAVC:
		return CODEC_AVC, true
	case stHEVC:
		return CODEC_HEVC, true
	}
	return CODEC_UNKNOWN, false
}

// registration reports a registration descriptor with the format_identifier.
func (es pmtStream) registration(format string) bool {
	for _, d := range es.descriptors {
		if d.tag == descRegistation && len(d.data) >= 4 && string(d.data[:4]) == format {
			return true
		}
	}
	return false
}

func (es pmtStream) hasDescriptor(tag byte) bool {
	for _, d := range es.descriptors {
		if d.tag == tag {
			return true
		}
	}
	return false
}

// isAC4 reports AC-4 as DVB carries it: private data with an AC-4 descriptor, an
// extension descriptor (ETSI EN 300 468, 6.4.1).
func (es pmtStream) isAC4() bool {
	if es.streamType != stPrivateData {
		return false
	}
	for _, d := range es.descriptors {
		if d.tag == descExtension && len(d.data) > 0 && d.data[0] == extAC4 {
			return true
		}
	}
	return false
}

// audioCodecOf returns how the frames of an audio stream are read, and false
// for a stream that is not audio.
func audioCodecOf(es pmtStream) (audioCodec, bool) {
	switch es.streamType {
	case stMPEG1Audio, stMPEG2Audio:
		return audioMPEG, true
	case stADTS:
		return audioADTS, true
	case stAC3, stEAC3:
		return audioAC3, true
	case stLATM:
		return audioUnknown, true
	case stPrivateData:
		switch {
		case es.hasDescriptor(descAC3), es.hasDescriptor(descEAC3), es.registration("AC-3"), es.registration("EAC3"):
			return audioAC3, true
		case es.registration("BSSD"), es.isAC4(): // S302M, AC-4: laps end on PES boundaries
			return audioUnknown, true
		}
	}
	return audioUnknown, false
}

// streamLabel names a stream that is neither looped video nor audio.
func streamLabel(es pmtStream) string {
	switch {
	case es.hasDescriptor(descANC) || es.registration("VANC"):
		return "smpte-2038"
	case es.hasDescriptor(descTeletext):
		return "teletext"
	case es.hasDescriptor(descSubtitling):
		return "dvb-subtitles"
	case es.streamType == stID3:
		return "id3"
	case es.streamType == stPrivateData:
		return "private"
	}
	if c, ok := videoCodecOf(es.streamType); ok {
		return c.String()
	}
	return fmt.Sprintf("stream type 0x%02x", es.streamType)
}

// carriesSections reports stream types that carry sections rather than PES
// (ISO/IEC 13818-1 table 2-34, SCTE 27, SCTE 35, ATSC A/90).
func carriesSections(streamType byte) bool {
	switch streamType {
	case 0x05, // private sections
		0x0a, // DSM-CC multi-protocol encapsulation
		0x0b, // DSM-CC U-N messages
		0x0c, // DSM-CC stream descriptors
		0x0d, // DSM-CC sections
		0x13, // SL-packetized stream in sections
		0x14, // DSM-CC synchronized download protocol
		0x16, // metadata in sections
		0x17, // metadata in DSM-CC data carousel
		0x18, // metadata in DSM-CC object carousel
		0x19, // metadata in synchronized download protocol
		0x82, // SCTE 27 subtitles
		0x86, // SCTE-35
		0x95: // ATSC data service table
		return true
	}
	return false
}

type pidRole uint8

const (
	roleOther pidRole = iota
	roleVideo
	roleAudio
	roleData
)

// looped reports the roles whose PES the loop is cut at and timed by.
func (r pidRole) looped() bool { return r == roleVideo || r == roleAudio }

type dataAssembly struct {
	open      bool
	endOffset int
	pts       unwrapper
}

type audioAssembly struct {
	open       bool
	headerSize int
	bytes      []byte // payload after the PES header
	endOffset  int
	// damaged counts the PES whose frames cannot be read: they straddle PES,
	// change sample rate, or a packet was lost.
	damaged      int
	firstDamaged int // packet
	pts          unwrapper
}

type pidContinuity struct {
	seen     bool
	last     byte
	repeated bool   // the last packet was sent twice
	payload  []byte // of the last packet with payload
}

type misplacedPES struct {
	count       int
	firstPacket int
}

type scanner struct {
	idx            *streamIndex
	role           [numPIDs]pidRole
	audioSlot      [numPIDs]int
	dataSlot       [numPIDs]int
	carriesPES     [numPIDs]bool
	continuity     [numPIDs]pidContinuity
	audio          []audioAssembly
	data           []dataAssembly
	scte35         map[int]*sectionAssembler
	duplicate      bool // the packet being added repeats its PID's previous one
	pcrUnwrap      unwrapper
	videoPTS       unwrapper
	videoDTS       unwrapper
	videoOpen      bool
	videoPicture   videoPicture
	videoBytes     []byte
	inspector      *pictureInspector
	nulls          uint32
	invalid        int
	corrupt        int
	pesWithoutPTS  int
	unreadablePES  int
	misplacedStart map[int]*misplacedPES
}

func newScanner(idx *streamIndex, p program) *scanner {
	s := &scanner{
		idx:            idx,
		scte35:         make(map[int]*sectionAssembler),
		pcrUnwrap:      unwrapper{wrap: pcrWrapU},
		videoPTS:       unwrapper{wrap: ptsWrapU},
		videoDTS:       unwrapper{wrap: ptsWrapU},
		misplacedStart: make(map[int]*misplacedPES),
	}
	idx.programNumber, idx.pmtPID, idx.pcrPID, idx.videoPID = p.number, p.pmtPID, p.pcrPID, -1
	if len(p.otherPrograms) > 0 {
		idx.warnings = append(idx.warnings, otherProgramsWarning(p))
	}
	for _, es := range p.streams {
		if codec, ok := videoCodecOf(es.streamType); ok && idx.videoPID < 0 {
			idx.videoPID, idx.videoCodec = es.pid, codec
			s.role[es.pid] = roleVideo
		} else if codec, ok := audioCodecOf(es); ok {
			s.audioSlot[es.pid] = len(idx.audio)
			idx.audio = append(idx.audio, audioStream{pid: es.pid, streamType: es.streamType, codec: codec, timeScale: 1})
			s.audio = append(s.audio, audioAssembly{pts: unwrapper{wrap: ptsWrapU}})
			s.role[es.pid] = roleAudio
		} else if es.streamType == stSCTE35 {
			idx.scte35PIDs = append(idx.scte35PIDs, es.pid)
			s.scte35[es.pid] = &sectionAssembler{}
		} else if _, video := videoCodecOf(es.streamType); !video && !carriesSections(es.streamType) {
			s.dataSlot[es.pid] = len(idx.data)
			idx.data = append(idx.data, dataStream{pid: es.pid, streamType: es.streamType, label: streamLabel(es)})
			s.data = append(s.data, dataAssembly{pts: unwrapper{wrap: ptsWrapU}})
			s.role[es.pid] = roleData
		} else {
			idx.passThrough = append(idx.passThrough,
				PassThroughStream{PID: es.pid, StreamType: es.streamType, Label: streamLabel(es)})
		}
		s.carriesPES[es.pid] = !carriesSections(es.streamType)
	}
	return s
}

func (s *scanner) addPacket(i int, b []byte) {
	s.idx.nullsBefore = append(s.idx.nullsBefore, s.nulls)
	if b[0] != syncByte {
		s.invalid++
		return
	}
	if tsOversizedAF(b) {
		s.corrupt++
		return
	}
	pid := tsPID(b)
	if pid == nullPID {
		s.nulls++
		return
	}
	s.checkContinuity(i, pid, b)
	s.checkPESStart(i, pid, b)
	s.trackUnits(i, pid, b)
	if (pid == s.idx.pcrPID || s.role[pid].looped()) && tsAFFlags(b)&afFlagDiscontinuity != 0 {
		s.idx.discontinuities = append(s.idx.discontinuities, i)
	}
	if pid == s.idx.pcrPID && tsCarriesPCR(b) {
		s.idx.pcrSamples = append(s.idx.pcrSamples,
			pcrSample{packet: i, pcr: s.pcrUnwrap.unwrap(readPCR(b[pcrFieldOffset:]))})
	}
	if a, ok := s.scte35[pid]; ok {
		a.add(b, i*PacketSize)
	}
	switch s.role[pid] {
	case roleVideo:
		s.addVideoPacket(i, b)
	case roleAudio:
		s.addAudioPacket(s.audioSlot[pid], i, b)
	case roleData:
		s.addDataPacket(s.dataSlot[pid], i, b)
	}
}

// checkContinuity follows ISO/IEC 13818-1 2.4.3.3: the counter steps by one on
// each packet with payload, a packet may be sent twice in a row, and a
// discontinuity_indicator lets it start anew. It sets s.duplicate for a packet
// sent twice: its payload is the previous packet's and must be read only once.
func (s *scanner) checkContinuity(i, pid int, b []byte) {
	s.duplicate = false
	if tsTransportErr(b) || !tsHasPayload(b) {
		return
	}
	c := &s.continuity[pid]
	cc := tsCC(b)
	payload := b[tsPayloadOffset(b):]
	restarted := tsAFFlags(b)&afFlagDiscontinuity != 0
	repeated := false
	if c.seen && !restarted && cc != (c.last+1)&0x0f {
		repeated = cc == c.last && !c.repeated
		if !repeated {
			s.idx.continuityErrors = append(s.idx.continuityErrors, continuityError{packet: i, pid: pid})
		}
	}
	s.duplicate = repeated && bytes.Equal(payload, c.payload)
	*c = pidContinuity{seen: true, last: cc, repeated: repeated, payload: payload}
}

// checkPESStart checks that each payload unit of an elementary stream starts
// with a PES header: the laps are cut at TS packets, so PES that start inside
// one cannot be looped.
func (s *scanner) checkPESStart(i, pid int, b []byte) {
	if !s.carriesPES[pid] || !tsPUSI(b) || !tsHasPayload(b) || tsScrambled(b) || tsTransportErr(b) {
		return
	}
	off := tsPayloadOffset(b)
	if off+4 <= PacketSize && b[off] == 0 && b[off+1] == 0 && b[off+2] == 1 && b[off+3] >= 0xbc {
		return
	}
	m, ok := s.misplacedStart[pid]
	if !ok {
		m = &misplacedPES{firstPacket: i}
		s.misplacedStart[pid] = m
	}
	m.count++
}

func (s *scanner) trackUnits(i, pid int, b []byte) {
	if !tsHasPayload(b) {
		return
	}
	units := s.idx.units[pid]
	switch {
	case tsPUSI(b):
		s.idx.units[pid] = append(units, payloadUnit{first: i, last: i})
	case len(units) > 0:
		units[len(units)-1].last = i
	}
}

func (s *scanner) addVideoPacket(i int, b []byte) {
	if !tsHasPayload(b) || s.duplicate {
		return
	}
	if !tsPUSI(b) {
		if s.videoOpen {
			s.videoBytes = append(s.videoBytes, b[tsPayloadOffset(b):]...)
		}
		return
	}
	s.finishVideoPES()
	h, ok := findPESHeader(b)
	if !ok {
		s.unreadablePES++
		return
	}
	if h.pts < 0 {
		s.pesWithoutPTS++
		return
	}
	s.videoOpen = true
	s.videoPicture = videoPicture{firstPacket: i, pts: s.videoPTS.unwrap(readTimestamp(b[h.pts:]))}
	s.videoPicture.dts = s.videoPicture.pts
	if h.dts >= 0 {
		s.videoPicture.dts = s.videoDTS.unwrap(readTimestamp(b[h.dts:]))
	}
	s.videoBytes = append(s.videoBytes[:0], b[h.start+h.size:]...)
}

func (s *scanner) finishVideoPES() {
	if !s.videoOpen {
		return
	}
	s.videoOpen = false
	if s.inspector == nil {
		s.inspector = newPictureInspector(s.idx.videoCodec)
	}
	s.videoPicture.info = s.inspector.inspect(s.videoBytes)
	s.idx.pictures = append(s.idx.pictures, s.videoPicture)
}

func (s *scanner) addAudioPacket(slot, i int, b []byte) {
	a := &s.audio[slot]
	st := &s.idx.audio[slot]
	// A lap counts its audio packets afresh, so a packet sent twice would be
	// two packets of payload there: it is left out.
	if !tsHasPayload(b) || s.duplicate {
		return
	}
	off := tsPayloadOffset(b)
	if !tsPUSI(b) {
		if a.open {
			a.endOffset += PacketSize - off
			st.chunks = append(st.chunks, payloadChunk{packet: i, endOffset: a.endOffset})
			a.bytes = append(a.bytes, b[off:]...)
		}
		return
	}
	s.finishAudioPES(slot, false)
	h, ok := findPESHeader(b)
	if !ok {
		s.unreadablePES++
		return
	}
	if h.pts < 0 {
		s.pesWithoutPTS++
		return
	}
	a.open, a.headerSize, a.endOffset = true, h.size, PacketSize-off
	st.pes = append(st.pes, audioPES{
		firstPacket: i,
		pts:         a.pts.unwrap(readTimestamp(b[h.pts:])),
		firstFrame:  len(st.frames),
		firstChunk:  len(st.chunks),
	})
	st.chunks = append(st.chunks, payloadChunk{packet: i, endOffset: a.endOffset})
	a.bytes = append(a.bytes[:0], b[h.start+h.size:]...)
}

func (s *scanner) finishAudioPES(slot int, endOfStream bool) {
	a := &s.audio[slot]
	st := &s.idx.audio[slot]
	if !a.open {
		return
	}
	a.open = false
	pes := &st.pes[len(st.pes)-1]
	pes.chunkCount = len(st.chunks) - pes.firstChunk
	if st.codec == audioUnknown {
		return
	}
	payload := a.bytes
	off := 0
	elapsed := int64(0)
	for off < len(payload) {
		h, ok := parseAudioFrameHeader(st.codec, payload[off:])
		if !ok || off+h.size > len(payload) || (st.sampleRate != 0 && h.sampleRate != st.sampleRate) {
			// A frame that continues in the next PES, another sample rate, or
			// what a lost packet leaves. The last PES of the file may just be cut
			// short, and keeps the frames before the cut.
			if !endOfStream {
				if a.damaged == 0 {
					a.firstDamaged = pes.firstPacket
				}
				a.damaged++
				pes.damaged = true
				// One frame, whose start and duration finalizeAudio sets.
				st.frames = append(st.frames[:pes.firstFrame], audioFrame{endOffset: a.endOffset})
			}
			break
		}
		if st.sampleRate == 0 {
			st.sampleRate, st.timeScale = h.sampleRate, int64(h.sampleRate)
		}
		end := a.headerSize + off + h.size
		if h.sameTimeAsPrevious && len(st.frames) > pes.firstFrame {
			st.frames[len(st.frames)-1].endOffset = end
		} else {
			d := int64(h.samples) * ticksPerSec
			st.frames = append(st.frames, audioFrame{start: pes.pts*st.timeScale + elapsed, duration: d, endOffset: end})
			elapsed += d
		}
		off += h.size
	}
	pes.frameCount = len(st.frames) - pes.firstFrame
}

// finalizeAudio makes each PES whose frames cannot be read one frame that lasts
// until the next PES starts: a lap ends before or after it, as after a lost
// packet. When that is most PES, the frames cannot be followed at all, and the
// whole stream is cut at PES boundaries instead.
func (s *scanner) finalizeAudio(slot int) {
	st := &s.idx.audio[slot]
	a := &s.audio[slot]
	st.frameLevel = st.codec != audioUnknown && len(st.frames) > a.damaged && 2*a.damaged <= len(st.pes)
	if st.frameLevel {
		for i := range st.pes {
			p := &st.pes[i]
			if !p.damaged {
				continue
			}
			f := &st.frames[p.firstFrame]
			f.start = p.pts * st.timeScale
			if next := p.firstFrame + 1; next < len(st.frames) {
				f.duration = max(st.frames[next].start-f.start, 0)
			}
		}
		if a.damaged > 0 {
			s.idx.warnings = append(s.idx.warnings, fmt.Sprintf("audio PID %d: the frames of %d of %d PES cannot be "+
				"read, the first at packet %d; no lap ends inside them", st.pid, a.damaged, len(st.pes), a.firstDamaged))
		}
		return
	}
	if st.codec != audioUnknown {
		s.idx.warnings = append(s.idx.warnings, fmt.Sprintf(
			"audio PID %d: frames are not aligned to PES or change format; laps end on PES boundaries", st.pid))
	}
	st.timeScale = 1
	st.frames = st.frames[:0]
	for i := range st.pes {
		p := &st.pes[i]
		d := int64(0)
		if i+1 < len(st.pes) {
			d = st.pes[i+1].pts - p.pts
		} else if len(st.frames) > 0 {
			d = st.frames[len(st.frames)-1].duration
		}
		p.firstFrame, p.frameCount = i, 1
		st.frames = append(st.frames, audioFrame{start: p.pts, duration: d,
			endOffset: st.chunks[p.firstChunk+p.chunkCount-1].endOffset})
	}
}

func (s *scanner) addDataPacket(slot, i int, b []byte) {
	a := &s.data[slot]
	st := &s.idx.data[slot]
	// As for audio, a packet sent twice is left out: a lap counts the packets
	// it places afresh.
	if !tsHasPayload(b) || s.duplicate || st.timing != dataByPTS {
		return
	}
	off := tsPayloadOffset(b)
	if !tsPUSI(b) {
		if a.open {
			a.endOffset += PacketSize - off
			st.chunks = append(st.chunks, payloadChunk{packet: i, endOffset: a.endOffset})
		}
		return
	}
	s.finishDataPES(slot)
	h, ok := findPESHeader(b)
	switch {
	case !ok:
		st.timing = dataUnreadableHeader
		return
	case h.pts < 0:
		st.timing = dataNoPTS
		return
	}
	pts := a.pts.unwrap(readTimestamp(b[h.pts:]))
	if len(st.pes) > 0 && pts < st.pes[len(st.pes)-1].pts {
		st.timing = dataOutOfOrder
		return
	}
	a.open, a.endOffset = true, PacketSize-off
	st.pes = append(st.pes, dataPES{firstPacket: i, pts: pts, firstChunk: len(st.chunks)})
	st.chunks = append(st.chunks, payloadChunk{packet: i, endOffset: a.endOffset})
}

func (s *scanner) finishDataPES(slot int) {
	a := &s.data[slot]
	if !a.open {
		return
	}
	a.open = false
	st := &s.idx.data[slot]
	pes := &st.pes[len(st.pes)-1]
	pes.chunkCount = len(st.chunks) - pes.firstChunk
}

// maxFieldPairGap: the second field of a frame is shown one field period after
// the first, at most 1/48 s, and never further apart than this.
const maxFieldPairGap = ticksPerSec / 24

// pairFields marks the second field of each frame coded as two field pictures:
// a field that follows an unpaired field of the other parity in decode order,
// as MPEG-2 and H.264 require and H.265 encoders do, and is shown just after it.
// The fields of a frame share an H.264 frame_num and an MPEG-2
// temporal_reference, and an H.264 IDR or an MPEG-2 field after a sequence
// header is a first field: a field lost from the source leaves its partner
// unpaired, not the pairing out of step.
func (s *scanner) pairFields() {
	pics := s.idx.pictures
	codec := s.idx.videoCodec
	pairs := func(first, second *videoPicture) bool {
		a, b := &first.info, &second.info
		if a.field == b.field || second.pts <= first.pts || second.pts-first.pts > maxFieldPairGap ||
			a.temporalRef != b.temporalRef {
			return false
		}
		switch codec {
		case CODEC_AVC:
			return b.accessPoint != apClosedGOP &&
				(a.numbering == nil || b.numbering == nil || a.numbering.frameNum == b.numbering.frameNum)
		case CODEC_MPEG2V:
			return !b.parameterSets
		}
		return true
	}
	open := -1
	for i := range pics {
		switch {
		case !pics[i].info.isField():
			open = -1
		case open >= 0 && pairs(&pics[open], &pics[i]):
			pics[i].info.secondField = true
			open = -1
		default:
			open = i
		}
	}
}

// promoteMPEG2ClosedGOPs makes an open-GOP MPEG-2 I picture that no B picture
// follows a closed-GOP access point: it has no leading pictures. A lone field
// is followed by its other field first.
func (s *scanner) promoteMPEG2ClosedGOPs() {
	if s.idx.videoCodec != CODEC_MPEG2V {
		return
	}
	pics := s.idx.pictures
	for i := range pics {
		if pics[i].info.accessPoint != apOpenGOP {
			continue
		}
		next := i + 1
		if pics[i].info.isField() {
			next++
		}
		if next < len(pics) && !pics[next].info.bPicture {
			pics[i].info.accessPoint = apClosedGOP
		}
	}
}

func (s *scanner) finish() error {
	idx := s.idx
	idx.nullsBefore = append(idx.nullsBefore, s.nulls)
	s.finishVideoPES()
	for slot := range s.audio {
		s.finishAudioPES(slot, true)
		s.finalizeAudio(slot)
	}
	for slot := range s.data {
		s.finishDataPES(slot)
		if st := &idx.data[slot]; st.timing != dataByPTS {
			st.pes, st.chunks = nil, nil
		}
	}
	s.pairFields()
	s.promoteMPEG2ClosedGOPs()
	for pid, a := range s.scte35 {
		idx.scte35Sections[pid] = a.sections
	}

	if s.invalid*1_000_000 > idx.packetCount*maxInvalidPerMillion {
		return fmt.Errorf("%d of %d packets lack the 0x47 sync byte: not a 188-byte TS", s.invalid, idx.packetCount)
	}
	if s.invalid > 0 {
		idx.warnings = append(idx.warnings,
			fmt.Sprintf("%d packets lack the sync byte; they are replaced by null packets", s.invalid))
	}
	if s.corrupt > 0 {
		idx.warnings = append(idx.warnings, fmt.Sprintf(
			"%d packets have an adaptation field longer than the packet; they are replaced by null packets", s.corrupt))
	}
	// Video and audio are cut at their PES, so a PES starting inside a packet
	// cannot be looped; other streams only break at the seam.
	misplacedOn := func(looped bool) string {
		var where []string
		pids := make([]int, 0, len(s.misplacedStart))
		for pid := range s.misplacedStart {
			pids = append(pids, pid)
		}
		sort.Ints(pids)
		for _, pid := range pids {
			if s.role[pid].looped() != looped {
				continue
			}
			m := s.misplacedStart[pid]
			where = append(where, fmt.Sprintf("PID %d (%d, the first at packet %d)", pid, m.count, m.firstPacket))
		}
		return strings.Join(where, ", ")
	}
	if where := misplacedOn(true); where != "" {
		return fmt.Errorf("payload units that do not start with a PES header on %s: the PES do not start at TS "+
			"packets, so this content cannot be looped", where)
	}
	if where := misplacedOn(false); where != "" {
		idx.warnings = append(idx.warnings, fmt.Sprintf(
			"payload units that do not start with a PES header on %s: those streams break at the seam", where))
	}
	if len(idx.continuityErrors) > 0 {
		perPID := make(map[int]int)
		for _, e := range idx.continuityErrors {
			perPID[e.pid]++
		}
		pids := make([]int, 0, len(perPID))
		for pid := range perPID {
			pids = append(pids, pid)
		}
		sort.Ints(pids)
		var where []string
		for _, pid := range pids {
			where = append(where, fmt.Sprintf("PID %d: %d", pid, perPID[pid]))
		}
		idx.warnings = append(idx.warnings, fmt.Sprintf("the source has %d continuity errors (%s), the first at packet %d",
			len(idx.continuityErrors), strings.Join(where, ", "), idx.continuityErrors[0].packet))
	}
	if idx.videoPID < 0 {
		return fmt.Errorf("the program has no H.262, H.264 or H.265 video")
	}
	if len(idx.pictures) == 0 {
		return fmt.Errorf("no video PES found")
	}
	if s.pesWithoutPTS > 0 {
		return fmt.Errorf("%d video or audio PES carry no PTS; every one must", s.pesWithoutPTS)
	}
	if s.unreadablePES > 0 {
		idx.warnings = append(idx.warnings, fmt.Sprintf(
			"%d video or audio PES headers are scrambled or split across packets; those PES are ignored", s.unreadablePES))
	}
	return nil
}

// scanStream scans a TS once. It fails when the input is not 188-byte TS, has
// no PMT or video, when a video or audio PES has no PTS, or when an elementary
// stream's payload unit does not start with a PES header: its PES do not start
// at TS packets.
func scanStream(ctx context.Context, data []byte) (*streamIndex, error) {
	if len(data) < PacketSize || data[0] != syncByte {
		return nil, fmt.Errorf("the input does not start with a 188-byte TS packet")
	}
	if isBTS(data) {
		return nil, fmt.Errorf("the input is an ISDB-T BTS (204-byte packets); only a 188-byte TS can be looped")
	}
	n := len(data) / PacketSize // whole packets only: a cut-short last packet is not looped
	p, err := findProgram(data, n)
	if err != nil {
		return nil, err
	}
	idx := &streamIndex{
		packetCount:    n,
		units:          make(map[int][]payloadUnit),
		scte35Sections: make(map[int][]SCTE35Section),
		nullsBefore:    make([]uint32, 0, n+1),
	}
	s := newScanner(idx, p)
	for i := 0; i < n; i++ {
		if i&0xffff == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		s.addPacket(i, data[i*PacketSize:(i+1)*PacketSize])
	}
	if err := s.finish(); err != nil {
		return nil, err
	}
	return idx, nil
}
