package internal

// Codec identifies the media codec of an elementary stream, as used by the
// mp2ts-loop engine. It is distinct from the string codec labels produced by
// ParseAstitsElementaryStreamInfo for the analysis tools.
type Codec uint32

const (
	CODEC_AVC Codec = iota
	CODEC_HEVC
	CODEC_MPEG2V // MPEG-2 video (ISO/IEC 13818-2, H.262)
	CODEC_AAC
	CODEC_AC3
	CODEC_EC3
	CODEC_MP2
	CODEC_SCTE35
	CODEC_ID3
	CODEC_PRIVATE
	CODEC_TELETEXT
	CODEC_DVB_SUBTITLES
	CODEC_UNKNOWN
)

func (c Codec) String() string {
	switch c {
	case CODEC_AVC:
		return "avc"
	case CODEC_HEVC:
		return "hevc"
	case CODEC_MPEG2V:
		return "mpeg2video"
	case CODEC_AAC:
		return "aac"
	case CODEC_AC3:
		return "ac-3"
	case CODEC_EC3:
		return "ec-3"
	case CODEC_MP2:
		return "mp2"
	case CODEC_SCTE35:
		return "scte-35"
	case CODEC_ID3:
		return "id3"
	case CODEC_PRIVATE:
		return "private"
	case CODEC_TELETEXT:
		return "teletext"
	case CODEC_DVB_SUBTITLES:
		return "dvb-subtitles"
	default:
		return "unknown"
	}
}

// IsVideo reports whether the codec is a video codec.
func (c Codec) IsVideo() bool {
	return c == CODEC_AVC || c == CODEC_HEVC || c == CODEC_MPEG2V
}

// IsAudio reports whether the codec is an audio codec.
func (c Codec) IsAudio() bool {
	switch c {
	case CODEC_AAC, CODEC_AC3, CODEC_EC3, CODEC_MP2:
		return true
	default:
		return false
	}
}
