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
