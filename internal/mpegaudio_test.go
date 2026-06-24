package internal

import "testing"

// mp2Frame builds one MPEG-1 Layer 2 frame: 48 kHz, 128 kbit/s, stereo, no
// padding -> frame size 384 bytes (header [0xFF,0xFD,0x84,0x00] + 380 payload).
func mp2Frame() []byte {
	return append([]byte{0xFF, 0xFD, 0x84, 0x00}, make([]byte, 380)...)
}

func TestMP2SplitSingle(t *testing.T) {
	f := mp2Frame()
	fr := NewMP2Framer()
	frames, missing, err := fr.Split(f, len(f), 0, 900000)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || missing != 0 {
		t.Fatalf("got %d frames, missing %d", len(frames), missing)
	}
	if frames[0].PTS != 900000 || frames[0].Size != 384 {
		t.Errorf("frame = %+v", frames[0])
	}
	if fr.SampleRate() != 48000 || fr.FrameDurTicks() != 2160 {
		t.Errorf("rate=%d frameDur=%d (want 48000/2160)", fr.SampleRate(), fr.FrameDurTicks())
	}
}

func TestMP2SplitMulti(t *testing.T) {
	var payload []byte
	for i := 0; i < 3; i++ {
		payload = append(payload, mp2Frame()...)
	}
	fr := NewMP2Framer()
	frames, missing, err := fr.Split(payload, len(payload), 0, 900000)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || missing != 0 {
		t.Fatalf("got %d frames, missing %d", len(frames), missing)
	}
	for i, want := range []int64{900000, 902160, 904320} {
		if frames[i].PTS != want {
			t.Errorf("frame %d PTS=%d want %d", i, frames[i].PTS, want)
		}
	}
}

func TestMP2SplitSpill(t *testing.T) {
	payload := append(mp2Frame(), mp2Frame()...)
	fr := NewMP2Framer()
	frames, missing, err := fr.Split(payload, len(payload)-5, 0, 900000)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || missing != 5 {
		t.Fatalf("got %d frames, missing %d (want 2, 5)", len(frames), missing)
	}
}
