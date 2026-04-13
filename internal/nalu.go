package internal

type NaluFrameData struct {
	PID     uint16     `json:"pid"`
	RAI     bool       `json:"rai"`
	PTS     int64      `json:"pts"`
	DTS     int64      `json:"dts,omitempty"`
	ImgType string     `json:"imgType,omitempty"`
	NALUS   []NaluData `json:"nalus,omitempty"`
}

type NaluData struct {
	Type string `json:"type"`
	Len  int    `json:"len"`
	Data any    `json:"data,omitempty"`
}

type SeiOut struct {
	Msg     string `json:"msg"`
	Payload any    `json:"payload,omitempty"`
}

// PicTimingAvcOut is a richer representation of PicTimingAvcSEI that exposes all fields
// including those hidden by the mp4ff ClockTSAvc MarshalJSON.
type PicTimingAvcOut struct {
	PictStruct     uint8              `json:"pict_struct"`
	CpbRemovalDelay *uint             `json:"cpb_removal_delay,omitempty"`
	DpbOutputDelay  *uint             `json:"dpb_output_delay,omitempty"`
	Clocks         []ClockTSAvcOut    `json:"clocks"`
}

// ClockTSAvcOut exposes all fields of a clock timestamp from pic_timing SEI.
type ClockTSAvcOut struct {
	ClockTimeStampFlag bool   `json:"clock_timestamp_flag"`
	CtType             *byte  `json:"ct_type,omitempty"`
	NuitFieldBasedFlag *bool  `json:"nuit_field_based_flag,omitempty"`
	CountingType       *byte  `json:"counting_type,omitempty"`
	FullTimeStampFlag  *bool  `json:"full_timestamp_flag,omitempty"`
	DiscontinuityFlag  *bool  `json:"discontinuity_flag,omitempty"`
	CntDroppedFlag     *bool  `json:"cnt_dropped_flag,omitempty"`
	NFrames            *byte  `json:"n_frames,omitempty"`
	Time               string `json:"time,omitempty"`
	TimeOffset         *int   `json:"time_offset,omitempty"`
}
