package internal

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Minimal MPEG-2 video syntax elements for building test access units.
func m2vSeqHdr() []byte { return []byte{0, 0, 1, 0xb3, 0x0b, 0x00, 0x90, 0x13, 0xff, 0xff, 0xe0, 0x18} }

func m2vGOP(closed bool) []byte {
	b3 := byte(0x00)
	if closed {
		b3 = mpeg2ClosedGOPBit
	}
	return []byte{0, 0, 1, 0xb8, 0x00, 0x08, 0x00, b3}
}

func m2vPic(picType byte) []byte { return []byte{0, 0, 1, 0x00, 0x00, picType << 3, 0xff, 0xf8} }

// m2vPicExt is a picture coding extension with the given picture_structure
// (1 = top field, 2 = bottom field, 3 = frame).
func m2vPicExt(structure byte) []byte {
	return []byte{0, 0, 1, 0xb5, 0x8f, 0xff, 0xf0 | structure, 0x80, 0x80}
}

func m2vSlice() []byte { return []byte{0, 0, 1, 0x01, 0x12, 0x34, 0x56} }

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestScanMPEG2AU(t *testing.T) {
	cases := []struct {
		name          string
		data          []byte
		rap, idr, cra bool
		seqHdr        bool
		picType       byte
		field         FieldParity
	}{
		{"closed GOP I", concat(m2vSeqHdr(), m2vGOP(true), m2vPic(mpeg2PicI), m2vPicExt(3), m2vSlice()),
			true, true, false, true, mpeg2PicI, FieldFrame},
		{"open GOP I", concat(m2vSeqHdr(), m2vGOP(false), m2vPic(mpeg2PicI), m2vPicExt(3), m2vSlice()),
			true, false, true, true, mpeg2PicI, FieldFrame},
		{"I without sequence header", concat(m2vGOP(true), m2vPic(mpeg2PicI), m2vPicExt(3), m2vSlice()),
			false, false, false, false, mpeg2PicI, FieldFrame},
		{"B picture", concat(m2vPic(mpeg2PicB), m2vPicExt(3), m2vSlice()),
			false, false, false, false, mpeg2PicB, FieldFrame},
		{"MPEG-1 style I (no extensions)", concat(m2vSeqHdr(), m2vGOP(true), m2vPic(mpeg2PicI), m2vSlice()),
			true, true, false, true, mpeg2PicI, FieldFrame},
		{"lone top field", concat(m2vSeqHdr(), m2vGOP(false), m2vPic(mpeg2PicI), m2vPicExt(1), m2vSlice()),
			true, false, true, true, mpeg2PicI, FieldTop},
		{"field pair in one PES", concat(m2vSeqHdr(), m2vGOP(false), m2vPic(mpeg2PicI), m2vPicExt(1), m2vSlice(),
			m2vPic(2), m2vPicExt(2), m2vSlice()),
			true, false, true, true, mpeg2PicI, FieldFrame},
	}
	for _, c := range cases {
		pic := scanMPEG2AU(c.data)
		if (pic.closedGOP || pic.openGOP) != c.rap || pic.closedGOP != c.idr || pic.openGOP != c.cra ||
			pic.seqHeader != c.seqHdr || pic.picType != c.picType || pic.field != c.field || !pic.hasPicture {
			t.Errorf("%s: scanMPEG2AU=%+v", c.name, pic)
		}
	}
}

// TestResolveMPEG2Leading checks that an open-GOP I picture stays open only
// when a B-picture follows it in coding order, skipping the second field of a
// field-coded I picture.
func TestResolveMPEG2Leading(t *testing.T) {
	openI := concat(m2vSeqHdr(), m2vGOP(false), m2vPic(mpeg2PicI), m2vPicExt(3), m2vSlice())
	openITop := concat(m2vSeqHdr(), m2vGOP(false), m2vPic(mpeg2PicI), m2vPicExt(1), m2vSlice())
	pField := concat(m2vPic(2), m2vPicExt(2), m2vSlice())
	p := concat(m2vPic(2), m2vPicExt(3), m2vSlice())
	b := concat(m2vPic(mpeg2PicB), m2vPicExt(3), m2vSlice())

	cases := []struct {
		name    string
		aus     [][]byte
		wantIDR bool // for the first picture
	}{
		{"open I then B", [][]byte{openI, b, b, p}, false},
		{"open I then P", [][]byte{openI, p, p}, true},
		{"open I field, second field, then B", [][]byte{openITop, pField, b}, false},
		{"open I field, second field, then P", [][]byte{openITop, pField, p}, true},
	}
	for _, c := range cases {
		idx := &streamIndex{videoCodec: CODEC_MPEG2V}
		insp := newPictureInspector(CODEC_MPEG2V)
		for _, d := range c.aus {
			idx.pictures = append(idx.pictures, videoPicture{info: insp.inspect(d)})
		}
		(&scanner{idx: idx}).promoteMPEG2ClosedGOPs()
		want := apOpenGOP
		if c.wantIDR {
			want = apClosedGOP
		}
		if got := idx.pictures[0].info.accessPoint; got != want {
			t.Errorf("%s: first picture is %v, want %v", c.name, got, want)
		}
	}
}

