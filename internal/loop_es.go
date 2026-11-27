package internal

import (
	"bytes"
	"errors"
	"strings"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/sei"
)

// accessPoint is how cleanly a decoder can start at a picture, from worst to best.
type accessPoint uint8

const (
	apNone accessPoint = iota
	// apRecoveryPoint is an AVC or HEVC picture with a recovery_point SEI (an
	// open GOP without IDR, or gradual decoder refresh).
	apRecoveryPoint
	// apOpenGOP is an HEVC CRA or BLA, or an MPEG-2 I picture whose GOP is open:
	// the pictures that follow it in decode order may reference the GOP before.
	apOpenGOP
	// apClosedGOP is an IDR, or an MPEG-2 I picture with a sequence header that
	// starts a closed GOP (or has no leading B pictures).
	apClosedGOP
)

func (a accessPoint) String() string {
	switch a {
	case apRecoveryPoint:
		return "recovery-point"
	case apOpenGOP:
		return "open-gop"
	case apClosedGOP:
		return "closed-gop"
	}
	return "none"
}

// pictureNumbering is what a decoder numbers a picture by, from its first slice
// header: H.264 frame_num and picture order count fields, H.265
// slice_pic_order_cnt_lsb. A decoder joins a lap that starts at a recovery point
// seamlessly only when the lap's first picture has the numbers of the picture
// after the lap's end.
type pictureNumbering struct {
	frameNum    uint32
	orderLSB    uint32
	orderDelta  int32 // H.264 delta_pic_order_cnt[0]
	bottomField bool
}

// pictureInfo classifies one access unit (a video PES payload).
type pictureInfo struct {
	accessPoint accessPoint
	// parameterSets: the access unit carries its sequence parameters (SPS, or an
	// MPEG-2 sequence header).
	parameterSets bool
	// bPicture: an MPEG-2 B picture. When it follows an open-GOP I picture, that
	// I picture has leading pictures.
	bPicture bool
	// field is FieldTop or FieldBottom for a field picture alone in its PES,
	// whose other field is in the next: an MPEG-2 field picture, an H.264 field
	// (field_pic_flag), an H.265 field (pic_struct 1, 2, 9-12); else FieldFrame.
	field FieldParity
	// secondField: a field that completes the field before it in decode order.
	// A loop never starts or ends on one.
	secondField bool
	// temporalRef is an MPEG-2 picture's temporal_reference, the same for both
	// fields of a frame.
	temporalRef uint16
	// firstShown is the field a frame shows first (MPEG-2 top_field_first,
	// H.264/H.265 pic_struct 3-6), FieldUnknown for a progressive frame or when
	// not signalled.
	firstShown FieldParity
	// shownFields is how many field periods the picture is shown: 1 for a field,
	// 2 for a frame, 3 for a frame that repeats its first field, 4 or 6 for a
	// frame shown twice or three times. 0 is not signalled, taken as 2.
	shownFields uint8
	// numbering is known for AVC and HEVC once the stream's parameter sets are.
	numbering *pictureNumbering
}

// fieldPeriods is how long the picture is shown, in field periods.
func (i *pictureInfo) fieldPeriods() int64 {
	if i.shownFields == 0 {
		return 2
	}
	return int64(i.shownFields)
}

// isField reports a field picture alone in its PES.
func (i *pictureInfo) isField() bool { return i.field == FieldTop || i.field == FieldBottom }

// cadence is what a picture starting a loop and the picture after its end must
// share for the fields to keep alternating across the seam, and a pulldown
// cadence to go on: the field shown first, and how long the frame (a first
// field with its second) is shown.
type cadence struct {
	first  FieldParity
	fields uint8
}

func (i *pictureInfo) cadence() cadence {
	if i.isField() {
		return cadence{i.field, 2}
	}
	return cadence{i.firstShown, uint8(i.fieldPeriods())}
}

