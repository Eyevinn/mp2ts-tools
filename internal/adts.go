package internal

import (
	"bytes"
	"fmt"

	"github.com/Eyevinn/mp4ff/aac"
)

// adtsFrequencyTable maps the 4-bit ADTS sampling_frequency_index to Hz.
var adtsFrequencyTable = map[byte]int{
	0: 96000, 1: 88200, 2: 64000, 3: 48000, 4: 44100, 5: 32000,
	6: 24000, 7: 22050, 8: 16000, 9: 12000, 10: 11025, 11: 8000, 12: 7350,
}

// ADTSFramer splits AAC/ADTS PES payloads into frames, including frames that are
// not aligned to PES boundaries (via the alignOffset/missing carry). One AAC
// frame is 1024 samples.
type ADTSFramer struct {
	sampleRate int
	frameDur   int64
}

// NewADTSFramer returns a new AAC/ADTS framer.
func NewADTSFramer() *ADTSFramer { return &ADTSFramer{} }

func (f *ADTSFramer) Name() string         { return "aac-adts" }
func (f *ADTSFramer) SampleRate() int      { return f.sampleRate }
func (f *ADTSFramer) FrameDurTicks() int64 { return f.frameDur }

// Split implements AudioFramer for AAC/ADTS.
func (f *ADTSFramer) Split(payload []byte, pesPayloadLen, alignOffset int, pts int64) ([]AudioFrame, int, error) {
	effLen := pesPayloadLen
	if effLen <= 0 {
		effLen = len(payload)
	}
	buf := bytes.NewBuffer(payload)
	pos := 0
	if alignOffset > 0 {
		if alignOffset > buf.Len() {
			return nil, 0, fmt.Errorf("alignOffset %d exceeds payload %d", alignOffset, buf.Len())
		}
		buf.Next(alignOffset)
		pos += alignOffset
	}

	var frames []AudioFrame
	nr := 0
	for {
		hdr, offset, err := aac.DecodeADTSHeader(buf)
		if err != nil {
			if len(frames) > 0 {
				return frames, 0, nil // no further frame starts in this PES
			}
			return nil, 0, fmt.Errorf("decode ADTS header: %w", err)
		}
		if alignOffset != 0 && offset != 0 {
			return nil, 0, fmt.Errorf("ADTS alignment mismatch: alignOffset=%d offset=%d", alignOffset, offset)
		}
		freq := adtsFrequencyTable[hdr.SamplingFrequencyIndex]
		if freq == 0 {
			return nil, 0, fmt.Errorf("unsupported ADTS sampling index %d", hdr.SamplingFrequencyIndex)
		}
		if f.sampleRate == 0 {
			f.sampleRate = freq
			f.frameDur = int64(1024) * TimeScale / int64(freq)
		}
		framePayload := int(hdr.PayloadLength)
		size := int(hdr.HeaderLength) + framePayload
		framePTS := AddPTS(pts, int64(nr)*1024*TimeScale/int64(freq))
		frames = append(frames, AudioFrame{PTS: framePTS, Size: size})

		pos += offset + int(hdr.HeaderLength)
		missing := framePayload + pos - effLen
		if missing > 0 {
			return frames, missing, nil // last frame spills into the next PES
		}
		if missing == 0 {
			return frames, 0, nil // PES ends exactly on a frame boundary
		}
		if framePayload > buf.Len() {
			return frames, 0, nil // defensive: not enough bytes for the payload
		}
		buf.Next(framePayload)
		pos += framePayload
		nr++
	}
}
