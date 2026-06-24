package internal

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

// AddPCR adds delta (27 MHz units) to a PCR value, wrapping in the 42-bit PCR domain.
// Negative results are normalized to be non-negative.
func AddPCR(pcr, delta int64) int64 {
	r := (pcr + delta) % PcrWrap
	if r < 0 {
		r += PcrWrap
	}
	return r
}

// SignedPCRDiff returns p2-p1 in the PCR (27 MHz) domain, wrap-aware, in
// [-PcrWrap/2, PcrWrap/2).
func SignedPCRDiff(p2, p1 int64) int64 {
	return (p2-p1+3*PcrWrap/2)%PcrWrap - PcrWrap/2
}

// UnsignedPCRDiff returns the non-negative wrap-aware difference p2-p1 in the PCR domain.
func UnsignedPCRDiff(p2, p1 int64) int64 {
	return (p2 - p1 + 2*PcrWrap) % PcrWrap
}