// TestMarkSeamBrokenLink sets broken_link in an open GOP header that straddles a
// TS packet boundary, and leaves a closed GOP untouched.
func TestMarkSeamBrokenLink(t *testing.T) {
	const vpid = 256
	mkPkt := func(pusi bool, payload []byte) []byte {
		b := make([]byte, PacketSize)
		b[0] = 0x47
		b[1] = byte(vpid >> 8)
		if pusi {
			b[1] |= 0x40
		}
		b[2] = byte(vpid & 0xff)
		b[3] = 0x10 // payload only
		for i := 4; i < PacketSize; i++ {
			b[i] = 0xff
		}
		copy(b[4:], payload)
		return b
	}
	const payloadSize = PacketSize - 4
	for _, closed := range []bool{false, true} {
		pesHdr := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x00, 0x00}
		head := concat(pesHdr, m2vSeqHdr())
		gop := m2vGOP(closed)
		// Put the GOP start code at the end of the first packet so its flag byte
		// lands in the second packet.
		pad := bytes.Repeat([]byte{0xff}, payloadSize-len(head)-4)
		first := concat(head, pad, gop[:4])
		second := concat(gop[4:], m2vPic(mpeg2PicI), m2vPicExt(3), m2vSlice())
		seg := concat(mkPkt(true, first), mkPkt(false, second), mkPkt(true, concat(pesHdr, m2vPic(mpeg2PicB))))
		orig := append([]byte(nil), seg...)
		flagOff := PacketSize + 4 + 3 // byte 3 of the GOP header, in the second packet

		ls := &loopSegment{packets: seg}
		marked := markSeamPictures(ls, &streamIndex{videoPID: vpid, videoCodec: CODEC_MPEG2V}, 0)
		if closed {
			if marked != 0 || !bytes.Equal(seg, orig) {
				t.Errorf("closed GOP: marked=%v, segment changed=%v", marked, !bytes.Equal(seg, orig))
			}
			continue
		}
		if marked != 1 || seg[flagOff] != mpeg2BrokenLinkBit {
			t.Fatalf("open GOP: marked=%v flag byte=0x%02x", marked, seg[flagOff])
		}
		seg[flagOff] = orig[flagOff]
		if !bytes.Equal(seg, orig) {
			t.Errorf("open GOP: bytes other than broken_link changed")
		}
	}
}

// TestPrepareLoopMPEG2OpenGOP loops the open-GOP fixture and checks that the
// loop-point GOP header carries broken_link in every lap.
func TestPrepareLoopMPEG2OpenGOP(t *testing.T) {
	const laps = 3
	l, out := loopFile(t, "testdata/mpeg2_open_mp2.ts", LoopOptions{}, laps)
	if lp := l.Report.Loop; lp.AccessPoint != "open-gop" || lp.GOPs != 3 || l.Report.Lap.SeamMarks != 1 {
		t.Fatalf("loop %+v, lap %+v", lp, l.Report.Lap)
	}
	n := 0
	for _, r := range collectPES(out) {
		if r.pid != 256 {
			continue
		}
		if k := bytes.Index(r.payload, []byte{0, 0, 1, mpeg2GroupStart}); k >= 0 && k+7 < len(r.payload) &&
			r.payload[k+7]&mpeg2BrokenLinkBit != 0 {
			n++
		}
	}
	if n != laps {
		t.Errorf("%d GOP headers with broken_link in %d laps", n, laps)
	}
}

// TestRefuseLoopWithoutPTS clears the PTS of one mid-stream video or audio PES in
// a copy of the MPEG-2 fixture: PrepareLoop must refuse it.
func TestRefuseLoopWithoutPTS(t *testing.T) {
	src, err := os.ReadFile("testdata/mpeg2_open_mp2.ts")
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{256, 257} {
		data := append([]byte(nil), src...)
		// Clear PTS_DTS_flags of the third PES start on pid; header_data_length is
		// kept, so the old timestamp bytes become header stuffing.
		starts := 0
		for i := 0; i+PacketSize <= len(data); i += PacketSize {
			b := data[i : i+PacketSize]
			if tsPID(b) != pid || !tsPUSI(b) {
				continue
			}
			if starts++; starts == 3 {
				b[tsPayloadOffset(b)+7] &^= 0xc0
				break
			}
		}
		path := filepath.Join(t.TempDir(), "nopts.ts")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := PrepareLoop(context.TODO(), path, LoopOptions{}); err == nil || !strings.Contains(err.Error(), "carry no PTS") {
			t.Errorf("pid %d: PrepareLoop err=%v, want refusal", pid, err)
		}
	}
}
