package internal

import "fmt"

// ac3FrameSizesWords is the AC-3 frame size in 16-bit words, indexed by
// 3*frmsizecod + fscod (fscod: 0=48 kHz, 1=44.1 kHz, 2=32 kHz). Per ETSI TS 102
// 366 (Dolby Digital / ATSC A/52). Each AC-3 frame is 1536 samples.
var ac3FrameSizesWords = []uint16{
	64, 69, 96, // 32 kbit/s
	64, 70, 96,
	80, 87, 120, // 40
	80, 88, 120,
	96, 104, 144, // 48
	96, 105, 144,
	112, 121, 168, // 56
	112, 122, 168,
	128, 139, 192, // 64
	128, 140, 192,
	160, 174, 240, // 80
	160, 175, 240,
	192, 208, 288, // 96
	192, 209, 288,
	224, 243, 336, // 112
	224, 244, 336,
	256, 278, 384, // 128
	256, 279, 384,
	320, 348, 480, // 160
	320, 349, 480,
	384, 417, 576, // 192
	384, 418, 576,
	448, 487, 672, // 224
	448, 488, 672,
	512, 557, 768, // 256
	512, 558, 768,
	640, 696, 960, // 320
	640, 697, 960,
	768, 835, 1152, // 384
	768, 836, 1152,
	896, 975, 1344, // 448
	896, 976, 1344,
	1024, 1114, 1536, // 512
	1024, 1115, 1536,
	1152, 1253, 1728, // 576
	1152, 1254, 1728,
	1280, 1393, 1920, // 640
	1280, 1394, 1920,
}

var ac3SampleRates = [3]int{48000, 44100, 32000}

const ac3SamplesPerFrame = 1536

// AC3Framer splits AC-3 (Dolby Digital) PES payloads into frames. Like MP2,
// AC-3 frames have no length field in the PES; the frame size is derived from
// the syncframe header (fscod + frmsizecod), so a frame spilling past the PES is
// carried to the next via missing/alignOffset. E-AC-3 (bsid 16) is not handled.
type AC3Framer struct {
	sampleRate int
	frameDur   int64
}

// NewAC3Framer returns a new AC-3 framer.
func NewAC3Framer() *AC3Framer { return &AC3Framer{} }

func (f *AC3Framer) Name() string         { return "ac-3" }
func (f *AC3Framer) SampleRate() int      { return f.sampleRate }
func (f *AC3Framer) FrameDurTicks() int64 { return f.frameDur }

// ac3SyncIndex returns the index of the next AC-3 sync word (0x0B77) at or after
// from, or -1 if none.
func ac3SyncIndex(data []byte, from int) int {
	for i := from; i+1 < len(data); i++ {
		if data[i] == 0x0b && data[i+1] == 0x77 {
			return i
		}
	}
	return -1
}

// Split implements AudioFramer for AC-3.
func (f *AC3Framer) Split(payload []byte, pesPayloadLen, alignOffset int, pts int64) ([]AudioFrame, int, error) {
	effLen := pesPayloadLen
	if effLen <= 0 {
		effLen = len(payload)
	}
	if alignOffset > len(payload) {
		return nil, 0, fmt.Errorf("alignOffset %d exceeds payload %d", alignOffset, len(payload))
	}
	pos := alignOffset
	var frames []AudioFrame
	nr := 0
	for {
		if pos+5 > len(payload) {
			if len(frames) > 0 {
				return frames, 0, nil
			}
			return nil, 0, fmt.Errorf("no AC-3 header in PES")
		}
		syncPos := ac3SyncIndex(payload, pos)
		if syncPos < 0 {
			if len(frames) > 0 {
				return frames, 0, nil
			}
			return nil, 0, fmt.Errorf("no AC-3 sync in PES")
		}
		if (alignOffset != 0 || nr > 0) && syncPos != pos {
			return nil, 0, fmt.Errorf("AC-3 alignment mismatch at frame %d: offset %d", nr, syncPos-pos)
		}
		if syncPos+5 > len(payload) {
			if len(frames) > 0 {
				return frames, 0, nil
			}
			return nil, 0, fmt.Errorf("AC-3 header straddles PES boundary")
		}
		b4 := payload[syncPos+4]
		fscod := (b4 >> 6) & 0x3
		if fscod == 3 {
			return nil, 0, fmt.Errorf("invalid AC-3 fscod")
		}
		frmsizecod := int(b4 & 0x3f)
		idx := 3*frmsizecod + int(fscod)
		if idx >= len(ac3FrameSizesWords) {
			return nil, 0, fmt.Errorf("invalid AC-3 frmsizecod %d", frmsizecod)
		}
		frameSize := int(ac3FrameSizesWords[idx]) * 2
		freq := ac3SampleRates[fscod]
		if f.sampleRate == 0 {
			f.sampleRate = freq
			f.frameDur = int64(ac3SamplesPerFrame) * TimeScale / int64(freq)
		}
		framePTS := AddPTS(pts, int64(nr)*ac3SamplesPerFrame*TimeScale/int64(freq))
		frames = append(frames, AudioFrame{PTS: framePTS, Size: frameSize})

		end := syncPos + frameSize
		missing := end - effLen
		if missing > 0 {
			return frames, missing, nil
		}
		if missing == 0 {
			return frames, 0, nil
		}
		pos = end
		nr++
	}
}
