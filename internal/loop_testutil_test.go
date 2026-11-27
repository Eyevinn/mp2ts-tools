package internal

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"strings"
	"testing"

	"github.com/Comcast/gots/v2"
	"github.com/Eyevinn/mp4ff/bits"
)

// Synthetic streams for the mp2ts-loop tests: a constant-rate TS with an access
// point every second and two AAC streams, one with three ADTS frames per PES
// and one with a frame per PES a quarter frame later; each audio frame carries
// its source PTS.
const (
	tVideoPID          = 0x100
	tAudioPID          = 0x101
	tAudio2PID         = 0x102
	tDataPID           = 0x103
	tSCTE35PID         = 0x1f0
	tPMTPID            = 0x1000
	tFrameTicks        = 3600 // 25 fps
	tAudioFrameTicks   = 1920 // AAC, 1024 samples at 48 kHz
	tAudioFrameSize    = 300
	tAudioFramesPerPES = 3
	tBitrate           = 1_500_000
	tStartPTS          = 900_000
	tSourcePTSOffset   = 7 // in each ADTS frame, after its header
	tDataPayloadSize   = 16
	tMuxDelay          = 400 * 27_000
)

type streamOptions struct {
	seconds int // default 3
	// audioMuxDelay: audio is multiplexed this many ticks later than video of
	// the same time; negative is earlier.
	audioMuxDelay int64
	// scte35: every access point comes with a splice_insert and a time_signal
	// with a segmentation descriptor, each with its own event ID and a break of
	// half a second.
	scte35           bool
	hevc             bool // a CRA where H.264 has an IDR
	audio2StreamType byte // default ADTS
	secondProgram    bool // the PAT also lists program 2, which has no PMT
	rate             FrameRate
	gop              int // frames per GOP, default 25
	// fields: H.264 frames coded as two field pictures (PAFF), each in its own
	// PES half a frame period after the other, with parameter sets and slice
	// headers that parse. bottomFirst sends the bottom field first.
	fields      bool
	bottomFirst bool
	// data: a private PES stream, as SMPTE ST 2038 ANC, with a PES for each
	// picture that carries that picture's PTS in its header and its first four
	// payload bytes, multiplexed dataFramesAhead frames before the picture;
	// negative is after. dataWithoutPTS leaves the PTS out of the header.
	data            bool
	dataFramesAhead int
	dataWithoutPTS  bool
	// leadingPictures: each IDR is shown after the two pictures decoded after it,
	// as B pictures that predict only from it, so a loop's pictures start before
	// its IDR.
	leadingPictures bool
}

func adtsHeader(frameLength int) []byte {
	return []byte{0xff, 0xf1, 0x4c, byte(0x80 | frameLength>>11&0x3), byte(frameLength >> 3),
		byte((frameLength&0x7)<<5 | 0x1f), 0xfc}
}

func h264AccessUnit(idr bool, size int) []byte {
	b := []byte{0, 0, 0, 1, 0x09, 0xf0} // access unit delimiter
	if idr {
		b = append(b, 0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x1e, 0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80)
		b = append(b, 0, 0, 0, 1, 0x65)
	} else {
		b = append(b, 0, 0, 0, 1, 0x41)
	}
	for len(b) < size {
		b = append(b, 0xaa)
	}
	return b
}

// nalUnitOf is a NAL unit with a start code, a one-byte header and an RBSP
// written by write, with emulation prevention bytes.
func nalUnitOf(header byte, write func(w *bits.EBSPWriter)) []byte {
	var buf bytes.Buffer
	w := bits.NewEBSPWriter(&buf)
	write(w)
	w.WriteRbspTrailingBits()
	return append([]byte{0, 0, 0, 1, header}, buf.Bytes()...)
}

// Parameter sets of 64x64 interlaced H.264 Main profile, coded as fields
// (frame_mbs_only_flag 0), with pic_struct in pic_timing (pic_struct_present_flag).
const (
	tLog2MaxFrameNum = 4
	tLog2MaxPOCLSB   = 6
)

