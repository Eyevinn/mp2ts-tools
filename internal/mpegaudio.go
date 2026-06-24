package internal

import "fmt"

// MPEG-1/2 audio (MP2 = Layer 2) header tables, per
// https://www.opennet.ru/docs/formats/mpeghdr.html. MPEG-2 AAC is separate.

var mpaBitrates = [][][]uint16{
	{ // MPEG-2.5
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0},
	},
	{ // reserved
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	},
	{ // MPEG-2
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0},
	},
	{ // MPEG-1
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0},
		{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0},
	},
}

var mpaSampleRates = [4][4]uint16{
	{11025, 12000, 8000, 0},  // MPEG-2.5
	{0, 0, 0, 0},             // reserved
	{22050, 24000, 16000, 0}, // MPEG-2
	{44100, 48000, 32000, 0}, // MPEG-1
}

// mpaSamplesPerFrameDiv8 is samples-per-frame / 8, indexed by [version][rawLayer].
var mpaSamplesPerFrameDiv8 = [][]byte{
	{0, 72, 144, 12},  // MPEG-2.5
	{0, 0, 0, 0},      // reserved
	{0, 72, 144, 12},  // MPEG-2
	{0, 144, 144, 12}, // MPEG-1
}

// MPEGAudioHeader is a decoded MPEG-1/2 audio frame header.
type MPEGAudioHeader struct {
	version    byte
	layer      byte
	bitrate    byte
	sampleRate byte
	paddingBit byte
}

// SampleRate returns the sample rate in Hz.
func (m MPEGAudioHeader) SampleRate() uint16 { return mpaSampleRates[m.version][m.sampleRate] }

// Bitrate returns the bitrate in kbit/s.
func (m MPEGAudioHeader) Bitrate() uint16 { return mpaBitrates[m.version][m.layer][m.bitrate] }

// SamplesPerFrame returns the number of samples in the frame.
func (m MPEGAudioHeader) SamplesPerFrame() uint16 {
	return 8 * uint16(mpaSamplesPerFrameDiv8[m.version][m.layer])
}

// FrameSize returns the frame size in bytes (Layer 2/3 formula; MP2 is Layer 2).
func (m MPEGAudioHeader) FrameSize() uint16 {
	ssf := int(m.SamplesPerFrame()) / 8
	br := int(m.Bitrate()) * 1000
	sr := int(m.SampleRate())
	if sr == 0 {
		return 0
	}
	return uint16(ssf*br/sr + int(m.paddingBit))
}

// FindMPEGAudioHeaderIndex returns the index of the next MPEG audio frame sync.
func FindMPEGAudioHeaderIndex(data []byte) (int, bool) {
	for pos := 0; pos < len(data)-4; pos++ {
		if data[pos] == 0xff && (data[pos+1]&0xe0) == 0xe0 &&
			(data[pos+2]&0xf0) != 0xf0 && (data[pos+2]&0x0c) != 0x0c &&
			(data[pos+1]&0x18) != 0x08 && (data[pos+1]&0x06) != 0x00 {
			return pos, true
		}
	}
	return -1, false
}

// DecodeMPEGAudioHeader decodes a 4-byte MPEG audio header.
func DecodeMPEGAudioHeader(hdr []byte) (MPEGAudioHeader, error) {
	if len(hdr) < 4 {
		return MPEGAudioHeader{}, fmt.Errorf("need 4 header bytes")
	}
	if hdr[0] != 0xff || (hdr[1]&0xe0) != 0xe0 ||
		(hdr[2]&0xf0) == 0xf0 || (hdr[2]&0x0c) == 0x0c {
		return MPEGAudioHeader{}, fmt.Errorf("bad MPEG audio header")
	}
	return MPEGAudioHeader{
		version:    (hdr[1] >> 3) & 3,
		layer:      (hdr[1] >> 1) & 3,
		bitrate:    hdr[2] >> 4,
		sampleRate: (hdr[2] >> 2) & 3,
		paddingBit: (hdr[2] >> 1) & 1,
	}, nil
}

// MP2Framer splits MPEG-1/2 audio (Layer 2) PES payloads into frames. Frame
// boundaries are computed from the header (there is no length field), so a frame
// that spills past the PES is carried to the next via the missing/alignOffset.
type MP2Framer struct {
	sampleRate int
	frameDur   int64
}

// NewMP2Framer returns a new MPEG-1/2 audio framer.
func NewMP2Framer() *MP2Framer { return &MP2Framer{} }

func (f *MP2Framer) Name() string         { return "mpeg-audio" }
func (f *MP2Framer) SampleRate() int      { return f.sampleRate }
func (f *MP2Framer) FrameDurTicks() int64 { return f.frameDur }

// Split implements AudioFramer for MPEG-1/2 audio.
func (f *MP2Framer) Split(payload []byte, pesPayloadLen, alignOffset int, pts int64) ([]AudioFrame, int, error) {
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
		if pos+4 > len(payload) {
			if len(frames) > 0 {
				return frames, 0, nil
			}
			return nil, 0, fmt.Errorf("no MPEG audio header in PES")
		}
		idx, ok := FindMPEGAudioHeaderIndex(payload[pos:])
		if !ok {
			if len(frames) > 0 {
				return frames, 0, nil
			}
			return nil, 0, fmt.Errorf("no MPEG audio sync in PES")
		}
		if (alignOffset != 0 || nr > 0) && idx != 0 {
			return nil, 0, fmt.Errorf("MPEG audio alignment mismatch at frame %d: offset %d", nr, idx)
		}
		syncPos := pos + idx
		if syncPos+4 > len(payload) {
			if len(frames) > 0 {
				return frames, 0, nil
			}
			return nil, 0, fmt.Errorf("MPEG audio header straddles PES boundary")
		}
		hdr, err := DecodeMPEGAudioHeader(payload[syncPos : syncPos+4])
		if err != nil {
			return nil, 0, fmt.Errorf("decode MPEG audio header: %w", err)
		}
		sr := int(hdr.SampleRate())
		spf := int(hdr.SamplesPerFrame())
		frameSize := int(hdr.FrameSize())
		if sr == 0 || spf == 0 || frameSize <= 4 {
			return nil, 0, fmt.Errorf("invalid MPEG audio frame (sr=%d spf=%d size=%d)", sr, spf, frameSize)
		}
		if f.sampleRate == 0 {
			f.sampleRate = sr
			f.frameDur = int64(spf) * TimeScale / int64(sr)
		}
		framePTS := AddPTS(pts, int64(nr)*int64(spf)*TimeScale/int64(sr))
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