// String names the fields shown in order, as in "top-bottom-top".
func (c cadence) String() string {
	var order []string
	switch c.first {
	case FieldTop:
		order = []string{"top", "bottom"}
	case FieldBottom:
		order = []string{"bottom", "top"}
	default:
		switch c.fields {
		case 4:
			return "as two frames"
		case 6:
			return "as three frames"
		}
		return "as a frame"
	}
	var names []string
	for k := 0; k < int(c.fields); k++ {
		names = append(names, order[k%2])
	}
	return strings.Join(names, "-")
}

// setPicStruct sets the field structure from an H.264 or H.265 pic_struct
// (Rec. ITU-T H.264 Table D-1, H.265 Table D.2). An H.264 field is known from
// its slice header instead.
func (i *pictureInfo) setPicStruct(picStruct byte, isHEVC bool) {
	switch picStruct {
	case 0:
		i.shownFields = 2
	case 1, 9, 11:
		if isHEVC {
			i.field, i.shownFields = FieldTop, 1
		}
	case 2, 10, 12:
		if isHEVC {
			i.field, i.shownFields = FieldBottom, 1
		}
	case 3:
		i.firstShown, i.shownFields = FieldTop, 2
	case 4:
		i.firstShown, i.shownFields = FieldBottom, 2
	case 5:
		i.firstShown, i.shownFields = FieldTop, 3
	case 6:
		i.firstShown, i.shownFields = FieldBottom, 3
	case 7:
		i.shownFields = 4
	case 8:
		i.shownFields = 6
	}
}

const (
	avcNaluNonIDR    = 1
	avcNaluIDR       = 5
	avcNaluSEI       = 6
	avcNaluSPS       = 7
	avcNaluPPS       = 8
	avcNaluAUD       = 9
	hevcNaluBLAWLP   = 16
	hevcNaluIRAPMax  = 23
	hevcNaluIDRWRADL = 19
	hevcNaluIDRNLP   = 20
	hevcNaluCRA      = 21
	hevcNaluVPS      = 32
	hevcNaluSPS      = 33
	hevcNaluPPS      = 34
	hevcNaluAUD      = 35
	hevcNaluPrefSEI  = 39
	seiPicTiming     = 1
	seiRecoveryPoint = 6
	rbspStopBit      = 0x80
	// maxNumberedPrefix: an access unit whose first slice starts later is not numbered.
	maxNumberedPrefix = 64 * 1024
)

// forEachStartCode calls visit with the position of the byte after every
// 00 00 01 start code prefix, until visit returns false.
func forEachStartCode(data []byte, visit func(pos int) bool) {
	i := 0
	for i+3 < len(data) {
		if data[i+2] > 1 {
			i += 3
			continue
		}
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			if !visit(i + 3) {
				return
			}
			i += 3
			continue
		}
		i++
	}
}

func hevcNaluType(firstByte byte) byte { return firstByte >> 1 & 0x3f }

// nalBody returns the body of the NAL unit whose header starts at pos, up to
// the next start code or the zero bytes before it.
func nalBody(data []byte, pos, headerSize int) []byte {
	body := min(pos+headerSize, len(data))
	end := body
	for end+2 < len(data) && (data[end] != 0 || data[end+1] != 0 || data[end+2] > 1) {
		end++
	}
	if end+2 >= len(data) {
		end = len(data)
	}
	return data[body:end]
}

// nalUnit returns the NAL unit whose header starts at pos, header included.
func nalUnit(data []byte, pos int) []byte {
	body := nalBody(data, pos, 0)
	return data[pos : pos+len(body)]
}

// seiHasRecoveryPoint reports a recovery_point message in an SEI NAL unit body,
// read over that NAL unit alone with emulation prevention bytes removed. A
// message that does not parse hides the rest of the NAL unit.
func seiHasRecoveryPoint(body []byte, isHEVC bool) bool {
	datas, err := sei.ExtractSEIData(bytes.NewReader(body))
	if err != nil && !errors.Is(err, sei.ErrRbspTrailingBitsMissing) {
		return false
	}
	for i := range datas {
		if datas[i].Type() != seiRecoveryPoint {
			continue
		}
		if isHEVC {
			_, err = sei.DecodeRecoveryPointHevcSEI(&datas[i])
		} else {
			_, err = sei.DecodeRecoveryPointAvcSEI(&datas[i])
		}
		if err == nil {
			return true
		}
	}
	return false
}