func h264FieldSPS() []byte {
	return nalUnitOf(0x67, func(w *bits.EBSPWriter) {
		w.Write(77, 8)      // profile_idc: Main
		w.Write(0, 8)       // constraint flags
		w.Write(30, 8)      // level_idc
		w.WriteExpGolomb(0) // seq_parameter_set_id
		w.WriteExpGolomb(tLog2MaxFrameNum - 4)
		w.WriteExpGolomb(0) // pic_order_cnt_type
		w.WriteExpGolomb(tLog2MaxPOCLSB - 4)
		w.WriteExpGolomb(2) // max_num_ref_frames
		w.Write(0, 1)       // gaps_in_frame_num_value_allowed_flag
		w.WriteExpGolomb(3) // pic_width_in_mbs_minus1
		w.WriteExpGolomb(1) // pic_height_in_map_units_minus1
		w.Write(0, 1)       // frame_mbs_only_flag
		w.Write(0, 1)       // mb_adaptive_frame_field_flag
		w.Write(1, 1)       // direct_8x8_inference_flag
		w.Write(0, 1)       // frame_cropping_flag
		w.Write(1, 1)       // vui_parameters_present_flag
		w.Write(0, 5)       // aspect ratio, overscan, video signal, chroma loc, timing info
		w.Write(0, 2)       // nal_hrd_parameters_present_flag, vcl_hrd_parameters_present_flag
		w.Write(1, 1)       // pic_struct_present_flag
		w.Write(0, 1)       // bitstream_restriction_flag
	})
}

func h264FieldPPS() []byte {
	return nalUnitOf(0x68, func(w *bits.EBSPWriter) {
		w.WriteExpGolomb(0) // pic_parameter_set_id
		w.WriteExpGolomb(0) // seq_parameter_set_id
		w.Write(0, 2)       // entropy_coding_mode_flag, bottom_field_pic_order_in_frame_present_flag
		w.WriteExpGolomb(0) // num_slice_groups_minus1
		w.WriteExpGolomb(0) // num_ref_idx_l0_default_active_minus1
		w.WriteExpGolomb(0) // num_ref_idx_l1_default_active_minus1
		w.Write(0, 3)       // weighted_pred_flag, weighted_bipred_idc
		w.WriteExpGolomb(0) // pic_init_qp_minus26
		w.WriteExpGolomb(0) // pic_init_qs_minus26
		w.WriteExpGolomb(0) // chroma_qp_index_offset
		w.Write(1, 1)       // deblocking_filter_control_present_flag
		w.Write(0, 2)       // constrained_intra_pred_flag, redundant_pic_cnt_present_flag
	})
}

// h264FieldAccessUnit is one field of a frame: an IDR field with the parameter
// sets, or a P field, with a pic_timing SEI (pic_struct 1 top, 2 bottom).
func h264FieldAccessUnit(idr, bottom bool, frameNum, poc uint, size int) []byte {
	b := []byte{0, 0, 0, 1, 0x09, 0xf0} // access unit delimiter
	if idr {
		b = append(append(b, h264FieldSPS()...), h264FieldPPS()...)
	}
	picStruct := uint(1)
	if bottom {
		picStruct = 2
	}
	b = append(b, nalUnitOf(0x06, func(w *bits.EBSPWriter) {
		w.Write(1, 8)         // payloadType: pic_timing
		w.Write(1, 8)         // payloadSize
		w.Write(picStruct, 4) // pic_struct
		w.Write(0, 1)         // clock_timestamp_flag
		w.Write(0b100, 3)     // payload byte alignment
	})...)
	header := byte(0x61) // nal_ref_idc 3, non-IDR slice
	if idr {
		header = 0x65
	}
	b = append(b, nalUnitOf(header, func(w *bits.EBSPWriter) {
		w.WriteExpGolomb(0)  // first_mb_in_slice
		sliceType := uint(5) // P
		if idr {
			sliceType = 7 // I
		}
		w.WriteExpGolomb(sliceType)
		w.WriteExpGolomb(0) // pic_parameter_set_id
		w.Write(frameNum, tLog2MaxFrameNum)
		w.Write(1, 1) // field_pic_flag
		bottomBit := uint(0)
		if bottom {
			bottomBit = 1
		}
		w.Write(bottomBit, 1)
		if idr {
			w.WriteExpGolomb(0) // idr_pic_id
		}
		w.Write(poc, tLog2MaxPOCLSB)
		if !idr {
			w.Write(0, 2) // num_ref_idx_active_override_flag, ref_pic_list_modification_flag_l0
			w.Write(0, 1) // adaptive_ref_pic_marking_mode_flag
		} else {
			w.Write(0, 2) // no_output_of_prior_pics_flag, long_term_reference_flag
		}
		w.WriteExpGolomb(0) // slice_qp_delta
		w.WriteExpGolomb(1) // disable_deblocking_filter_idc
		w.Write(0xaa, 8)
	})...)
	for len(b) < size {
		b = append(b, 0xaa)
	}
	return b
}

