package app

const (
	ptsWrap = 1 << 33
	pcrWrap = ptsWrap * 300
)

func SignedPTSDiff(p2, p1 int64) int64 {
	return (p2-p1+3*ptsWrap/2)%ptsWrap - ptsWrap/2
}

func UnsignedPTSDiff(p2, p1 int64) int64 {
	return (p2 - p1 + 2*ptsWrap) % ptsWrap
}

func AddPTS(p1, p2 int64) int64 {
	return (p1 + p2) % ptsWrap
}