// readSEINumber reads one ff-coded SEI number (payload type or size).
func readSEINumber(s []byte, i *int) (int, bool) {
	v := 0
	for *i < len(s) && s[*i] == 0xff {
		v += 0xff
		*i++
	}
	if *i >= len(s) {
		return 0, false
	}
	v += int(s[*i])
	*i++
	return v, true
}

// hasEmulationPrevention reports an emulation prevention byte (00 00 03) that
// ends at or before last.
func hasEmulationPrevention(body []byte, last int) bool {
	for i := 2; i <= last && i < len(body); i++ {
		if body[i-2] == 0 && body[i-1] == 0 && body[i] == 3 {
			return true
		}
	}
	return false
}

// setRecoveryPointBrokenLink sets broken_link_flag in the recovery_point SEI
// message of an SEI NAL unit body, if it has one: H.264 D.1.8, H.265 D.2.8. The
// flag follows an Exp-Golomb count and exact_match_flag. Emulation prevention
// bytes would move the flag, and setting a zero byte could unmask one after it,
// so the message is then left alone. It reports whether it changed.
func setRecoveryPointBrokenLink(s []byte) bool {
	i := 0
	for i < len(s) && s[i] != rbspStopBit {
		typ, ok1 := readSEINumber(s, &i)
		size, ok2 := readSEINumber(s, &i)
		if !ok1 || !ok2 {
			return false
		}
		if typ != seiRecoveryPoint {
			i += size
			continue
		}
		payloadBits := min(size, len(s)-i) * 8
		zeros := 0
		for zeros < payloadBits && s[i+zeros/8]&(0x80>>(zeros%8)) == 0 {
			zeros++
		}
		bit := 2*zeros + 2 // the count, then exact_match_flag
		if bit >= payloadBits {
			return false
		}
		at := i + bit/8
		mask := byte(0x80 >> (bit % 8))
		if s[at]&mask != 0 || hasEmulationPrevention(s, at+2) {
			return false
		}
		s[at] |= mask
		return true
	}
	return false
}

func raise(current *accessPoint, candidate accessPoint) {
	*current = max(*current, candidate)
}

func inspectAVC(data []byte) pictureInfo {
	info := pictureInfo{field: FieldFrame}
	forEachStartCode(data, func(pos int) bool {
		typ := data[pos] & 0x1f
		switch {
		case typ == avcNaluSPS:
			info.parameterSets = true
		case typ == avcNaluSEI && seiHasRecoveryPoint(nalBody(data, pos, 1), false):
			raise(&info.accessPoint, apRecoveryPoint)
		case typ == avcNaluIDR:
			raise(&info.accessPoint, apClosedGOP)
		}
		// SPS and SEI come before the first slice of the access unit.
		return typ != avcNaluIDR && typ != avcNaluNonIDR
	})
	return info
}

func inspectHEVC(data []byte) pictureInfo {
	info := pictureInfo{field: FieldFrame}
	forEachStartCode(data, func(pos int) bool {
		typ := hevcNaluType(data[pos])
		switch {
		case typ == hevcNaluSPS:
			info.parameterSets = true
		case typ == hevcNaluPrefSEI && seiHasRecoveryPoint(nalBody(data, pos, 2), true):
			raise(&info.accessPoint, apRecoveryPoint)
		case typ == hevcNaluIDRWRADL || typ == hevcNaluIDRNLP:
			raise(&info.accessPoint, apClosedGOP)
		case typ >= hevcNaluBLAWLP && typ <= hevcNaluIRAPMax:
			raise(&info.accessPoint, apOpenGOP)
		}
		return typ > 31 // until the first VCL NAL unit
	})
	return info
}