// h265AccessUnit is a CRA with its parameter sets, or a trailing picture.
func h265AccessUnit(cra bool, size int) []byte {
	b := []byte{0, 0, 0, 1, 35 << 1, 1, 0x50} // access unit delimiter
	if cra {
		for _, typ := range []byte{32, 33, 34} { // VPS, SPS, PPS
			b = append(b, 0, 0, 0, 1, typ<<1, 1, 0x0c, 0x01)
		}
		b = append(b, 0, 0, 0, 1, 21<<1, 1)
	} else {
		b = append(b, 0, 0, 0, 1, 1<<1, 1)
	}
	for len(b) < size {
		b = append(b, 0xaa)
	}
	return b
}

// psiSection builds a section with section syntax and its CRC.
func psiSection(tableID byte, idExt int, body []byte) []byte {
	n := 5 + len(body) + 4
	s := []byte{tableID, 0xb0 | byte(n>>8), byte(n), byte(idExt >> 8), byte(idExt), 0xc1, 0, 0}
	s = append(s, body...)
	return append(s, gots.ComputeCRC(s)...)
}

// pesPackets carries a PES in TS packets of pid; the last is padded with
// adaptation-field stuffing. dts < 0 leaves it out.
func pesPackets(pid int, streamID byte, pts, dts int64, payload []byte) [][]byte {
	hdr := []byte{0, 0, 1, streamID, 0, 0, 0x80, 0x80, 5, 0x20, 0, 0, 0, 0}
	if dts >= 0 {
		hdr[7], hdr[8], hdr[9] = 0xc0, 10, 0x30
		hdr = append(hdr, 0x10, 0, 0, 0, 0)
		writeTimestamp(hdr[14:], uint64(dts))
	}
	writeTimestamp(hdr[9:], uint64(pts))
	length := len(hdr) - 6 + len(payload)
	if length > 0xffff {
		length = 0
	}
	binary.BigEndian.PutUint16(hdr[4:], uint16(length))
	return tsPackets(pid, append(hdr, payload...))
}

// dataPESPackets carries a private_stream_1 PES of the test's data stream,
// with a PTS unless pts < 0.
func dataPESPackets(pts int64, payload []byte) [][]byte {
	hdr := []byte{0, 0, 1, 0xbd, 0, 0, 0x80, 0, 0}
	if pts >= 0 {
		hdr[7], hdr[8] = 0x80, 5
		hdr = append(hdr, 0x20, 0, 0, 0, 0)
		writeTimestamp(hdr[9:], uint64(pts))
	}
	binary.BigEndian.PutUint16(hdr[4:], uint16(len(hdr)-6+len(payload)))
	return tsPackets(tDataPID, append(hdr, payload...))
}

// tsPackets carries a payload unit in TS packets of pid.
func tsPackets(pid int, unit []byte) [][]byte {
	var pkts [][]byte
	for off := 0; off < len(unit); off += maxPayload {
		n := min(maxPayload, len(unit)-off)
		p := make([]byte, PacketSize)
		p[0], p[1], p[2], p[3] = syncByte, byte(pid>>8&0x1f), byte(pid), 0x10
		if off == 0 {
			p[1] |= pusiBit
		}
		copy(p[tsHeader:], unit[off:off+n])
		shrinkPayload(p, n)
		pkts = append(pkts, p)
	}
	return pkts
}

// testMuxer sends packets at a constant rate, with the PAT and PMT every 100
// ms, a PCR every 20 ms in a packet of its own on the PCR PID, queued packets
// otherwise, and null packets when none are queued.
type testMuxer struct {
	out     []byte
	queue   [][]byte
	sent    int64
	last    map[int]byte
	psi     [][]byte
	pcrPID  int
	base    int64
	nextPSI int64
	nextPCR int64
}

func (m *testMuxer) clock() int64 { return m.base + m.sent*PacketSize*8*27_000_000/tBitrate }

func (m *testMuxer) emit(p []byte) {
	p = append([]byte(nil), p...)
	if pid := tsPID(p); pid != nullPID {
		last, ok := m.last[pid]
		if !ok {
			last = 15
		}
		if tsHasPayload(p) {
			last = (last + 1) & 0x0f
		}
		m.last[pid] = last
		p[3] = p[3]&0xf0 | last
	}
	m.out = append(m.out, p...)
	m.sent++
}

