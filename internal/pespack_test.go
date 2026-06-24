package internal

import (
	"bytes"
	"testing"

	"github.com/Comcast/gots/v2/pes"
)

// reassemblePES concatenates the PES payload bytes from a sequence of TS packets.
func reassemblePES(t *testing.T, pkts []byte, wantPID int) []byte {
	t.Helper()
	if len(pkts)%PacketSize != 0 {
		t.Fatalf("packet bytes not a multiple of %d", PacketSize)
	}
	var out []byte
	for i := 0; i < len(pkts)/PacketSize; i++ {
		p := pkts[i*PacketSize : (i+1)*PacketSize]
		if p[0] != 0x47 {
			t.Fatalf("packet %d has no sync", i)
		}
		pid := (int(p[1]&0x1f) << 8) | int(p[2])
		if pid != wantPID {
			t.Fatalf("packet %d pid=%d want %d", i, pid, wantPID)
		}
		pusi := p[1]&0x40 != 0
		if (i == 0) != pusi {
			t.Fatalf("packet %d PUSI=%v (first=%v)", i, pusi, i == 0)
		}
		off := 4
		if afc := (p[3] >> 4) & 3; afc == 2 || afc == 3 {
			off = 5 + int(p[4])
		}
		out = append(out, p[off:]...)
	}
	return out
}

func TestPacketizePESRoundTrip(t *testing.T) {
	frame := make([]byte, 500)
	for i := range frame {
		frame[i] = byte(i * 7)
	}
	const pid = 257
	const streamID = 0xbd
	pts := int64(123456)

	pkts := packetizePES(pid, buildAudioPES(streamID, pts, frame))
	if len(pkts) != 3*PacketSize { // (14+500)=514 -> 184+184+146
		t.Fatalf("got %d packets, want 3", len(pkts)/PacketSize)
	}
	pesBytes := reassemblePES(t, pkts, pid)
	ph, err := pes.NewPESHeader(pesBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ph.HasPTS() || int64(ph.PTS()) != pts {
		t.Errorf("PTS = %d (hasPTS=%v) want %d", ph.PTS(), ph.HasPTS(), pts)
	}
	if ph.StreamId() != streamID {
		t.Errorf("streamID = 0x%02x want 0x%02x", ph.StreamId(), streamID)
	}
	if !bytes.Equal(ph.Data(), frame) {
		t.Errorf("payload mismatch: got %d bytes, want %d", len(ph.Data()), len(frame))
	}
}

func TestRepackageFramesAsPES(t *testing.T) {
	f0 := bytes.Repeat([]byte{0xaa}, 300)
	f1 := bytes.Repeat([]byte{0xbb}, 300)
	f2 := bytes.Repeat([]byte{0xcc}, 300)
	pkts := repackageFramesAsPES(257, 0xbd, [][]byte{f0, f1, f2}, []int64{1000, 3880, 6760})
	// Three independent 1-frame PES, each starting with a PUSI packet.
	pusiCount := 0
	for i := 0; i < len(pkts)/PacketSize; i++ {
		if pkts[i*PacketSize+1]&0x40 != 0 {
			pusiCount++
		}
	}
	if pusiCount != 3 {
		t.Fatalf("got %d PUSI packets, want 3 (one per frame PES)", pusiCount)
	}
}
