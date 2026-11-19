package internal

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/bits"
	"github.com/Eyevinn/mp4ff/hevc"
)

func makePacket(pid int, withPCR bool) []byte {
	p := make([]byte, PacketSize)
	p[0], p[1], p[2] = syncByte, byte(pid>>8&0x1f), byte(pid)
	if withPCR {
		p[3], p[4], p[5] = 0x30, 7, afFlagPCR
		writePCR(p[pcrFieldOffset:], 123_456_789)
		for i := 12; i < PacketSize; i++ {
			p[i] = byte(i)
		}
	} else {
		p[3] = 0x10
		for i := 4; i < PacketSize; i++ {
			p[i] = byte(i)
		}
	}
	return p
}

func TestShrinkPayload(t *testing.T) {
	p := makePacket(0x101, false)
	orig := append([]byte(nil), p...)
	shrinkPayload(p, 100)
	if tsPayloadOffset(p) != PacketSize-100 || !bytes.Equal(p[PacketSize-100:], orig[4:104]) || p[4] != 83 || p[5] != 0 {
		t.Errorf("payload-only packet: offset %d, AF length %d flags %#x", tsPayloadOffset(p), p[4], p[5])
	}

	p = makePacket(0x100, true)
	orig = append([]byte(nil), p...)
	shrinkPayload(p, 10)
	if !tsCarriesPCR(p) || readPCR(p[pcrFieldOffset:]) != 123_456_789 || !bytes.Equal(p[PacketSize-10:], orig[12:22]) {
		t.Error("a packet with PCR keeps it and its first payload bytes")
	}
	stripPayload(p)
	if !tsCarriesPCR(p) || tsHasPayload(p) || p[4] != 183 {
		t.Errorf("stripped PCR packet: PCR %v, payload %v, AF length %d", tsCarriesPCR(p), tsHasPayload(p), p[4])
	}

	p = makePacket(0x101, false)
	stripPayload(p)
	if tsPID(p) != nullPID {
		t.Error("removing all payload of a packet without PCR makes a null packet")
	}
}

func TestRemoveClockReferences(t *testing.T) {
	p := makePacket(0x101, true)
	p[5] |= afFlagRandomAccess
	orig := append([]byte(nil), p...)
	removeClockReferences(p)
	if tsCarriesPCR(p) || p[4] != 7 || p[5] != afFlagRandomAccess || !bytes.Equal(p[6:12], bytes.Repeat([]byte{0xff}, 6)) ||
		!bytes.Equal(p[12:], orig[12:]) {
		t.Errorf("moved packet: % x", p[:16])
	}
	// An adaptation field longer than its packet is not read past the packet.
	buf := bytes.Repeat([]byte{0xa5}, PacketSize+80)
	copy(buf, makePacket(0x101, true))
	buf[4] = 255
	if !tsOversizedAF(buf) {
		t.Error("oversized adaptation field not seen")
	}
	removeClockReferences(buf[:PacketSize])
	if !bytes.Equal(buf[PacketSize:], bytes.Repeat([]byte{0xa5}, 80)) {
		t.Error("written past the packet")
	}
}

func TestTimestamps(t *testing.T) {
	f := []byte{0x31, 0, 1, 0, 1}
	writeTimestamp(f, PtsWrap-1)
	if readTimestamp(f) != PtsWrap-1 || f[0]&0xf0 != 0x30 {
		t.Errorf("timestamp % x", f)
	}
	if wrapAdd(PtsWrap-10, 25, PtsWrap) != 15 || wrapAdd(5, -10, PtsWrap) != PtsWrap-5 {
		t.Error("wrapAdd")
	}
	u := unwrapper{wrap: ptsWrapU}
	for i, c := range []struct {
		in   uint64
		want int64
	}{{PtsWrap - 100, PtsWrap - 100}, {50, PtsWrap + 50}, {PtsWrap - 10, PtsWrap - 10}} {
		if got := u.unwrap(c.in); got != c.want {
			t.Errorf("unwrap %d: %d, want %d", i, got, c.want)
		}
	}
}

func TestAudioFrameHeaders(t *testing.T) {
	if h, ok := parseAudioFrameHeader(audioADTS, adtsHeader(371)); !ok || h.size != 371 || h.samples != 1024 || h.sampleRate != 48_000 {
		t.Errorf("ADTS %+v", h)
	}
	// MPEG-1 layer II, 192 kbit/s, 48 kHz: 144 * 192000 / 48000 = 576 bytes.
	layer2 := []byte{0xff, 0xfd, 0xa4, 0x00}
	if h, ok := parseAudioFrameHeader(audioMPEG, layer2); !ok || h.size != 576 || h.samples != 1152 || h.sampleRate != 48_000 {
		t.Errorf("MP2 %+v", h)
	}
	// AC-3, 48 kHz, frmsizecod 28 (384 kbit/s): 768 words.
	if h, ok := parseAudioFrameHeader(audioAC3, []byte{0x0b, 0x77, 0, 0, 0x1c, 0x40}); !ok || h.size != 1536 ||
		h.samples != 1536 || h.sampleRate != 48_000 {
		t.Errorf("AC-3 %+v", h)
	}
	// E-AC-3 independent substream, frmsiz 383, 48 kHz, six blocks; then a dependent one.
	if h, ok := parseAudioFrameHeader(audioAC3, []byte{0x0b, 0x77, 0x01, 0x7f, 0x30, 0x80}); !ok || h.size != 768 ||
		h.samples != 1536 || h.sameTimeAsPrevious {
		t.Errorf("E-AC-3 %+v", h)
	}
	if h, _ := parseAudioFrameHeader(audioAC3, []byte{0x0b, 0x77, 0x41, 0x7f, 0x30, 0x80}); !h.sameTimeAsPrevious {
		t.Error("E-AC-3 dependent substream")
	}
	if _, ok := parseAudioFrameHeader(audioADTS, layer2); ok {
		t.Error("MP2 read as ADTS")
	}
}