func (m *testMuxer) runUntil(t int64) {
	for now := m.clock(); now < t; now = m.clock() {
		switch {
		case now >= m.nextPSI:
			for _, p := range m.psi {
				m.emit(p)
			}
			m.nextPSI += 100 * 27_000
		case now >= m.nextPCR:
			p := make([]byte, PacketSize)
			writePCRPacket(p, m.pcrPID, 0)
			writePCR(p[pcrFieldOffset:], uint64(now))
			m.emit(p)
			m.nextPCR += 20 * 27_000
		case len(m.queue) > 0:
			m.emit(m.queue[0])
			m.queue = m.queue[1:]
		default:
			p := make([]byte, PacketSize)
			writeNullPacket(p)
			m.emit(p)
		}
	}
}

func buildStream(t *testing.T, o streamOptions) []byte {
	t.Helper()
	if o.seconds == 0 {
		o.seconds = 3
	}
	if o.audio2StreamType == 0 {
		o.audio2StreamType = stADTS
	}
	if !o.rate.known() {
		o.rate = FrameRate{25, 1}
	}
	if o.gop == 0 {
		o.gop = 25
	}
	pat := []byte{0, 1, 0xe0 | tPMTPID>>8, tPMTPID & 0xff}
	if o.secondProgram {
		pat = append(pat, 0, 2, 0xe0|0x1010>>8, 0x1010&0xff)
	}
	videoType := byte(stAVC)
	if o.hevc {
		videoType = stHEVC
	}
	pmt := []byte{0xe0 | tVideoPID>>8, tVideoPID & 0xff, 0xf0, 0}
	for _, es := range [][2]int{{int(videoType), tVideoPID}, {stADTS, tAudioPID}, {int(o.audio2StreamType), tAudio2PID}} {
		pmt = append(pmt, byte(es[0]), 0xe0|byte(es[1]>>8), byte(es[1]), 0xf0, 0)
	}
	if o.data {
		pmt = append(pmt, stPrivateData, 0xe0|tDataPID>>8, tDataPID&0xff, 0xf0, 0)
	}
	if o.scte35 {
		pmt = append(pmt, stSCTE35, 0xe0|tSCTE35PID>>8, tSCTE35PID&0xff, 0xf0, 0)
	}
	m := &testMuxer{last: make(map[int]byte), pcrPID: tVideoPID, base: tStartPTS*pcrPerTick - tMuxDelay}
	m.nextPSI, m.nextPCR = m.base, m.base
	m.psi = append(tsPackets(0, append([]byte{0}, psiSection(0x00, 1, pat)...)),
		tsPackets(tPMTPID, append([]byte{0}, psiSection(0x02, 1, pmt)...))...)

	frameTick := func(f int64) int64 { return (2*f*ticksPerSec*o.rate.Den + o.rate.Num) / (2 * o.rate.Num) }
	frames := int64(o.seconds) * o.rate.Num / o.rate.Den
	// shownIn is the frame period a picture is shown in, two after it is decoded.
	shownIn := func(f int64) int64 {
		inGOP := (f + int64(o.gop) - 5) % int64(o.gop)
		switch {
		case !o.leadingPictures || inGOP > 2:
			return f + 2
		case inGOP == 0:
			return f + 4
		}
		return f + 1
	}
	type track struct {
		pid           int
		framesPerPES  int
		next          int64
		audioStreamID byte
	}
	tracks := []*track{{tAudioPID, tAudioFramesPerPES, -tAudioFrameTicks, 0xc0},
		{tAudio2PID, 1, -tAudioFrameTicks + tAudioFrameTicks/4, 0xc1}}
	for f := int64(0); f < frames; f++ {
		tick := frameTick(f)
		m.runUntil((tStartPTS+tick)*pcrPerTick - tMuxDelay)
		// Like a capture, the stream starts a few frames before its first IDR.
		idr := f%int64(o.gop) == 5
		size := 3000
		if idr {
			size = 12000
		}
		pts := tStartPTS + frameTick(shownIn(f))
		switch {
		case o.fields:
			// Frame numbers count from the IDR; picture order counts are 2 a frame.
			n := uint((f - 5 + int64(o.gop)) % int64(o.gop))
			half := (frameTick(f+1) - tick) / 2
			for k, bottom := range []bool{o.bottomFirst, !o.bottomFirst} {
				field := h264FieldAccessUnit(idr && k == 0, bottom, n%(1<<tLog2MaxFrameNum),
					(2*n+uint(k))%(1<<tLog2MaxPOCLSB), size/2)
				m.queue = append(m.queue, pesPackets(tVideoPID, 0xe0, pts+int64(k)*half,
					tStartPTS+tick+int64(k)*half, field)...)
			}
		case o.hevc:
			m.queue = append(m.queue, pesPackets(tVideoPID, 0xe0, pts, tStartPTS+tick, h265AccessUnit(idr, size))...)
		default:
			m.queue = append(m.queue, pesPackets(tVideoPID, 0xe0, pts, tStartPTS+tick, h264AccessUnit(idr, size))...)
		}
		if picture := f + int64(o.dataFramesAhead); o.data && picture >= 0 {
			dataPTS := tStartPTS + frameTick(picture+2)
			payload := bytes.Repeat([]byte{0xaa}, tDataPayloadSize)
			binary.BigEndian.PutUint32(payload, uint32(dataPTS))
			if o.dataWithoutPTS {
				dataPTS = -1
			}
			m.queue = append(m.queue, dataPESPackets(dataPTS, payload)...)
		}
		if o.scte35 && idr {
			cue := uint32(f / int64(o.gop))
			m.queue = append(m.queue, tsPackets(tSCTE35PID, append([]byte{0}, spliceInsert(100+cue, true, pts, 45_000)...))...)
			m.queue = append(m.queue, tsPackets(tSCTE35PID, append([]byte{0},
				timeSignal(pts, segDesc(1000+cue, 0x34, 45_000))...))...)
		}
		for _, tr := range tracks {
			for tr.next+o.audioMuxDelay <= tick {
				payload := make([]byte, 0, tAudioFrameSize*tr.framesPerPES)
				for k := 0; k < tr.framesPerPES; k++ {
					frame := bytes.Repeat([]byte{0x55}, tAudioFrameSize)
					copy(frame, adtsHeader(tAudioFrameSize))
					binary.BigEndian.PutUint32(frame[tSourcePTSOffset:], uint32(tStartPTS+tr.next+int64(k)*tAudioFrameTicks))
					payload = append(payload, frame...)
				}
				m.queue = append(m.queue, pesPackets(tr.pid, tr.audioStreamID, tStartPTS+tr.next, -1, payload)...)
				tr.next += tAudioFrameTicks * int64(tr.framesPerPES)
			}
		}
	}
	m.runUntil((tStartPTS+frameTick(frames))*pcrPerTick - tMuxDelay)
	return m.out
}

