package internal

// AudioFrame is one coded audio frame that starts in a PES payload.
type AudioFrame struct {
	PTS  int64
	Size int // total frame size in bytes (header + payload)
}

// AudioFramer splits PES payloads into individual audio frames. Implementations
// are codec-specific (AAC/ADTS now; AC-3/MP2 later) and stateful: they learn the
// sample rate from the first frame. This is the seam that lets the loop engine
// handle non-PES-aligned audio and compute drift-free audio fits per codec.
type AudioFramer interface {
	// Name identifies the framer/codec.
	Name() string
	// SampleRate is the audio sample rate in Hz (0 until the first Split).
	SampleRate() int
	// FrameDurTicks is the 90 kHz duration of one audio frame (0 until the first
	// Split).
	FrameDurTicks() int64
	// Split parses the frames that start in payload. alignOffset is the number of
	// bytes at the start belonging to a frame begun in the previous PES;
	// pesPayloadLen is the PES payload length from the header (<=0 if unbounded).
	// It returns the frames (absolute PTS) and missing = bytes of the last frame
	// that spill into the next PES (the carry-out for the next Split; 0 when the
	// PES ends on a frame boundary).
	Split(payload []byte, pesPayloadLen, alignOffset int, pts int64) (frames []AudioFrame, missing int, err error)
}

// newAudioFramer returns an AudioFramer for the codec, or nil if unsupported.
func newAudioFramer(c Codec) AudioFramer {
	switch c {
	case CODEC_AAC:
		return NewADTSFramer()
	case CODEC_MP2:
		return NewMP2Framer()
	case CODEC_AC3:
		return NewAC3Framer()
	default:
		return nil
	}
}