// inspectMPEG2 classifies an MPEG-2 access unit. A random access point is an I
// picture after a sequence header (ITU-T H.222.0, 2.1.67), as clean as a closed
// GOP when closed_gop is set. progressiveSequence is the latest sequence
// extension's, kept from earlier access units.
func inspectMPEG2(data []byte, progressiveSequence *bool) pictureInfo {
	pic := scanMPEG2AU(data)
	info := pictureInfo{parameterSets: pic.seqHeader, bPicture: pic.picType == mpeg2PicB, field: pic.field,
		temporalRef: pic.temporalRef}
	switch {
	case pic.closedGOP:
		info.accessPoint = apClosedGOP
	case pic.openGOP:
		info.accessPoint = apOpenGOP
	}
	if pic.progressiveSequence != nil {
		*progressiveSequence = *pic.progressiveSequence
	}
	// ISO/IEC 13818-2, 6.3.10: top_field_first and repeat_first_field of a frame.
	switch {
	case !pic.extension:
		info.shownFields = 2 // MPEG-1
	case info.isField():
		info.shownFields = 1
	case pic.structure != FieldFrame: // both fields of the frame in one PES
		info.firstShown, info.shownFields = pic.structure, 2
	case *progressiveSequence:
		info.shownFields = 2
		if pic.repeatFirst {
			info.shownFields = 4
			if pic.topFirst {
				info.shownFields = 6
			}
		}
	default:
		info.firstShown, info.shownFields = FieldBottom, 2
		if pic.topFirst {
			info.firstShown = FieldTop
		}
		if pic.repeatFirst {
			info.shownFields = 3
		}
	}
	return info
}

// pictureInspector classifies the access units of one video stream in decode
// order: parameter sets carry over from one access unit to the next, for the
// picture numbering.
type pictureInspector struct {
	codec   Codec
	avcSPS  map[uint32]*avc.SPS
	avcPPS  map[uint32]*avc.PPS
	hevcSPS map[uint32]*hevc.SPS
	hevcPPS map[uint32]*hevc.PPS
	// mpeg2Progressive: the MPEG-2 sequence is progressive_sequence.
	mpeg2Progressive bool
}

func newPictureInspector(codec Codec) *pictureInspector {
	return &pictureInspector{
		codec:   codec,
		avcSPS:  make(map[uint32]*avc.SPS),
		avcPPS:  make(map[uint32]*avc.PPS),
		hevcSPS: make(map[uint32]*hevc.SPS),
		hevcPPS: make(map[uint32]*hevc.PPS),
	}
}

// inspect classifies an access unit, a video PES payload without its header.
func (p *pictureInspector) inspect(es []byte) pictureInfo {
	switch p.codec {
	case CODEC_AVC:
		info := inspectAVC(es)
		p.readHeaders(es, &info)
		return info
	case CODEC_HEVC:
		info := inspectHEVC(es)
		p.readHeaders(es, &info)
		return info
	case CODEC_MPEG2V:
		return inspectMPEG2(es, &p.mpeg2Progressive)
	}
	return pictureInfo{field: FieldFrame}
}