// variableRate returns the stream without its null packets and with a PCR that
// runs clockRate as fast as the timestamps: variable-rate, and a lap sent
// faster than it lasts when the clock runs slow.
func variableRate(stream []byte, clockRate float64) []byte {
	var out []byte
	first := int64(-1)
	for i := 0; i < len(stream)/PacketSize; i++ {
		p := packetAt(stream, i)
		if tsPID(p) == nullPID {
			continue
		}
		out = append(out, p...)
		if q := out[len(out)-PacketSize:]; tsCarriesPCR(q) {
			pcr := int64(readPCR(q[pcrFieldOffset:]))
			if first < 0 {
				first = pcr
			}
			writePCR(q[pcrFieldOffset:], uint64(first+int64(float64(pcr-first)*clockRate+0.5)))
		}
	}
	return out
}

// countingDTS returns the stream with its video DTS replaced by a picture
// counter, as a muxer writes when it takes a frame number for a 90 kHz time.
func countingDTS(stream []byte) []byte {
	out := append([]byte(nil), stream...)
	first, pictures := int64(-1), int64(0)
	for i := 0; i < len(out)/PacketSize; i++ {
		p := packetAt(out, i)
		if tsPID(p) != tVideoPID {
			continue
		}
		if h, ok := findPESHeader(p); ok && h.dts >= 0 {
			if first < 0 {
				first = int64(readTimestamp(p[h.dts:]))
			}
			writeTimestamp(p[h.dts:], uint64(first+pictures))
			pictures++
		}
	}
	return out
}

// asBTS follows each packet with 16 bytes, as an ISDB-T BTS carries its
// information and parity.
func asBTS(stream []byte) []byte {
	var out []byte
	for i := 0; i < len(stream)/PacketSize; i++ {
		out = append(out, packetAt(stream, i)...)
		out = append(out, bytes.Repeat([]byte{0xff}, btsPacketSize-PacketSize)...)
	}
	return out
}