func TestMarkSeam(t *testing.T) {
	cases := []struct {
		name    string
		codec   Codec
		data    []byte
		changed int
		want    []byte // nil: unchanged
	}{
		{"HEVC CRA becomes BLA_W_LP, IDR is left alone", CODEC_HEVC,
			[]byte{0, 0, 1, 21 << 1, 1, 0xaa, 0, 0, 1, 21 << 1, 1, 0xbb, 0, 0, 1, 19 << 1, 1}, 2,
			[]byte{0, 0, 1, 16 << 1, 1, 0xaa, 0, 0, 1, 16 << 1, 1, 0xbb, 0, 0, 1, 19 << 1, 1}},
		{"open MPEG-2 GOP gets broken_link", CODEC_MPEG2V,
			[]byte{0, 0, 1, 0xb3, 1, 2, 3, 0, 0, 1, 0xb8, 0, 8, 0, 0x00, 0, 0, 1, 0x00, 0, 0x08}, 1,
			[]byte{0, 0, 1, 0xb3, 1, 2, 3, 0, 0, 1, 0xb8, 0, 8, 0, 0x20, 0, 0, 1, 0x00, 0, 0x08}},
		{"closed MPEG-2 GOP does not", CODEC_MPEG2V, []byte{0, 0, 1, 0xb8, 0, 8, 0, 0x40, 0, 0, 1, 0x00, 0, 0x08}, 0, nil},
		// An SEI with a user data message first, then recovery_point with
		// recovery_frame_cnt 2 (011), exact_match_flag 1, broken_link_flag 0.
		{"AVC recovery_point gets broken_link_flag", CODEC_AVC,
			[]byte{0, 0, 1, 0x06, 5, 2, 0xaa, 0xbb, 6, 1, 0b0111_0000, 0x80, 0, 0, 1, 0x41, 0x9a}, 1,
			[]byte{0, 0, 1, 0x06, 5, 2, 0xaa, 0xbb, 6, 1, 0b0111_1000, 0x80, 0, 0, 1, 0x41, 0x9a}},
		// recovery_poc_cnt 0 (1), exact_match_flag 0, broken_link_flag 0.
		{"HEVC recovery_point gets broken_link_flag", CODEC_HEVC,
			[]byte{0, 0, 1, 39 << 1, 1, 6, 1, 0b1000_0000, 0x80, 0, 0, 1, 1 << 1, 1, 0xaa}, 1,
			[]byte{0, 0, 1, 39 << 1, 1, 6, 1, 0b1010_0000, 0x80, 0, 0, 1, 1 << 1, 1, 0xaa}},
		// User data that claims 5 bytes in an SEI of 3, then a slice whose bytes
		// read like a recovery_point message.
		{"an SEI is not read on into the next NAL unit", CODEC_AVC,
			[]byte{0, 0, 1, 0x06, 0x05, 0x05, 0xaa, 0, 0, 1, 0x41, 0x06, 0x01, 0x70, 0x80}, 0, nil},
		{"a recovery_point after an emulation prevention byte is left alone", CODEC_AVC,
			[]byte{0, 0, 1, 0x06, 5, 3, 0, 0, 3, 6, 1, 0b0111_0000, 0x80, 0, 0, 1, 0x41, 0x9a}, 0, nil},
	}
	for _, c := range cases {
		data := append([]byte(nil), c.data...)
		if n := markSeam(c.codec, data); n != c.changed {
			t.Errorf("%s: changed %d, want %d", c.name, n, c.changed)
		}
		want := c.want
		if want == nil {
			want = c.data
		}
		if !bytes.Equal(data, want) {
			t.Errorf("%s: % x", c.name, data)
		}
	}
	avc := []byte{0, 0, 1, 0x06, 5, 2, 0xaa, 0xbb, 6, 1, 0b0111_0000, 0x80, 0, 0, 1, 0x41, 0x9a}
	if markSeam(CODEC_AVC, avc); markSeam(CODEC_AVC, avc) != 0 {
		t.Error("a set broken_link_flag is set again")
	}
}

func TestInspectPicture(t *testing.T) {
	inspect := func(codec Codec, data []byte) pictureInfo { return newPictureInspector(codec).inspect(data) }
	if info := inspect(CODEC_AVC, h264AccessUnit(true, 100)); info.accessPoint != apClosedGOP || !info.parameterSets {
		t.Errorf("AVC IDR %+v", info)
	}
	if info := inspect(CODEC_AVC, h264AccessUnit(false, 100)); info.accessPoint != apNone {
		t.Errorf("AVC non-IDR %+v", info)
	}
	if info := inspect(CODEC_HEVC, []byte{0, 0, 1, 33 << 1, 1, 0x42, 0, 0, 1, 21 << 1, 1, 0xaa}); info.accessPoint != apOpenGOP {
		t.Errorf("HEVC CRA %+v", info)
	}
	mpeg2 := []byte{0, 0, 1, 0xb3, 1, 2, 3, 0, 0, 1, 0xb8, 0, 8, 0, 0x40, 0, 0, 1, 0x00, 0, 0x08}
	if info := inspect(CODEC_MPEG2V, mpeg2); info.accessPoint != apClosedGOP {
		t.Errorf("MPEG-2 closed GOP %+v", info)
	}
	if info := inspect(CODEC_AVC, []byte{0, 0, 1, 0x06, 5, 2, 0xaa, 0xbb, 6, 1, 0b0111_0000, 0x80, 0, 0, 1, 0x41, 0x9a}); info.accessPoint != apRecoveryPoint {
		t.Errorf("AVC recovery point %+v", info)
	}
}