// readHeaders reads the parameter sets and SEI before the first slice and then
// the first slice header, for the picture numbering and the field structure. A
// parameter set that does not parse is skipped; the ones known from earlier
// access units are kept.
func (p *pictureInspector) readHeaders(es []byte, info *pictureInfo) {
	var timing [][]byte // pic_timing SEI payloads
	forEachStartCode(es, func(pos int) bool {
		if pos > maxNumberedPrefix {
			return false
		}
		if p.codec == CODEC_AVC {
			switch typ := es[pos] & 0x1f; typ {
			case avcNaluSPS:
				// The VUI says where pic_struct is in pic_timing; an SPS whose VUI
				// does not parse is still read for the numbering.
				nal := nalUnit(es, pos)
				sps, err := avc.ParseSPSNALUnit(nal, true)
				if err != nil {
					sps, err = avc.ParseSPSNALUnit(nal, false)
				}
				if err == nil {
					p.avcSPS[sps.ParameterID] = sps
				}
			case avcNaluPPS:
				if pps, err := avc.ParsePPSNALUnit(nalUnit(es, pos), p.avcSPS); err == nil {
					p.avcPPS[pps.PicParameterSetID] = pps
				}
			case avcNaluSEI:
				timing = appendPicTiming(timing, nalBody(es, pos, 1))
			case avcNaluIDR, avcNaluNonIDR, 2, 3, 4:
				nal := nalUnit(es, pos)
				if sh, err := avc.ParseSliceHeader(nal, p.avcSPS, p.avcPPS); err == nil {
					info.numbering = &pictureNumbering{frameNum: sh.FrameNum, orderLSB: sh.PicOrderCntLsb,
						orderDelta: sh.DeltaPicOrderCnt[0], bottomField: sh.BottomFieldFlag}
				}
				p.avcFields(info, nal, timing)
				if info.isField() && hasSecondPicture(CODEC_AVC, es, pos) {
					info.field, info.firstShown, info.shownFields = FieldFrame, info.field, 2
				}
				return false
			}
			return true
		}
		switch typ := hevcNaluType(es[pos]); {
		case typ == hevcNaluSPS:
			if sps, err := hevc.ParseSPSNALUnit(nalUnit(es, pos)); err == nil {
				p.hevcSPS[uint32(sps.SpsID)] = sps
			}
		case typ == hevcNaluPPS:
			if pps, err := hevc.ParsePPSNALUnit(nalUnit(es, pos), p.hevcSPS); err == nil {
				p.hevcPPS[pps.PicParameterSetID] = pps
			}
		case typ == hevcNaluPrefSEI:
			timing = appendPicTiming(timing, nalBody(es, pos, 2))
		case typ <= 31:
			nal := nalUnit(es, pos)
			if sh, err := hevc.ParseSliceHeader(nal, p.hevcSPS, p.hevcPPS); err == nil &&
				sh.FirstSliceSegmentInPicFlag {
				info.numbering = &pictureNumbering{orderLSB: uint32(sh.PicOrderCntLsb)}
			}
			p.hevcFields(info, nal, timing)
			if info.isField() && hasSecondPicture(CODEC_HEVC, es, pos) {
				info.field, info.firstShown, info.shownFields = FieldFrame, info.field, 2
			}
			return false
		}
		return true
	})
}

// appendPicTiming appends the payloads of the pic_timing messages of an SEI NAL
// unit body.
func appendPicTiming(timing [][]byte, body []byte) [][]byte {
	datas, err := sei.ExtractSEIData(bytes.NewReader(body))
	if err != nil && !errors.Is(err, sei.ErrRbspTrailingBitsMissing) {
		return timing
	}
	for i := range datas {
		if datas[i].Type() == seiPicTiming {
			timing = append(timing, datas[i].Payload())
		}
	}
	return timing
}

// readBits returns n bits of b from bit offset at.
func readBits(b []byte, at, n int) (uint32, bool) {
	if at+n > len(b)*8 {
		return 0, false
	}
	v := uint32(0)
	for k := at; k < at+n; k++ {
		v = v<<1 | uint32(b[k/8]>>(7-k%8)&1)
	}
	return v, true
}

// sliceStartBytes is enough of a slice NAL unit for what headStart reads.
const sliceStartBytes = 24

// headStart reads the start of a slice header, the bytes after the NAL unit
// header with emulation prevention bytes removed.
type headStart struct {
	b   []byte
	pos int
	bad bool
}

func newHeadStart(nal []byte, headerSize int) *headStart {
	raw := nal[min(headerSize, len(nal)):min(sliceStartBytes, len(nal))]
	b := make([]byte, 0, len(raw))
	for i, c := range raw {
		if c == 3 && i >= 2 && raw[i-1] == 0 && raw[i-2] == 0 {
			continue
		}
		b = append(b, c)
	}
	return &headStart{b: b}
}

func (h *headStart) bits(n int) uint32 {
	v, ok := readBits(h.b, h.pos, n)
	h.bad = h.bad || !ok
	h.pos += n
	return v
}

// ue reads an Exp-Golomb coded number.
func (h *headStart) ue() uint32 {
	zeros := 0
	for !h.bad && h.bits(1) == 0 {
		if zeros++; zeros > 31 {
			h.bad = true
		}
	}
	if h.bad {
		return 0
	}
	return 1<<zeros - 1 + h.bits(zeros)
}

