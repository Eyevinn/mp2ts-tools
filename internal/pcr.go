package internal

import (
	"github.com/Comcast/gots/v2"
	"github.com/Comcast/gots/v2/packet"
	"github.com/Comcast/gots/v2/packet/adaptationfield"
)

// RewritePCR shifts the PCR value in a packet by dtsOffset (90 kHz units),
// applying the *300 conversion to the 27 MHz PCR domain and wrapping. It is a
// no-op for packets without a PCR.
func RewritePCR(pkt *packet.Packet, dtsOffset uint64) {
	pcrBytes, err := adaptationfield.PCR(pkt)
	if err != nil {
		return
	}
	pcr := gots.ExtractPCR(pcrBytes)
	newPCR := (pcr + dtsOffset*300) % uint64(PcrWrap)
	gots.InsertPCR(pcrBytes, newPCR)
}

// packetPCR returns the PCR (27 MHz) of a packet and whether it has one. It
// guards adaptation-field presence so it is safe on any packet.
func packetPCR(pkt *packet.Packet) (int64, bool) {
	if !packet.ContainsAdaptationField(pkt) || adaptationfield.Length(pkt) == 0 {
		return 0, false
	}
	if !adaptationfield.HasPCR(pkt) {
		return 0, false
	}
	pcrBytes, err := adaptationfield.PCR(pkt)
	if err != nil {
		return 0, false
	}
	return int64(gots.ExtractPCR(pcrBytes)), true
}

// GetPCRValue returns the PCR (27 MHz) of a packet, or -1 if absent.
func GetPCRValue(pkt *packet.Packet) int64 {
	if pcr, ok := packetPCR(pkt); ok {
		return pcr
	}
	return -1
}

// SignedPTSJitter returns the deviation of ptsDiff from the nearest multiple of
// idealStep, in the range [-idealStep/2, idealStep/2). It is used to snap
// jittery PTS/DTS steps onto a constant-frame-rate grid. idealStep must be > 0.
func SignedPTSJitter(ptsDiff, idealStep int64) int64 {
	return (ptsDiff+8*idealStep+idealStep/2)%idealStep - idealStep/2
}
