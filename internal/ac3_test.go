package internal

import "testing"

// ac3Frame48k builds one 48 kHz AC-3 frame for the given frmsizecod. With
// frmsizecod=20 (192 kbit/s) the frame is 384 words = 768 bytes.
func ac3Frame48k(t *testing.T, frmsizecod int) []byte {
	t.Helper()
	idx := 3*frmsizecod + 0 // fscod 0 = 48 kHz
	size := int(ac3FrameSizesWords[idx]) * 2
	f := make([]byte, size)
	f[0], f[1] = 0x0b, 0x77 // sync
	f[4] = byte(frmsizecod) // fscod 0 in top 2 bits, frmsizecod in low 6
	return f
}

func TestAC3SplitSingle(t *testing.T) {
	f := ac3Frame48k(t, 20) // 768 bytes
	fr := NewAC3Framer()
	frames, missing, err := fr.Split(f, len(f), 0, 900000)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || missing != 0 {
		t.Fatalf("got %d frames, missing %d", len(frames), missing)
	}
	if frames[0].PTS != 900000 || frames[0].Size != 768 {
		t.Errorf("frame = %+v", frames[0])
	}
	if fr.SampleRate() != 48000 || fr.FrameDurTicks() != 2880 {
		t.Errorf("rate=%d frameDur=%d (want 48000/2880)", fr.SampleRate(), fr.FrameDurTicks())
	}
}

func TestAC3SplitMulti(t *testing.T) {
	var payload []byte
	for i := 0; i < 3; i++ {
		payload = append(payload, ac3Frame48k(t, 20)...)
	}
	fr := NewAC3Framer()
	frames, missing, err := fr.Split(payload, len(payload), 0, 900000)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || missing != 0 {
		t.Fatalf("got %d frames, missing %d", len(frames), missing)
	}
	for i, want := range []int64{900000, 902880, 905760} {
		if frames[i].PTS != want {
			t.Errorf("frame %d PTS=%d want %d", i, frames[i].PTS, want)
		}
	}
}

func TestAC3SplitSpill(t *testing.T) {
	payload := append(ac3Frame48k(t, 20), ac3Frame48k(t, 20)...)
	fr := NewAC3Framer()
	frames, missing, err := fr.Split(payload, len(payload)-7, 0, 900000)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || missing != 7 {
		t.Fatalf("got %d frames, missing %d (want 2, 7)", len(frames), missing)
	}
}