// avcFields sets the field structure of an H.264 picture from its first slice:
// a field from field_pic_flag and bottom_field_flag (Rec. ITU-T H.264 7.3.3), a
// frame's from pic_struct, which follows the CPB and DPB delays in pic_timing
// (D.1.3) when the SPS has HRD parameters. The slice header is read only as far
// as the field flags, so a slice that does not parse further still tells.
func (p *pictureInspector) avcFields(info *pictureInfo, nal []byte, timing [][]byte) {
	h := newHeadStart(nal, 1)
	h.ue() // first_mb_in_slice
	h.ue() // slice_type
	pps := p.avcPPS[h.ue()]
	if pps == nil {
		return
	}
	sps := p.avcSPS[pps.SeqParameterSetID]
	if sps == nil {
		return
	}
	if sps.SeparateColourPlaneFlag {
		h.bits(2) // colour_plane_id
	}
	h.bits(int(sps.Log2MaxFrameNumMinus4) + 4) // frame_num
	if !sps.FrameMbsOnlyFlag && h.bits(1) == 1 {
		bottom := h.bits(1) == 1
		if h.bad {
			return
		}
		info.field, info.shownFields = FieldTop, 1
		if bottom {
			info.field = FieldBottom
		}
		return
	}
	if h.bad || !sps.PicStructPresent() {
		return
	}
	skip := 0
	if sps.CpbDpbDelaysPresent() {
		hrd := sps.VUI.NalHrdParameters
		if hrd == nil {
			hrd = sps.VUI.VclHrdParameters
		}
		if hrd == nil {
			return
		}
		skip = int(hrd.CpbRemovalDelayLengthMinus1+1) + int(hrd.DpbOutputDelayLengthMinus1+1)
	}
	for _, t := range timing {
		if ps, ok := readBits(t, skip, 4); ok {
			info.setPicStruct(byte(ps), false)
			return
		}
	}
}

// hevcFields sets the field structure of an H.265 picture from pic_struct, the
// first field of pic_timing when the SPS VUI has frame_field_info_present_flag
// (Rec. ITU-T H.265 D.2.3). With field_seq_flag, every picture is a field. The
// SPS is found from the first slice's slice_pic_parameter_set_id (7.3.6.1).
func (p *pictureInspector) hevcFields(info *pictureInfo, nal []byte, timing [][]byte) {
	h := newHeadStart(nal, 2)
	if h.bits(1) == 0 { // first_slice_segment_in_pic_flag
		return
	}
	if typ := hevcNaluType(nal[0]); typ >= hevcNaluBLAWLP && typ <= hevcNaluIRAPMax {
		h.bits(1) // no_output_of_prior_pics_flag
	}
	ppsID := h.ue()
	if h.bad {
		return
	}
	pps := p.hevcPPS[ppsID]
	if pps == nil {
		return
	}
	sps := p.hevcSPS[pps.SeqParameterSetID]
	if sps == nil || sps.VUI == nil {
		return
	}
	if sps.VUI.FieldSeqFlag {
		info.shownFields = 1
	}
	if !sps.VUI.FrameFieldInfoPresentFlag {
		return
	}
	for _, t := range timing {
		if len(t) > 0 {
			info.setPicStruct(t[0]>>4, true)
			return
		}
	}
}

// hasSecondPicture reports another picture after the slice at pos in an H.264
// or H.265 access unit: an access unit delimiter, or a slice that starts a
// picture (first_mb_in_slice 0, first_slice_segment_in_pic_flag). A PES with
// both fields of a frame holds a frame.
func hasSecondPicture(codec Codec, es []byte, pos int) bool {
	found := false
	forEachStartCode(es[pos:], func(at int) bool {
		at += pos
		if codec == CODEC_AVC {
			typ := es[at] & 0x1f
			found = typ == avcNaluAUD ||
				((typ == avcNaluIDR || typ == avcNaluNonIDR) && at+1 < len(es) && es[at+1]&0x80 != 0)
		} else {
			typ := hevcNaluType(es[at])
			found = typ == hevcNaluAUD || (typ <= 31 && at+2 < len(es) && es[at+2]&0x80 != 0)
		}
		return !found
	})
	return found
}

