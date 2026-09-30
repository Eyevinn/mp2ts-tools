package internal

import (
	"bytes"
	"context"
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
		au := ScanAU(CODEC_MPEG2V, c.data)
		if au.IsRAP != c.rap || au.IsIDR != c.idr || au.IsCRA != c.cra || au.HasSPS != c.seqHdr ||
			au.PicType != c.picType || au.Field != c.field || !au.IsVCL {
			t.Errorf("%s: ScanAU=%+v", c.name, au)
		}
		if got := au.PSComplete(CODEC_MPEG2V); got != c.seqHdr {
			t.Errorf("%s: PSComplete=%v want %v", c.name, got, c.seqHdr)
		}
		if got := ScanRAP(CODEC_MPEG2V, c.data); got != au.RAPInfo {
			t.Errorf("%s: ScanRAP=%+v differs from ScanAU", c.name, got)
		}
	}
}

// TestResolveMPEG2Leading checks that an open-GOP I picture stays open (CRA
// class) only when a B-picture follows it in coding order, skipping the second
// field of a field-coded I picture.
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
		st := &scanTrack{codec: CODEC_MPEG2V, mediaType: "video"}
		for i, d := range c.aus {
			if err := st.HandlePES(&PESData{PTS: int64(i) * 3600, Data: d}, false); err != nil {
				t.Fatal(err)
			}
		}
		first := st.pics[0]
		if first.IsIDR != c.wantIDR || first.IsCRA == c.wantIDR || !first.IsRAP {
			t.Errorf("%s: first picture %+v, want IsIDR=%v", c.name, first, c.wantIDR)
		}
	}
}

func TestLoopPointClass(t *testing.T) {
	idr := PicRecord{IsIDR: true, IsRAP: true}
	cra := PicRecord{IsCRA: true, IsRAP: true}
	other := PicRecord{}
	cases := []struct {
		name  string
		codec Codec
		pics  []PicRecord
		want  string
		count int
	}{
		{"IDR loop", CODEC_HEVC, []PicRecord{idr, other, idr, other, cra}, loopPointIDR, 2},
		{"single IDR falls through to CRA", CODEC_HEVC, []PicRecord{idr, other, cra, other, cra, cra}, loopPointCRA, 3},
		{"MPEG-2 closed", CODEC_MPEG2V, []PicRecord{idr, other, idr, idr}, loopPointClosedI, 3},
		{"MPEG-2 first GOP closed, rest open", CODEC_MPEG2V, []PicRecord{idr, other, cra, other, cra}, loopPointOpenI, 2},
		{"single IDR only", CODEC_AVC, []PicRecord{idr, other, other}, loopPointIDR, 1},
		{"no loop points", CODEC_AVC, []PicRecord{other, other}, loopPointNone, 0},
	}
	for _, c := range cases {
		label, eligible := loopPointClass(c.codec, c.pics)
		n := 0
		for _, p := range c.pics {
			if eligible(p) {
				n++
			}
		}
		if label != c.want || n != c.count {
			t.Errorf("%s: got %s with %d points, want %s with %d", c.name, label, n, c.want, c.count)
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

		marked := markSeamBrokenLink(seg, len(seg)/PacketSize, vpid)
		if closed {
			if marked || !bytes.Equal(seg, orig) {
				t.Errorf("closed GOP: marked=%v, segment changed=%v", marked, !bytes.Equal(seg, orig))
			}
			continue
		}
		if !marked || seg[flagOff] != mpeg2BrokenLinkBit {
			t.Fatalf("open GOP: marked=%v flag byte=0x%02x", marked, seg[flagOff])
		}
		seg[flagOff] = orig[flagOff]
		if !bytes.Equal(seg, orig) {
			t.Errorf("open GOP: bytes other than broken_link changed")
		}
	}
}

// TestPrepareLoopMPEG2OpenGOP builds the loop segment for the open-GOP fixture
// and checks that the loop-point GOP header carries broken_link.
func TestPrepareLoopMPEG2OpenGOP(t *testing.T) {
	seg, plan, err := PrepareLoop(context.TODO(), "testdata/mpeg2_open_mp2.ts", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LoopPointType != loopPointOpenI || plan.NumGOPs != 3 {
		t.Fatalf("plan: type %s, %d GOPs", plan.LoopPointType, plan.NumGOPs)
	}
	buf, _ := firstAUBytes(seg.Data, seg.NumPackets, seg.VideoPID)
	k := bytes.Index(buf, []byte{0, 0, 1, mpeg2GroupStart})
	if k < 0 || k+7 >= len(buf) {
		t.Fatal("no GOP header in the loop-point access unit")
	}
	if b3 := buf[k+7]; b3&mpeg2ClosedGOPBit != 0 || b3&mpeg2BrokenLinkBit == 0 {
		t.Errorf("GOP flags byte 0x%02x: want open GOP with broken_link", b3)
	}
}
