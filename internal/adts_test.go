package internal

import (
	"testing"

	"github.com/Eyevinn/mp4ff/aac"
)

// adtsFrame builds one ADTS frame (7-byte header + plLen payload bytes) at 48 kHz.
func adtsFrame(t *testing.T, plLen int) []byte {
	t.Helper()
	hdr, err := aac.NewADTSHeader(48000, 2, aac.AAClc, uint16(plLen))
	if err != nil {
		t.Fatalf("NewADTSHeader: %v", err)
	}
	return append(hdr.Encode(), make([]byte, plLen)...)
}

func TestADTSSplitSingle(t *testing.T) {
	f := adtsFrame(t, 100) // size 107
	fr := NewADTSFramer()
	frames, missing, err := fr.Split(f, len(f), 0, 900000)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || missing != 0 {
		t.Fatalf("got %d frames, missing %d", len(frames), missing)
	}
	if frames[0].PTS != 900000 || frames[0].Size != 107 {
		t.Errorf("frame = %+v", frames[0])
	}
	if fr.SampleRate() != 48000 || fr.FrameDurTicks() != 1920 {
		t.Errorf("rate=%d frameDur=%d", fr.SampleRate(), fr.FrameDurTicks())
	}
}

func TestADTSSplitMultiAligned(t *testing.T) {
	var payload []byte
	for i := 0; i < 3; i++ {
		payload = append(payload, adtsFrame(t, 100)...)
	}
	fr := NewADTSFramer()
	frames, missing, err := fr.Split(payload, len(payload), 0, 900000)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || missing != 0 {
		t.Fatalf("got %d frames, missing %d", len(frames), missing)
	}
	for i, want := range []int64{900000, 901920, 903840} {
		if frames[i].PTS != want {
			t.Errorf("frame %d PTS=%d want %d", i, frames[i].PTS, want)
		}
	}
}

func TestADTSSplitSpill(t *testing.T) {
	a := adtsFrame(t, 100) // 107
	b := adtsFrame(t, 100) // 107
	payload := append(append([]byte{}, a...), b...)
	// Pretend the PES payload ends 5 bytes before the end of frame b.
	fr := NewADTSFramer()
	frames, missing, err := fr.Split(payload, len(payload)-5, 0, 900000)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || missing != 5 {
		t.Fatalf("got %d frames, missing %d (want 2, 5)", len(frames), missing)
	}
}

func TestADTSSplitAlignCarry(t *testing.T) {
	// Second PES begins with 5 carried-over bytes, then frame C.
	c := adtsFrame(t, 80) // 87
	payload := append(make([]byte, 5), c...)
	fr := NewADTSFramer()
	frames, missing, err := fr.Split(payload, len(payload), 5, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || missing != 0 {
		t.Fatalf("got %d frames, missing %d (want 1, 0)", len(frames), missing)
	}
	if frames[0].PTS != 1000000 || frames[0].Size != 87 {
		t.Errorf("frame = %+v", frames[0])
	}
}