// markSeam rewrites the loop's first access unit so a decoder handles what
// precedes it in each lap as an edit: HEVC CRA becomes BLA_W_LP, which makes
// the decoder drop the leading RASL pictures, an open MPEG-2 GOP gets
// broken_link, and an AVC or HEVC recovery_point SEI gets broken_link_flag. It
// returns how many syntax elements it changed.
func markSeam(codec Codec, es []byte) int {
	changed := 0
	switch codec {
	case CODEC_HEVC:
		forEachStartCode(es, func(pos int) bool {
			switch hevcNaluType(es[pos]) {
			case hevcNaluCRA:
				// Keep forbidden_zero_bit and the top bit of nuh_layer_id.
				es[pos] = es[pos]&0x81 | hevcNaluBLAWLP<<1
				changed++
			case hevcNaluPrefSEI:
				if setRecoveryPointBrokenLink(nalBody(es, pos, 2)) {
					changed++
				}
			}
			return true
		})
	case CODEC_MPEG2V:
		forEachStartCode(es, func(pos int) bool {
			code := es[pos]
			if code == mpeg2GroupStart && pos+4 < len(es) {
				if flags := es[pos+4]; flags&mpeg2ClosedGOPBit == 0 && flags&mpeg2BrokenLinkBit == 0 {
					es[pos+4] |= mpeg2BrokenLinkBit
					changed++
				}
				return false
			}
			return code != mpeg2PictureStart
		})
	case CODEC_AVC:
		forEachStartCode(es, func(pos int) bool {
			typ := es[pos] & 0x1f
			if typ == avcNaluSEI && setRecoveryPointBrokenLink(nalBody(es, pos, 1)) {
				changed++
			}
			// SEI comes before the first slice of the access unit.
			return typ != avcNaluIDR && typ != avcNaluNonIDR
		})
	}
	return changed
}

// audioCodec is how mp2ts-loop reads the frames of an audio stream.
type audioCodec uint8

const (
	// audioUnknown is cut only at PES boundaries.
	audioUnknown audioCodec = iota
	audioMPEG
	audioADTS
	audioAC3 // AC-3 and E-AC-3
)

func (c audioCodec) String() string {
	switch c {
	case audioMPEG:
		return "mpeg-audio"
	case audioADTS:
		return "aac-adts"
	case audioAC3:
		return "ac3"
	}
	return "unknown"
}

// audioFrameHeader is what the header of one coded audio frame gives.
type audioFrameHeader struct {
	size       int
	samples    int
	sampleRate int
	// sameTimeAsPrevious: an E-AC-3 dependent or additional independent
	// substream frame, which plays at the same time as the frame before it.
	sameTimeAsPrevious bool
}

// parseAudioFrameHeader returns the header of the frame starting at data, and
// false when there is no valid header there.
func parseAudioFrameHeader(codec audioCodec, data []byte) (audioFrameHeader, bool) {
	switch codec {
	case audioMPEG:
		return parseMPEGAudioHeader(data)
	case audioADTS:
		return parseADTSHeader(data)
	case audioAC3:
		return parseAC3Header(data)
	}
	return audioFrameHeader{}, false
}

var adtsSampleRates = [13]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

func parseADTSHeader(d []byte) (audioFrameHeader, bool) {
	const headerSize = 7
	if len(d) < headerSize || d[0] != 0xff || d[1]&0xf6 != 0xf0 {
		return audioFrameHeader{}, false
	}
	rateIndex := int(d[2] >> 2 & 0x0f)
	frameLength := int(d[3]&0x03)<<11 | int(d[4])<<3 | int(d[5]>>5)
	if rateIndex >= len(adtsSampleRates) || frameLength < headerSize {
		return audioFrameHeader{}, false
	}
	blocks := int(d[6]&0x03) + 1
	return audioFrameHeader{size: frameLength, samples: 1024 * blocks, sampleRate: adtsSampleRates[rateIndex]}, true
}

