package common

const (
	PacketSize = 188
	PtsWrap    = 1 << 33
	PcrWrap    = PtsWrap * 300
	TimeScale  = 90000
)

func SignedPTSDiff(p2, p1 int64) int64 {
	return (p2-p1+3*PtsWrap/2)%PtsWrap - PtsWrap/2
}

func UnsignedPTSDiff(p2, p1 int64) int64 {
	return (p2 - p1 + 2*PtsWrap) % PtsWrap
}

func AddPTS(p1, p2 int64) int64 {
	return (p1 + p2) % PtsWrap
}
