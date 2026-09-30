package internal

import "bytes"

// MPEG-2 video (ISO/IEC 13818-2, H.262) start code values: the byte following
// the 00 00 01 start-code prefix.
const (
	mpeg2PictureStart   = 0x00
	mpeg2SliceMin       = 0x01
	mpeg2SliceMax       = 0xaf
	mpeg2SequenceHeader = 0xb3
	mpeg2Extension      = 0xb5
	mpeg2GroupStart     = 0xb8 // group_of_pictures header
)

// MPEG-2 picture_coding_type values.
const (
	mpeg2PicI = 1
	mpeg2PicB = 3
)

const (
	mpeg2PictureCodingExtID = 8    // extension_start_code_identifier of the picture coding extension
	mpeg2ClosedGOPBit       = 0x40 // closed_gop in byte 3 of the GOP header
	mpeg2BrokenLinkBit      = 0x20 // broken_link in byte 3 of the GOP header
)

var mpeg2PictureStartCode = []byte{0, 0, 1, mpeg2PictureStart}

// scanMPEG2AU scans an MPEG-2 video PES payload and classifies its first picture.
//
// An MPEG-2 random-access point is an I picture preceded by a sequence header.
// It maps onto the H.264/H.265 classes as follows: IsIDR when its GOP header has
// closed_gop set, so the B-pictures that follow it predict only from it; IsCRA
// otherwise (open GOP), since those B-pictures then predict from the previous
// GOP. The scan later promotes an open-GOP I picture to IsIDR when no B-picture
// follows it. HasSPS reports the sequence header.
func scanMPEG2AU(data []byte) AUInfo {
	var au AUInfo
	closedGOP := false
	for i := 0; i+3 < len(data); i++ {
		if data[i] != 0 || data[i+1] != 0 || data[i+2] != 1 {
			continue
		}
		code, body := data[i+3], data[i+4:]
		switch {
		case code == mpeg2SequenceHeader:
			au.HasSPS = true
		case code == mpeg2GroupStart && len(body) >= 4:
			closedGOP = body[3]&mpeg2ClosedGOPBit != 0
		case code == mpeg2PictureStart && len(body) >= 2:
			// temporal_reference (10 bits), then picture_coding_type (3 bits).
			au.PicType = (body[1] >> 3) & 0x07
			au.IsVCL = true
			au.Field = FieldFrame // MPEG-1, or MPEG-2 until the coding extension says otherwise
		case code == mpeg2Extension && au.IsVCL && len(body) >= 3 && body[0]>>4 == mpeg2PictureCodingExtID:
			switch body[2] & 0x03 { // picture_structure
			case 1:
				au.Field = FieldTop
			case 2:
				au.Field = FieldBottom
			}
		case code >= mpeg2SliceMin && code <= mpeg2SliceMax:
			if au.IsVCL {
				au.finishMPEG2(data[i:], closedGOP)
				return au
			}
		}
	}
	if au.IsVCL {
		au.finishMPEG2(nil, closedGOP)
	}
	return au
}

// finishMPEG2 sets the random-access classes once the first picture's headers
// are parsed. rest is the payload from the picture's first slice: a field
// picture that shares its PES with the other field of the frame makes the
// access unit a whole frame.
func (au *AUInfo) finishMPEG2(rest []byte, closedGOP bool) {
	if au.Field != FieldFrame && bytes.Contains(rest, mpeg2PictureStartCode) {
		au.Field = FieldFrame
	}
	if au.PicType == mpeg2PicI && au.HasSPS {
		au.IsRAP = true
		au.IsIDR = closedGOP
		au.IsCRA = !closedGOP
	}
}