// mpegAudioBitrates is in kbit/s by [table][bitrate_index]: MPEG-1 layer I, II,
// III, then MPEG-2/2.5 layer I, then MPEG-2/2.5 layers II and III.
var mpegAudioBitrates = [5][15]int{
	{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
}

func parseMPEGAudioHeader(d []byte) (audioFrameHeader, bool) {
	if len(d) < 4 || d[0] != 0xff || d[1]&0xe0 != 0xe0 {
		return audioFrameHeader{}, false
	}
	version := d[1] >> 3 & 0x3   // 3 MPEG-1, 2 MPEG-2, 0 MPEG-2.5
	layerBits := d[1] >> 1 & 0x3 // 3 layer I, 2 layer II, 1 layer III
	bitrateIndex := int(d[2] >> 4)
	rateIndex := int(d[2] >> 2 & 0x3)
	padding := int(d[2] >> 1 & 0x1)
	if version == 1 || layerBits == 0 || bitrateIndex == 0 || bitrateIndex == 15 || rateIndex == 3 {
		return audioFrameHeader{}, false
	}
	mpeg1 := version == 3
	layer := 4 - int(layerBits)
	table := 4
	switch {
	case mpeg1:
		table = layer - 1
	case layer == 1:
		table = 3
	}
	bitrate := mpegAudioBitrates[table][bitrateIndex] * 1000
	rate := [3]int{44100, 48000, 32000}[rateIndex]
	switch version {
	case 2:
		rate >>= 1
	case 0:
		rate >>= 2
	}
	h := audioFrameHeader{sampleRate: rate}
	switch {
	case layer == 1:
		h.size, h.samples = (12*bitrate/rate+padding)*4, 384
	case layer == 2 || mpeg1:
		h.size, h.samples = 144*bitrate/rate+padding, 1152
	default:
		h.size, h.samples = 72*bitrate/rate+padding, 576
	}
	if h.size < 4 {
		return audioFrameHeader{}, false
	}
	return h, true
}

// ac3FrameWords is the AC-3 frame size in 16-bit words, ATSC A/52 Table 5.18.
func ac3FrameWords(frameSizeCode, rateCode int) int {
	kbps := [19]int{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 448, 512, 576, 640}[frameSizeCode/2]
	switch rateCode {
	case 0: // 48 kHz
		return kbps * 2
	case 1: // 44.1 kHz: 1536 samples hold a fractional number of words, padded by the odd code
		return kbps*1000*1536/44100/16 + frameSizeCode&1
	}
	return kbps * 3 // 32 kHz
}

func parseAC3Header(d []byte) (audioFrameHeader, bool) {
	rates := [3]int{48000, 44100, 32000}
	if len(d) < 6 || d[0] != 0x0b || d[1] != 0x77 {
		return audioFrameHeader{}, false
	}
	bsid := d[5] >> 3
	rateCode := int(d[4] >> 6)
	if bsid <= 10 {
		frameSizeCode := int(d[4] & 0x3f)
		if rateCode == 3 || frameSizeCode >= 38 {
			return audioFrameHeader{}, false
		}
		return audioFrameHeader{size: ac3FrameWords(frameSizeCode, rateCode) * 2, samples: 1536, sampleRate: rates[rateCode]}, true
	}
	if bsid > 16 {
		return audioFrameHeader{}, false
	}
	// E-AC-3, ATSC A/52 Annex E
	streamType := d[2] >> 6
	substreamID := d[2] >> 3 & 0x7
	frameSize := (int(d[2]&0x7)<<8 | int(d[3])) + 1
	h := audioFrameHeader{size: frameSize * 2, sameTimeAsPrevious: streamType == 1 || substreamID != 0}
	if rateCode == 3 {
		reduced := int(d[4] >> 4 & 0x3)
		if reduced == 3 {
			return audioFrameHeader{}, false
		}
		h.sampleRate, h.samples = [3]int{24000, 22050, 16000}[reduced], 256*6
	} else {
		h.sampleRate, h.samples = rates[rateCode], 256*[4]int{1, 2, 3, 6}[d[4]>>4&0x3]
	}
	return h, true
}
