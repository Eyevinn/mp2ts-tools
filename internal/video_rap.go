package internal

import (
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
)

// RAPInfo describes the random-access properties of a coded picture (or field),
// determined from its first VCL (slice) NAL unit.
type RAPInfo struct {
	IsVCL bool // a VCL (slice) NAL unit was found
	IsIDR bool // AVC IDR, or HEVC IDR_W_RADL / IDR_N_LP
	IsCRA bool // HEVC CRA (open-GOP random access)
	IsRAP bool // any IRAP: IDR, CRA, or BLA
}

// ScanRAP scans the Annex-B NAL start codes in a PES payload and returns the
// random-access properties of the first VCL (slice) NAL unit. Non-VCL NAL units
// (parameter sets, SEI, AUD) are skipped.
func ScanRAP(codec Codec, data []byte) RAPInfo {
	n := len(data)
	for i := 0; i < n-3; i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			if info, isVCL := naluRAP(codec, data[i+3]); isVCL {
				return info
			}
		}
	}
	return RAPInfo{}
}

// naluRAP classifies a single NAL unit from its first header byte. It returns
// the RAP info and whether the NAL is a VCL (slice) unit.
func naluRAP(codec Codec, firstByte byte) (RAPInfo, bool) {
	switch codec {
	case CODEC_AVC:
		switch avc.GetNaluType(firstByte) {
		case avc.NALU_IDR:
			return RAPInfo{IsVCL: true, IsIDR: true, IsRAP: true}, true
		case avc.NALU_NON_IDR:
			return RAPInfo{IsVCL: true}, true
		}
	case CODEC_HEVC:
		nt := hevc.GetNaluType(firstByte)
		if !hevc.IsVideoNaluType(nt) {
			return RAPInfo{}, false
		}
		info := RAPInfo{
			IsVCL: true,
			IsIDR: nt == hevc.NALU_IDR_W_RADL || nt == hevc.NALU_IDR_N_LP,
			IsCRA: nt == hevc.NALU_CRA,
			// IRAP VCL types are BLA_W_LP(16) .. IRAP_VCL23(23).
			IsRAP: nt >= hevc.NALU_BLA_W_LP && nt <= hevc.NALU_IRAP_VCL23,
		}
		return info, true
	}
	return RAPInfo{}, false
}

// IsIDRImage reports whether a PES payload's first slice is an IDR picture.
func IsIDRImage(codec Codec, data []byte) bool {
	return ScanRAP(codec, data).IsIDR
}

// AUInfo describes a coded access unit: the random-access properties of its
// first slice plus which parameter sets it carries.
type AUInfo struct {
	RAPInfo
	HasVPS bool // HEVC only
	HasSPS bool
	HasPPS bool
}

// PSComplete reports whether the access unit carries the parameter sets a
// decoder needs to start here (VPS+SPS+PPS for HEVC, SPS+PPS for AVC).
func (au AUInfo) PSComplete(codec Codec) bool {
	if codec == CODEC_HEVC {
		return au.HasVPS && au.HasSPS && au.HasPPS
	}
	return au.HasSPS && au.HasPPS
}

// ScanAU scans a PES payload once, collecting the parameter sets present before
// the first VCL NAL unit and classifying that VCL unit's random-access kind.
// Parameter sets always precede the first slice in a well-formed access unit, so
// scanning stops at the first VCL NAL.
func ScanAU(codec Codec, data []byte) AUInfo {
	var au AUInfo
	n := len(data)
	for i := 0; i < n-3; i++ {
		if data[i] != 0 || data[i+1] != 0 || data[i+2] != 1 {
			continue
		}
		b := data[i+3]
		switch codec {
		case CODEC_AVC:
			switch avc.GetNaluType(b) {
			case avc.NALU_SPS:
				au.HasSPS = true
			case avc.NALU_PPS:
				au.HasPPS = true
			}
		case CODEC_HEVC:
			switch hevc.GetNaluType(b) {
			case hevc.NALU_VPS:
				au.HasVPS = true
			case hevc.NALU_SPS:
				au.HasSPS = true
			case hevc.NALU_PPS:
				au.HasPPS = true
			}
		}
		if info, isVCL := naluRAP(codec, b); isVCL {
			au.RAPInfo = info
			return au
		}
	}
	return au
}