// TestRecoveryPointSEI checks that a recovery point SEI is read past emulation
// prevention bytes and within its NAL unit.
func TestRecoveryPointSEI(t *testing.T) {
	au := func(body ...byte) []byte {
		// A non-IDR slice whose data reads like a recovery_point message.
		return concat([]byte{0, 0, 1, 0x06}, body, []byte{0, 0, 1, 0x41, 0x06, 0x01, 0x70, 0x80})
	}
	cases := []struct {
		name string
		data []byte
		want accessPoint
	}{
		// pic_timing (type 1) of 3 bytes, 00 00 01, escaped as 00 00 03 01, then a
		// recovery_point: the escaped size of the first message must not hide it.
		{"after an escaped message", au(0x01, 0x03, 0, 0, 3, 0x01, 0x06, 0x01, 0x70, 0x80), apRecoveryPoint},
		// pic_timing of 4 bytes, 00 00 01 06, then user data: a walk by the escaped
		// size lands on the 06 and takes it for a recovery point.
		{"not made up by an escaped message", au(0x01, 0x04, 0, 0, 3, 0x01, 0x06, 0x05, 0x01, 0xaa, 0x80), apNone},
		// User data that claims more bytes than its NAL unit has is not read on
		// into the slice that follows.
		{"not read into the next NAL unit", au(0x05, 0x05, 0xaa), apNone},
	}
	for _, c := range cases {
		if got := inspectAVC(c.data).accessPoint; got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestPictureNumbering reads the picture numbering of a stream's pictures, most
// of which carry no parameter sets of their own: they are kept from earlier
// access units.
func TestPictureNumbering(t *testing.T) {
	for _, c := range []struct {
		file  string
		codec Codec
	}{{"testdata/bbb_1s.ts", CODEC_AVC}, {"testdata/obs_hevc_aac.ts", CODEC_HEVC}} {
		data, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		idx, err := scanData(t, data)
		if err != nil {
			t.Fatal(err)
		}
		numbered, withoutPS := 0, 0
		for _, p := range idx.pictures {
			if p.info.numbering != nil {
				numbered++
				if !p.info.parameterSets {
					withoutPS++
				}
			}
		}
		if numbered != len(idx.pictures) || withoutPS == 0 {
			t.Errorf("%s: %d of %d pictures numbered, %d without parameter sets", c.file, numbered, len(idx.pictures), withoutPS)
		}
	}
	// A slice NAL unit cut short does not drop the parameter sets for later pictures.
	data, _ := os.ReadFile("testdata/bbb_1s.ts")
	idx, _ := scanData(t, data)
	insp := newPictureInspector(CODEC_AVC)
	var first []byte
	for _, r := range collectPESOn(data, 256) {
		if first == nil {
			first = r
			continue
		}
		insp.inspect(first)
		if insp.inspect([]byte{0, 0, 1, 0x41}).numbering != nil {
			t.Error("a slice of one byte numbered")
		}
		if insp.inspect(r).numbering == nil || idx.pictures[1].info.numbering == nil {
			t.Error("the parameter sets were dropped")
		}
		break
	}
}

// collectPESOn returns the PES payloads of pid.
func collectPESOn(stream []byte, pid int) [][]byte {
	var out [][]byte
	for i := 0; i < len(stream)/PacketSize; i++ {
		p := packetAt(stream, i)
		if tsPID(p) != pid || !tsHasPayload(p) {
			continue
		}
		if h, ok := findPESHeader(p); ok {
			out = append(out, append([]byte(nil), p[h.start+h.size:]...))
		} else if len(out) > 0 {
			out[len(out)-1] = append(out[len(out)-1], p[tsPayloadOffset(p):]...)
		}
	}
	return out
}

func TestDetectFrameRate(t *testing.T) {
	// Pictures shown steps[i] ticks for fields[i] field periods (none: frames), in
	// decode order I P B B: the rate is read in display order.
	rateOf := func(steps []int64, fields ...uint8) FrameRate {
		var shown []videoPicture
		ts := int64(0)
		for i := 0; i < 100; i++ {
			p := videoPicture{pts: ts}
			if len(fields) > 0 {
				p.info.shownFields = fields[i%len(fields)]
			}
			shown = append(shown, p)
			ts += steps[i%len(steps)]
		}
		var pics []videoPicture
		for i := 0; i+3 <= len(shown); i += 3 {
			pics = append(pics, shown[i+2], shown[i], shown[i+1])
		}
		r, _ := detectFrameRate(pics)
		return r
	}
	cases := []struct {
		name   string
		steps  []int64
		fields []uint8
		want   FrameRate
	}{
		{"25", []int64{3600}, nil, FrameRate{25, 1}},
		{"29.97", []int64{3003}, nil, FrameRate{30000, 1001}},
		{"30", []int64{3000}, nil, FrameRate{30, 1}},
		{"59.94", []int64{1501, 1502}, nil, FrameRate{60000, 1001}},
		{"60", []int64{1500}, nil, FrameRate{60, 1}},
		{"23.976", []int64{3754, 3754, 3753, 3754}, nil, FrameRate{24000, 1001}},
		{"24", []int64{3750}, nil, FrameRate{24, 1}},
		{"none", []int64{3100}, nil, FrameRate{}},
		{"fields of 25 fps", []int64{1800}, []uint8{1}, FrameRate{25, 1}},
		{"3:2 pulldown", []int64{4505, 3003, 4504, 3003}, []uint8{3, 2, 3, 2}, FrameRate{30000, 1001}},
		{"3:2 pulldown, not signalled", []int64{4505, 3003, 4504, 3003}, nil, FrameRate{}},
		{"23.976 fps frames repeated in 59.94", []int64{4505, 3003, 4504, 3003}, []uint8{6, 4, 6, 4},
			FrameRate{60000, 1001}},
	}
	for _, c := range cases {
		if got := rateOf(c.steps, c.fields...); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseFrameRate(t *testing.T) {
	for in, want := range map[string]FrameRate{"25": {25, 1}, "29.97": {30000, 1001}, "59.94": {60000, 1001},
		"23.976": {24000, 1001}, "30000/1001": {30000, 1001}} {
		if got, err := ParseFrameRate(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0", "25.5", "1/0", "x"} {
		if _, err := ParseFrameRate(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestAudioLapController(t *testing.T) {
	// 47 frames of 1920 ticks against a 90000-tick loop: 240 ticks too many a lap.
	var starts []int64
	for f := int64(0); f <= 47; f++ {
		starts = append(starts, f*tAudioFrameTicks)
	}
	c := audioLapController{frameStarts: starts, lapDuration: 90_000}
	end, frames := int64(0), int64(0)
	for lap := int64(0); lap < 1000; lap++ {
		r := c.next()
		if r.gap || lap*90_000+r.offset != end || r.offset < 0 || r.offset >= tAudioFrameTicks {
			t.Fatalf("lap %d: %+v, audio ended at %d", lap, r, end)
		}
		end = lap*90_000 + r.offset + starts[r.keptFrames]
		frames += int64(r.keptFrames)
	}
	if d := frames*tAudioFrameTicks - 1000*90_000; d < 0 || d >= tAudioFrameTicks {
		t.Errorf("audio %d ticks beyond 1000 laps", d)
	}

	// 29.97 fps S302M-like units of 1601 and 1602 samples at 48 kHz, in 90 kHz ticks.
	starts = []int64{0}
	for u := 0; u < 40; u++ {
		step := int64(3003)
		if u%5 == 0 {
			step = 3002
		}
		starts = append(starts, starts[len(starts)-1]+step)
	}
	c = audioLapController{frameStarts: starts, lapDuration: 30 * 3003}
	end = 0
	for lap := int64(0); lap < 500; lap++ {
		r := c.next()
		if r.gap || lap*30*3003+r.offset != end || r.offset < 0 || r.offset >= 3003 {
			t.Fatalf("varying PES, lap %d: %+v", lap, r)
		}
		end = lap*30*3003 + r.offset + starts[r.keptFrames]
	}

	// Too little audio leaves a gap instead of drifting.
	c = audioLapController{frameStarts: []int64{0, 1920, 3840}, lapDuration: 6000}
	for lap := 0; lap < 10; lap++ {
		if r := c.next(); !r.gap || r.offset != 0 || r.keptFrames != 2 {
			t.Fatalf("lap %d: %+v", lap, r)
		}
	}
}

// indexOf returns an index of pictures, 20 packets apart at a constant rate
// whose PCR keeps time with their PTS, as the scan of such a stream gives it.
func indexOf(pics []videoPicture) *streamIndex {
	const perPicture = 20
	const pcrPerPacket = tFrameTicks * pcrPerTick / perPicture
	idx := &streamIndex{packetCount: (len(pics) + 1) * perPicture, pcrPID: tVideoPID, videoPID: tVideoPID}
	idx.nullsBefore = make([]uint32, idx.packetCount+1)
	for p := 0; p < idx.packetCount; p += perPicture {
		idx.pcrSamples = append(idx.pcrSamples, pcrSample{packet: p, pcr: int64(p) * pcrPerPacket})
	}
	for i := range pics {
		pics[i].firstPacket = i * perPicture
	}
	idx.pictures = pics
	return idx
}

func picturesEvery(count, gop int, ap accessPoint) []videoPicture {
	var pics []videoPicture
	for i := 0; i < count; i++ {
		tick := int64(i) * tFrameTicks
		p := videoPicture{pts: tick, dts: tick}
		if i%gop == 0 {
			p.info.accessPoint = ap
		}
		pics = append(pics, p)
	}
	return pics
}

// TestWindowSentOffItsTime checks that a loop whose window is not sent in about
// the time it lasts is not chosen: 1 s of video sent in 0.1 s, too little to
// tell the PCR from the PTS over the whole file.
func TestWindowSentOffItsTime(t *testing.T) {
	idx := &streamIndex{packetCount: 1001, pcrPID: tVideoPID, videoPID: tVideoPID,
		pcrSamples: []pcrSample{{0, 0}, {1000, 5_400_000}}}
	idx.nullsBefore = make([]uint32, idx.packetCount+1)
	for i, p := range picturesEvery(26, 25, apClosedGOP) {
		p.firstPacket = 100 + i*20
		idx.pictures = append(idx.pictures, p)
	}
	_, err := chooseLoop(idx, loopOptions{})
	if err == nil || !strings.Contains(err.Error(), "lasts 1.000 s by the video PTS but takes 0.100 s by the PCR") {
		t.Errorf("%v", err)
	}
}

// TestRecoveryPointNumbering checks that a loop between recovery points keeps
// the picture numbering when one can.
func TestRecoveryPointNumbering(t *testing.T) {
	// A recovery point every 10 pictures, no IDR; picture order count LSB 2n mod 64.
	numbered := func(maxFrameNum uint32) []videoPicture {
		pics := picturesEvery(201, 10, apRecoveryPoint)
		for i := range pics {
			pics[i].info.numbering = &pictureNumbering{frameNum: uint32(i) % maxFrameNum, orderLSB: 2 * uint32(i) % 64}
		}
		return pics
	}
	// The longest loop whose length is a multiple of both number ranges.
	plan, err := chooseLoop(indexOf(numbered(16)), loopOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.accessPoint != apRecoveryPoint || plan.frames != 160 || plan.startPicture != 0 ||
		!anyHas(plan.warnings, "until the stream recovers") || anyHas(plan.warnings, "keeps the picture numbering") {
		t.Errorf("%+v", plan)
	}
	// None keeps it: the longest, with the numbers in the warning.
	plan, err = chooseLoop(indexOf(numbered(256)), loopOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.frames != 200 || !anyHas(plan.warnings, "each lap starts with frame_num 0 and picture order count LSB 0 "+
		"where the stream continues with frame_num 200 and picture order count LSB 16") {
		t.Errorf("%d frames, warnings %v", plan.frames, plan.warnings)
	}
	// Only the fields that differ are named.
	pics := picturesEvery(11, 10, apRecoveryPoint)
	for i := range pics {
		pics[i].info.numbering = &pictureNumbering{bottomField: i == 10}
	}
	plan, err = chooseLoop(indexOf(pics), loopOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !anyHas(plan.warnings, "each lap starts with a top field where the stream continues with a bottom field") {
		t.Errorf("warnings %v", plan.warnings)
	}
}

// TestDTSJumpAtSeam checks that a loop whose DTS would jump at the seam is
// avoided or refused. PTS keeps time with the PCR; PTS - DTS grows each picture.
func TestDTSJumpAtSeam(t *testing.T) {
	drifting := func(gop, count int, drift int64) []videoPicture {
		pics := picturesEvery(count, gop, apClosedGOP)
		for i := range pics {
			pics[i].dts -= int64(i) * drift
		}
		return pics
	}
	opts := loopOptions{frameRate: FrameRate{25, 1}}
	// 600 ticks a picture: 150 pictures part PTS from DTS by one second more.
	plan, err := chooseLoop(indexOf(drifting(25, 251, 600)), opts)
	if err != nil {
		t.Fatal(err)
	}
	if plan.frames != 150 || !anyHas(plan.warnings, "the video DTS steps +1000.0 ms off at every seam") {
		t.Errorf("%d frames, warnings %v", plan.frames, plan.warnings)
	}
	_, err = chooseLoop(indexOf(drifting(50, 151, 2000)), opts)
	if err == nil || !strings.Contains(err.Error(), "the video DTS would jump +3.333 s at every seam") {
		t.Errorf("%v", err)
	}
}

func TestPictureShownAfterLoopEnd(t *testing.T) {
	pics := picturesEvery(101, 25, apClosedGOP)
	pics[49].pts = pics[50].pts + tFrameTicks // decoded before the access point at 50, shown after it
	plan, err := chooseLoop(indexOf(pics), loopOptions{maxDuration: 50 * tFrameTicks})
	if err != nil {
		t.Fatal(err)
	}
	if plan.startPicture != 25 || plan.endPicture != 75 {
		t.Errorf("loop %d..%d, want 25..75", plan.startPicture, plan.endPicture)
	}
}

// TestLoopNeverSpansDiscontinuity checks that a loop never spans a signalled
// discontinuity.
func TestLoopNeverSpansDiscontinuity(t *testing.T) {
	idx := indexOf(picturesEvery(126, 25, apClosedGOP))
	idx.discontinuities = []int{30 * 20}
	plan, err := chooseLoop(idx, loopOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.startPicture != 50 || plan.endPicture != 125 {
		t.Errorf("loop %d..%d, want 50..125", plan.startPicture, plan.endPicture)
	}
	idx.discontinuities = []int{10 * 20, 30 * 20, 60 * 20, 80 * 20, 110 * 20}
	if _, err := chooseLoop(idx, loopOptions{}); err == nil || !strings.Contains(err.Error(), "discontinuity") {
		t.Errorf("a discontinuity in every GOP: %v", err)
	}
}

// TestWholeFrames checks that at 59.94 fps (1501.5 ticks per frame) with an odd
// GOP length the loop is an even number of GOPs, so its duration is a whole
// number of frame periods.
func TestWholeFrames(t *testing.T) {
	var pics []videoPicture
	for n := 0; n <= 15*7; n++ {
		ts := (int64(n)*3003 + 1) / 2 // round(n * 1501.5): 1501/1502 dither
		p := videoPicture{pts: ts, dts: ts}
		if n%15 == 0 {
			p.info.accessPoint = apClosedGOP
		}
		pics = append(pics, p)
	}
	idx := indexOf(pics)
	for i := range idx.pcrSamples { // the PCR keeps time with 59.94 fps
		idx.pcrSamples[i].pcr = int64(idx.pcrSamples[i].packet) * 3003 * pcrPerTick / 40
	}
	plan, err := chooseLoop(idx, loopOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.frameRate != (FrameRate{60000, 1001}) || plan.gops != 6 || plan.frames != 90 || !plan.exact {
		t.Errorf("%v: %d GOPs, %d frames, exact %v", plan.frameRate, plan.gops, plan.frames, plan.exact)
	}
}

// fieldPictures are pictures coded as fields, a field period apart, top field
// first, each shown for one field period.
func fieldPictures(count int) []videoPicture {
	var pics []videoPicture
	for i := 0; i < count; i++ {
		tick := int64(i) * tFrameTicks / 2
		p := videoPicture{pts: tick, dts: tick}
		p.info.field, p.info.shownFields = FieldTop, 1
		if i%2 == 1 {
			p.info.field = FieldBottom
		}
		pics = append(pics, p)
	}
	return pics
}

// pcrEvery sets an index's PCR to keep time with its pictures' PTS.
func pcrEvery(idx *streamIndex) {
	for k := range idx.pcrSamples {
		pic := min(k, len(idx.pictures)-1)
		idx.pcrSamples[k].pcr = idx.pictures[pic].pts * pcrPerTick
		if k > pic {
			idx.pcrSamples[k].pcr += int64(k-pic) * (idx.pictures[pic].pts - idx.pictures[pic-1].pts) * pcrPerTick
		}
	}
}

// TestPairFields pairs fields in decode order, and recovers from a lost field.
func TestPairFields(t *testing.T) {
	// Top-first frames 0-4 of an H.264 stream; the top field of frame 2 is lost.
	var pics []videoPicture
	for _, f := range fieldPictures(10) {
		frame := uint32(f.pts / tFrameTicks)
		f.info.numbering = &pictureNumbering{frameNum: frame}
		if f.pts != 2*tFrameTicks {
			pics = append(pics, f)
		}
	}
	idx := &streamIndex{videoCodec: CODEC_AVC, pictures: pics}
	(&scanner{idx: idx}).pairFields()
	var got []bool
	for _, p := range idx.pictures {
		got = append(got, p.info.secondField)
	}
	if want := []bool{false, true, false, true, false, false, true, false, true}; !slices.Equal(got, want) {
		t.Errorf("second fields %v, want %v", got, want)
	}
	// An H.264 IDR field is a first field, a field shown before the open one
	// is not its second, and an MPEG-2 frame's fields share temporal_reference.
	pics = fieldPictures(4)
	pics[1].info.accessPoint = apClosedGOP
	idx = &streamIndex{videoCodec: CODEC_AVC, pictures: pics}
	(&scanner{idx: idx}).pairFields()
	if idx.pictures[1].info.secondField || !idx.pictures[2].info.secondField {
		t.Errorf("IDR on a bottom field: %+v", idx.pictures)
	}
	pics = fieldPictures(4)
	pics[1].pts = -tFrameTicks
	pics[3].info.temporalRef = 1
	idx = &streamIndex{videoCodec: CODEC_MPEG2V, pictures: pics}
	(&scanner{idx: idx}).pairFields()
	for i, p := range idx.pictures {
		if p.info.secondField {
			t.Errorf("picture %d paired", i)
		}
	}
}

// TestLoopPointsSkipSecondFields checks that a loop of field pictures starts
// and ends on first fields, counts whole frames and finds the frame rate.
func TestLoopPointsSkipSecondFields(t *testing.T) {
	// H.265, whose IRAP pictures may be second fields: access points on the
	// bottom fields 1 and 175, the second of their frames, and on the first
	// fields 50, 100 and 150.
	pics := fieldPictures(181)
	for _, i := range []int{1, 50, 100, 150, 175} {
		pics[i].info.accessPoint = apClosedGOP
	}
	idx := indexOf(pics)
	idx.videoCodec = CODEC_HEVC
	pcrEvery(idx)
	(&scanner{idx: idx}).pairFields()
	plan, err := chooseLoop(idx, loopOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.startPicture != 50 || plan.endPicture != 150 || plan.frameRate != (FrameRate{25, 1}) ||
		plan.fieldPeriods != 100 || !plan.exact {
		t.Errorf("loop %d..%d at %v, %d field periods, exact %v", plan.startPicture, plan.endPicture,
			plan.frameRate, plan.fieldPeriods, plan.exact)
	}
	// A rate given as the field rate counts pictures.
	plan, err = chooseLoop(idx, loopOptions{frameRate: FrameRate{50, 1}})
	if err != nil || !plan.exact {
		t.Errorf("at 50 fps: %v %+v", err, plan)
	}
}

// TestPulldownCadence checks that a 3:2 pulldown loop holds whole cadence
// cycles, so its fields keep alternating across the seam, and warns when none
// can.
func TestPulldownCadence(t *testing.T) {
	// 23.976 fps frames shown as 59.94 Hz fields: top-bottom-top, bottom-top,
	// bottom-top-bottom, top-bottom.
	cycle := []cadence{{FieldTop, 3}, {FieldBottom, 2}, {FieldBottom, 3}, {FieldTop, 2}}
	pulldown := func(points ...int) *streamIndex {
		var pics []videoPicture
		fields := int64(0)
		for n := 0; n <= 42; n++ {
			c := cycle[n%len(cycle)]
			ts := (fields*3003 + 1) / 2
			p := videoPicture{pts: ts, dts: ts}
			p.info.firstShown, p.info.shownFields = c.first, c.fields
			pics = append(pics, p)
			fields += int64(c.fields)
		}
		for _, n := range points {
			pics[n].info.accessPoint = apClosedGOP
		}
		idx := indexOf(pics)
		pcrEvery(idx)
		return idx
	}
	// An access point every 7 frames. 0..42 is not whole frame periods, and
	// 0..35 is, but ends on the cycle's last frame, top-bottom, where it starts
	// on top-bottom-top: four cycles, 0..28, are the longest loop.
	everySeventh := []int{0, 7, 14, 21, 28, 35, 42}
	plan, err := chooseLoop(pulldown(everySeventh...), loopOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.startPicture != 0 || plan.endPicture != 28 || plan.frameRate != (FrameRate{30000, 1001}) ||
		plan.fieldPeriods != 70 || !plan.exact || anyHas(plan.warnings, "cadence") {
		t.Errorf("loop %d..%d at %v, %d field periods, exact %v, warnings %v", plan.startPicture, plan.endPicture,
			plan.frameRate, plan.fieldPeriods, plan.exact, plan.warnings)
	}
	// Given as the film rate, the loop is whole frames too.
	plan, err = chooseLoop(pulldown(everySeventh...), loopOptions{frameRate: FrameRate{24000, 1001}})
	if err != nil || plan.endPicture != 28 || !plan.exact {
		t.Errorf("at 23.976 fps: %v %+v", err, plan)
	}
	plan, err = chooseLoop(pulldown(0, 42), loopOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !anyHas(plan.warnings, "the loop starts on a picture shown top-bottom-top where the stream continues with "+
		"one shown bottom-top-bottom: the field order or pulldown cadence breaks at every seam") {
		t.Errorf("warnings %v", plan.warnings)
	}
}

// TestMarkSeamFieldPair checks that both fields of an H.265 frame coded as two
// CRA fields become BLA at the loop start, and nothing after them.
func TestMarkSeamFieldPair(t *testing.T) {
	for _, secondIsCRA := range []bool{true, false} {
		var packets []byte
		for k := 0; k < 3; k++ {
			au := h265AccessUnit(k != 1 || secondIsCRA, 100)
			for _, p := range pesPackets(tVideoPID, 0xe0, int64(k)*1800, -1, au) {
				packets = append(packets, p...)
			}
		}
		pics := fieldPictures(3)
		for k := range pics {
			if k != 1 || secondIsCRA {
				pics[k].info.accessPoint = apOpenGOP
			}
		}
		idx := &streamIndex{videoPID: tVideoPID, videoCodec: CODEC_HEVC, pictures: pics}
		(&scanner{idx: idx}).pairFields()
		seg := &loopSegment{packets: packets}
		want := 1
		if secondIsCRA {
			want = 2
		}
		if n := markSeamPictures(seg, idx, 0); n != want {
			t.Errorf("second field CRA %v: %d marked, want %d", secondIsCRA, n, want)
		}
		if last := collectPESOn(packets, tVideoPID)[2]; !bytes.Equal(last, h265AccessUnit(true, 100)) {
			t.Error("the next frame was marked")
		}
	}
}

// TestFieldStructure reads how pictures are shown: MPEG-2 picture_structure,
// top_field_first, repeat_first_field and progressive_sequence; H.264
// field_pic_flag and pic_struct after the HRD delays; H.265 pic_struct.
func TestFieldStructure(t *testing.T) {
	seqExt := func(progressive bool) []byte {
		if progressive {
			return []byte{0, 0, 1, 0xb5, 0x14, 0x8a, 0, 1, 0, 0}
		}
		return []byte{0, 0, 1, 0xb5, 0x14, 0x82, 0, 1, 0, 0}
	}
	picExt := func(structure byte, topFirst, repeat bool) []byte {
		b3 := byte(0)
		if topFirst {
			b3 |= 0x80
		}
		if repeat {
			b3 |= 0x02
		}
		return []byte{0, 0, 1, 0xb5, 0x8f, 0xff, 0xf0 | structure, b3, 0x80}
	}
	type shown struct {
		field, first FieldParity
		fields       uint8
	}
	of := func(i pictureInfo) shown { return shown{i.field, i.firstShown, i.shownFields} }
	mpeg2 := newPictureInspector(CODEC_MPEG2V)
	for _, c := range []struct {
		name string
		au   []byte
		want shown
	}{
		{"interlaced, top first, repeated", concat(m2vSeqHdr(), seqExt(false), m2vPic(mpeg2PicI), picExt(3, true, true),
			m2vSlice()), shown{FieldFrame, FieldTop, 3}},
		{"bottom first", concat(m2vPic(mpeg2PicB), picExt(3, false, false), m2vSlice()), shown{FieldFrame, FieldBottom, 2}},
		{"field", concat(m2vPic(mpeg2PicB), picExt(2, false, false), m2vSlice()), shown{FieldBottom, FieldUnknown, 1}},
		{"field pair", concat(m2vPic(mpeg2PicB), picExt(2, false, false), m2vSlice(), m2vPic(mpeg2PicB),
			picExt(1, false, false), m2vSlice()), shown{FieldFrame, FieldBottom, 2}},
		{"progressive, three frames", concat(m2vSeqHdr(), seqExt(true), m2vPic(mpeg2PicI), picExt(3, true, true),
			m2vSlice()), shown{FieldFrame, FieldUnknown, 6}},
		{"progressive, two frames", concat(m2vPic(mpeg2PicB), picExt(3, false, true), m2vSlice()),
			shown{FieldFrame, FieldUnknown, 4}},
		{"MPEG-1", concat(m2vPic(mpeg2PicB), m2vSlice()), shown{FieldFrame, FieldUnknown, 2}},
	} {
		if got := of(mpeg2.inspect(c.au)); got != c.want {
			t.Errorf("MPEG-2 %s: %+v, want %+v", c.name, got, c.want)
		}
	}

	// H.264 with HRD delays of 5 and 3 bits before pic_struct.
	h264 := newPictureInspector(CODEC_AVC)
	h264.avcSPS[0] = &avc.SPS{VUI: &avc.VUIParameters{PicStructPresentFlag: true, NalHrdParametersPresentFlag: true,
		NalHrdParameters: &avc.HrdParameters{CpbRemovalDelayLengthMinus1: 4, DpbOutputDelayLengthMinus1: 2}}}
	h264.avcPPS[0] = &avc.PPS{}
	timing := func(picStruct uint) []byte {
		return nalUnitOf(0x06, func(w *bits.EBSPWriter) {
			w.Write(1, 8) // pic_timing
			w.Write(2, 8)
			w.Write(0, 5+3) // cpb_removal_delay, dpb_output_delay
			w.Write(picStruct, 4)
			w.Write(0, 3) // clock_timestamp_flag for three clocks
			w.Write(1, 1) // payload alignment
		})
	}
	slice := func(field, bottom bool) []byte {
		return nalUnitOf(0x41, func(w *bits.EBSPWriter) {
			w.WriteExpGolomb(0) // first_mb_in_slice
			w.WriteExpGolomb(5) // slice_type
			w.WriteExpGolomb(0) // pic_parameter_set_id
			w.Write(0, 4)       // frame_num
			w.Write(map[bool]uint{false: 0, true: 1}[field], 1)
			if field {
				w.Write(map[bool]uint{false: 0, true: 1}[bottom], 1)
			}
			w.Write(0xff, 8)
		})
	}
	aud := []byte{0, 0, 0, 1, 0x09, 0xf0}
	for _, c := range []struct {
		name string
		au   []byte
		want shown
	}{
		{"top-bottom-top frame", concat(timing(5), slice(false, false)), shown{FieldFrame, FieldTop, 3}},
		{"frame doubling", concat(timing(7), slice(false, false)), shown{FieldFrame, FieldUnknown, 4}},
		{"bottom field", concat(timing(2), slice(true, true)), shown{FieldBottom, FieldUnknown, 1}},
		{"both fields in one PES", concat(slice(true, true), aud, slice(true, false)), shown{FieldFrame, FieldBottom, 2}},
	} {
		if got := of(h264.inspect(c.au)); got != c.want {
			t.Errorf("H.264 %s: %+v, want %+v", c.name, got, c.want)
		}
	}

	// H.265 field coding: pic_struct 12 is a bottom field paired with the next.
	h265 := newPictureInspector(CODEC_HEVC)
	h265.hevcSPS[0] = &hevc.SPS{VUI: &hevc.VUIParameters{FieldSeqFlag: true, FrameFieldInfoPresentFlag: true}}
	h265.hevcPPS[0] = &hevc.PPS{}
	sei := []byte{0, 0, 1, 39 << 1, 1, 0x01, 0x01, 12 << 4, 0x80}
	for _, c := range []struct {
		name string
		au   []byte
		want shown
	}{
		{"trailing field", concat(sei, []byte{0, 0, 1, 1 << 1, 1, 0b1100_0000}), shown{FieldBottom, FieldUnknown, 1}},
		{"CRA field", concat(sei, []byte{0, 0, 1, 21 << 1, 1, 0b1010_0000}), shown{FieldBottom, FieldUnknown, 1}},
		{"without pic_timing", []byte{0, 0, 1, 1 << 1, 1, 0b1100_0000}, shown{FieldFrame, FieldUnknown, 1}},
	} {
		if got := of(h265.inspect(c.au)); got != c.want {
			t.Errorf("H.265 %s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// nullLap is a lap of null packets, one a millisecond.
func nullLap(packets int, constantRate bool) *loopSegment {
	s := &loopSegment{duration: int64(packets) * 90, pcrPID: tVideoPID, constantRate: constantRate, lapPackets: packets}
	s.packets = make([]byte, packets*PacketSize)
	for i := 0; i < packets; i++ {
		writeNullPacket(packetAt(s.packets, i))
		s.sendTimes = append(s.sendTimes, int64(i)*27_000)
	}
	return s
}

func putVideoPacket(s *loopSegment, i int, withPCR bool, cc int) {
	p := makePacket(tVideoPID, withPCR)
	p[3] = p[3]&0xf0 | byte(cc&0x0f)
	copy(packetAt(s.packets, i), p)
}

// lapPCRTimes returns the times of a lap's PCRs, then the next lap's first.
func lapPCRTimes(s *loopSegment) []int64 {
	var times []int64
	for i := 0; i < s.packetCount(); i++ {
		if p := packetAt(s.packets, i); tsPID(p) == s.pcrPID && tsCarriesPCR(p) {
			times = append(times, s.sendTimes[i])
		}
	}
	return append(times, s.duration*pcrPerTick+times[0])
}

// TestFillSeamPCRGap: a 1 s lap of null packets, then the PCR every 20 ms from
// 100 to 500 ms with video between, then null packets. Those 5 to 36 ms after the
// last PCR were audio, across where the first added PCR would otherwise go.
func TestFillSeamPCRGap(t *testing.T) {
	s := nullLap(1000, true)
	for i := 100; i <= 500; i++ {
		putVideoPacket(s, i, i%20 == 0, i+3)
	}
	audioSlots := make([]bool, s.packetCount())
	for i := 505; i < 537; i++ {
		audioSlots[i] = true
	}
	added := fillSeamPCRGap(s, audioSlots)
	pcrs := lapPCRTimes(s)
	if added == 0 || added != len(pcrs)-22 { // 21 PCRs in the source, and the next lap's first
		t.Errorf("added %d PCRs, %d in all", added, len(pcrs))
	}
	for i := 1; i < len(pcrs); i++ {
		// Short of 40 ms, also before the next lap's first, so that audio the
		// writer inserts there keeps the PCR within 40 ms.
		if pcrs[i]-pcrs[i-1] > pcrFillSpacing {
			t.Errorf("PCR %d at %d ms", i, pcrs[i]/27_000)
		}
	}
	if s.packetCount() != 1000 || len(s.warnings) != 0 {
		t.Errorf("%d packets, warnings %v", s.packetCount(), s.warnings)
	}
	for i := 505; i < 537; i++ {
		if tsPID(packetAt(s.packets, i)) != nullPID {
			t.Errorf("audio slot %d taken", i)
		}
	}
	for i := 0; i < s.packetCount(); i++ {
		p := packetAt(s.packets, i)
		if tsPID(p) != tVideoPID || (i >= 100 && i <= 500) {
			continue
		}
		if !tsCarriesPCR(p) || tsHasPayload(p) {
			t.Errorf("added PCR packet %d", i)
		}
		// Before the PID's first packet in the first lap: the counter before its.
		if i < 100 && tsCC(p) != (100+3-1)&0x0f {
			t.Errorf("added PCR packet %d: CC %d", i, tsCC(p))
		}
	}
}

// TestFillSeamPCRGapVariableRate: 500 ms of packets in a 600 ms lap, the PCR
// every 20 ms up to 480 ms, then video without one.
func TestFillSeamPCRGapVariableRate(t *testing.T) {
	s := nullLap(500, false)
	s.duration = 600 * 90
	for i := 0; i < 500; i++ {
		putVideoPacket(s, i, i%20 == 0 && i <= 480, i)
	}
	if added := fillSeamPCRGap(s, make([]bool, s.packetCount())); added != 3 || s.packetCount() != 503 {
		t.Errorf("added %d, %d packets", added, s.packetCount())
	}
	for i := 1; i < len(s.sendTimes); i++ {
		if s.sendTimes[i] < s.sendTimes[i-1] || s.sendTimes[i] >= s.duration*pcrPerTick {
			t.Fatalf("send time %d: %d", i, s.sendTimes[i])
		}
	}
	pcrs := lapPCRTimes(s)
	for i := 1; i < len(pcrs); i++ {
		if pcrs[i]-pcrs[i-1] > maxPCRSpacing {
			t.Errorf("PCR %d at %d ms", i, pcrs[i]/27_000)
		}
	}
}

// TestSCTE35LegacyCommandLength finds the event IDs of a splice_insert with the
// legacy splice_command_length 0xfff, a splice time and a break duration, then
// a segmentation descriptor.
func TestSCTE35LegacyCommandLength(t *testing.T) {
	sec := []byte{
		0xfc, 0x30, 54, // table_id, sap_type 3, section_length
		0x00,                         // protocol_version
		0x00, 0x00, 0x00, 0x00, 0x00, // pts_adjustment
		0xff,             // cw_index
		0xff, 0xff, 0xff, // tier, splice_command_length 0xfff
		0x05,                  // splice_insert
		0x00, 0x00, 0x00, 100, // splice_event_id
		0x7f,                         // not cancelled
		0xef,                         // out of network, program splice, duration, not immediate
		0xfe, 0x00, 0x00, 0x00, 0x00, // splice_time with PTS
		0xfe, 0x00, 0x01, 0x5f, 0x90, // break_duration
		0x00, 0x01, 0x00, 0x00, // unique_program_id, avail_num, avails_expected
		0x00, 17, // descriptor_loop_length
		0x02, 15, 'C', 'U', 'E', 'I', // segmentation_descriptor
		0x00, 0x00, 0x00, 200, // segmentation_event_id
		0x7f, 0xbf, 0x00, 0x00, 0x34, 0x00, 0x00,
	}
	sec = append(sec, 0, 0, 0, 0)
	var ids []uint32
	for _, o := range scte35EventIDOffsets(sec) {
		ids = append(ids, binary.BigEndian.Uint32(sec[o:]))
	}
	if len(ids) != 2 || ids[0] != 100 || ids[1] != 200 {
		t.Errorf("event IDs %v, want [100 200]", ids)
	}
}

// TestUDPOutput sends packets due 1 ms apart: 7 to a datagram, each datagram
// when its last packet is due.
func TestUDPOutput(t *testing.T) {
	rx, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rx.Close() }()
	u, err := NewUDPOutput(rx.LocalAddr().String(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.Close() }()
	const n = 30
	packets := make([]byte, n*PacketSize)
	for i := 0; i < n; i++ {
		copy(packetAt(packets, i), makePacket(0x100+i, false))
	}
	start := time.Now()
	if err := u.Write(context.Background(), packets, func(i int) int64 { return int64(i) * 27_000 }); err != nil {
		t.Fatal(err)
	}
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 27*time.Millisecond {
		t.Errorf("sent in %v, the last packet is due at 29 ms", elapsed)
	}
	var got []byte
	buf := make([]byte, 2000)
	for len(got) < len(packets) {
		_ = rx.SetReadDeadline(time.Now().Add(time.Second))
		k, _, err := rx.ReadFrom(buf)
		if err != nil {
			t.Fatal(err)
		}
		if k != tsPacketsPerDatagram*PacketSize && len(got)+k != len(packets) {
			t.Errorf("datagram of %d bytes", k)
		}
		got = append(got, buf[:k]...)
	}
	if !bytes.Equal(got, packets) {
		t.Error("the datagrams differ from the packets")
	}
	if pcrToNs(27_000_000*86_400*365) != int64(86_400*365)*1e9 {
		t.Error("pcrToNs over a year")
	}
}

// TestPMTDescriptors checks that a PMT whose descriptor runs past its loop is
// kept, and that streams are told apart by their stream type and descriptors.
func TestPMTDescriptors(t *testing.T) {
	body := []byte{0xe1, 0x00, 0xf0, 0x00, // PCR_PID 0x100, no program descriptors
		stAVC, 0xe1, 0x00, 0xf0, 0x00,
		stPrivateData, 0xe1, 0x01, 0xf0, 0x06, descAC3, 0x01, 0x00, 0x0a, 0x09, 'e', // runs past its loop
		stPrivateData, 0xe1, 0x02, 0xf0, 0x06, descRegistation, 0x04, 'B', 'S', 'S', 'D',
		stPrivateData, 0xe1, 0x03, 0xf0, 0x03, descExtension, 0x01, extAC4,
		stPrivateData, 0xe1, 0x04, 0xf0, 0x03, descTeletext, 0x01, 0x00,
		stLATM, 0xe1, 0x05, 0xf0, 0x00,
	}
	streams, pcrPID, ok := parsePMT(psiSection(0x02, 1, body))
	if !ok || pcrPID != 0x100 || len(streams) != 6 {
		t.Fatalf("PMT: ok %v, PCR PID %d, %d streams", ok, pcrPID, len(streams))
	}
	if d := streams[1].descriptors; len(d) != 1 || d[0].tag != descAC3 {
		t.Errorf("descriptors %+v, want the AC-3 descriptor only", d)
	}
	want := []struct {
		codec audioCodec
		audio bool
	}{{audioUnknown, false}, {audioAC3, true}, {audioUnknown, true}, {audioUnknown, true}, {audioUnknown, false},
		{audioUnknown, true}}
	for i, es := range streams {
		if c, ok := audioCodecOf(es); c != want[i].codec || ok != want[i].audio {
			t.Errorf("PID %#x: %v %v, want %v %v", es.pid, c, ok, want[i].codec, want[i].audio)
		}
	}
	if l := passThroughLabel(streams[4]); l != "teletext" {
		t.Errorf("label %q", l)
	}
	// A PMT whose stream loop runs past the section is refused.
	if _, _, ok := parsePMT(psiSection(0x02, 1, append(body[:len(body)-1:len(body)-1], 0x10))); ok {
		t.Error("an ES_info_length past the section accepted")
	}
}
