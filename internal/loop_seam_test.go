package internal

import (
	"testing"

	"github.com/Eyevinn/mp4ff/hevc"
)

// TestRewriteSeamCRAtoBLA verifies that the loop-point CRA slice NAL is rewritten
// to BLA_W_LP while the PES start code and other bytes are left untouched.
func TestRewriteSeamCRAtoBLA(t *testing.T) {
	const vpid = 256
	mkPkt := func(pusi bool, payload []byte) []byte {
		b := make([]byte, PacketSize)
		b[0] = 0x47
		b[1] = byte(vpid >> 8)
		if pusi {
			b[1] |= 0x40
		}
		b[2] = byte(vpid & 0xff)
		b[3] = 0x10 // adaptation_field_control = payload only, CC 0
		copy(b[4:], payload)
		return b
	}

	cra := byte(hevc.NALU_CRA) << 1 // HEVC NAL header byte for a CRA, layer 0
	// First AU payload: PES start (00 00 01 E0 ...) then a CRA slice start code.
	au := []byte{
		0, 0, 1, 0xE0, 0x80, 0x80, 0x05, // PES header
		0x21, 0, 0, 0, 0, // stray 0x21 not preceded by a start code
		0, 0, 1, cra, 0xAA, 0xBB, // CRA slice NAL
	}
	craOff := 4 + 15 // payload starts at byte 4; CRA header is au[15]

	seg := append(mkPkt(true, au), mkPkt(true, []byte{0, 0, 1, 0xE0})...)
	n := len(seg) / PacketSize

	flips := rewriteSeamCRAtoBLA(seg, n, vpid)
	if flips != 1 {
		t.Fatalf("flips = %d, want 1", flips)
	}
	want := (cra & 0x81) | (byte(hevc.NALU_BLA_W_LP) << 1)
	if got := seg[craOff]; got != want {
		t.Fatalf("CRA byte = 0x%02x, want BLA 0x%02x", got, want)
	}
	// PES stream-id (0xE0) and the stray 0x21 must be unchanged.
	if seg[4+3] != 0xE0 {
		t.Errorf("PES stream-id was modified: 0x%02x", seg[4+3])
	}
	if seg[4+7] != 0x21 {
		t.Errorf("non-start-code 0x21 was modified: 0x%02x", seg[4+7])
	}
}
