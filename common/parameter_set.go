package common

import "encoding/hex"

type ElementaryStreamInfo struct {
	PID   uint16 `json:"pid"`
	Codec string `json:"codec"`
	Type  string `json:"type"`
}

type PsInfo struct {
	PID          uint16 `json:"pid"`
	ParameterSet string `json:"parameterSet"`
	Nr           uint32 `json:"nr"`
	Hex          string `json:"hex"`
	Length       int    `json:"length"`
	Details      any    `json:"details,omitempty"`
}

func (jp *JsonPrinter) PrintPS(pid uint16, psKind string, nr uint32, ps []byte, details any, verbose bool, show bool) {
	hexStr := hex.EncodeToString(ps)
	length := len(hexStr) / 2
	psInfo := PsInfo{
		PID:          pid,
		ParameterSet: psKind,
		Nr:           nr,
		Hex:          hexStr,
		Length:       length,
	}
	if verbose {
		psInfo.Details = details
	}
	jp.Print(psInfo, show)
}