// nullsMovedBefore returns the stream with count of the null packets between
// begin and end moved to just before begin, and its PCR restamped to stay
// linear: a constant-rate stream whose buffer is fuller at begin than at end, so
// a loop between them is padded with null packets before its seam.
func nullsMovedBefore(t *testing.T, stream []byte, begin, end, count int) []byte {
	t.Helper()
	var out []byte
	moved := 0
	firstPkt, lastPkt := -1, 0
	var firstPCR, lastPCR uint64
	for i := 0; i < len(stream)/PacketSize; i++ {
		if i == begin {
			for range count {
				out = append(out, make([]byte, PacketSize)...)
				writeNullPacket(out[len(out)-PacketSize:])
			}
		}
		p := packetAt(stream, i)
		if tsCarriesPCR(p) {
			lastPkt, lastPCR = i, readPCR(p[pcrFieldOffset:])
			if firstPkt < 0 {
				firstPkt, firstPCR = lastPkt, lastPCR
			}
		}
		if i >= begin && i < end && moved < count && tsPID(p) == nullPID {
			moved++
			continue
		}
		out = append(out, p...)
	}
	if moved != count || firstPkt < 0 {
		t.Fatalf("moved %d of %d null packets", moved, count)
	}
	span, packets := lastPCR-firstPCR, uint64(lastPkt-firstPkt)
	for i := 0; i < len(out)/PacketSize; i++ {
		if p := packetAt(out, i); tsCarriesPCR(p) {
			writePCR(p[pcrFieldOffset:], firstPCR+uint64(i-firstPkt)*span/packets)
		}
	}
	return out
}

// loopData prepares the loop of a stream and writes laps of it.
func loopData(t *testing.T, data []byte, opts LoopOptions, laps int) (*Loop, []byte, LapStatistics) {
	t.Helper()
	l, err := prepareLoop(context.Background(), "test", data, opts)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	stats, err := l.WriteLaps(context.Background(), []LapOutput{WriterOutput{W: &out}}, laps)
	if err != nil {
		t.Fatal(err)
	}
	return l, out.Bytes(), stats
}

// loopFile prepares the loop of a file and writes laps of it.
func loopFile(t *testing.T, path string, opts LoopOptions, laps int) (*Loop, []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	l, out, _ := loopData(t, data, opts, laps)
	return l, out
}

// continuityErrors counts the packets whose counter does not follow their PID's
// previous one, or repeat it in a packet without payload.
func continuityErrors(stream []byte) int {
	last := make(map[int]byte)
	errs := 0
	for i := 0; i < len(stream)/PacketSize; i++ {
		p := packetAt(stream, i)
		pid := tsPID(p)
		if pid == nullPID {
			continue
		}
		if l, ok := last[pid]; ok {
			want := l
			if tsHasPayload(p) {
				want = (l + 1) & 0x0f
			}
			if tsCC(p) != want {
				errs++
			}
		}
		last[pid] = tsCC(p)
	}
	return errs
}

type pesRecord struct {
	pid      int
	packet   int
	pts, dts int64
	payload  []byte // after the PES header
}

// collectPES returns the PES of the test stream's video, audio and data, in
// order; pts is -1 when a PES has none.
func collectPES(stream []byte) []*pesRecord {
	var recs []*pesRecord
	open := make(map[int]*pesRecord)
	for i := 0; i < len(stream)/PacketSize; i++ {
		p := packetAt(stream, i)
		pid := tsPID(p)
		if (pid != tVideoPID && pid != tAudioPID && pid != tAudio2PID && pid != tDataPID) || !tsHasPayload(p) {
			continue
		}
		if h, ok := findPESHeader(p); ok {
			r := &pesRecord{pid: pid, packet: i, pts: -1}
			if h.pts >= 0 {
				r.pts = int64(readTimestamp(p[h.pts:]))
			}
			r.dts = r.pts
			if h.dts >= 0 {
				r.dts = int64(readTimestamp(p[h.dts:]))
			}
			r.payload = append(r.payload, p[h.start+h.size:]...)
			open[pid] = r
			recs = append(recs, r)
		} else if r := open[pid]; r != nil {
			r.payload = append(r.payload, p[tsPayloadOffset(p):]...)
		}
	}
	return recs
}

// outputPCRs returns the PCRs on pid from packet from on.
func outputPCRs(stream []byte, pid, from int) []int64 {
	var pcrs []int64
	for i := from; i < len(stream)/PacketSize; i++ {
		if p := packetAt(stream, i); tsPID(p) == pid && tsCarriesPCR(p) {
			pcrs = append(pcrs, int64(readPCR(p[pcrFieldOffset:])))
		}
	}
	return pcrs
}

func anyHas(texts []string, part string) bool {
	for _, s := range texts {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}
