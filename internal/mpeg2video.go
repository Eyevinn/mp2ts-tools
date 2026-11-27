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
	mpeg2SequenceExtID      = 1    // extension_start_code_identifier of the sequence extension
	mpeg2PictureCodingExtID = 8    // extension_start_code_identifier of the picture coding extension
	mpeg2ClosedGOPBit       = 0x40 // closed_gop in byte 3 of the GOP header
	mpeg2BrokenLinkBit      = 0x20 // broken_link in byte 3 of the GOP header
)

var mpeg2PictureStartCode = []byte{0, 0, 1, mpeg2PictureStart}

// mpeg2Picture describes the first picture of an MPEG-2 access unit.
type mpeg2Picture struct {
	hasPicture  bool        // a picture header was found
	seqHeader   bool        // a sequence header comes before it
	picType     byte        // picture_coding_type
	temporalRef uint16      // temporal_reference, shared by the two fields of a frame
	field       FieldParity // a frame (or field pair), or a lone field picture
	// structure is the first picture's own: FieldTop or FieldBottom for a field
	// picture, also when its other field shares the access unit.
	structure FieldParity
	// extension: a picture coding extension follows the picture header (MPEG-2,
	// not MPEG-1). topFirst and repeatFirst are its top_field_first and
	// repeat_first_field.
	extension   bool
	topFirst    bool
	repeatFirst bool
	// progressiveSequence is the sequence extension's progressive_sequence, when
	// a sequence extension comes before the picture.
	progressiveSequence *bool
	closedGOP           bool // an I picture after a sequence header whose GOP is closed
	openGOP             bool // an I picture after a sequence header whose GOP is open
}

// scanMPEG2AU scans an MPEG-2 video PES payload and classifies its first picture.
//
// An MPEG-2 random-access point is an I picture preceded by a sequence header.
// Its GOP header's closed_gop says whether the B pictures that follow it predict
// only from it, or from the previous GOP too (an open GOP). The scan later
// promotes an open-GOP I picture that no B picture follows.
func scanMPEG2AU(data []byte) mpeg2Picture {
	var pic mpeg2Picture
	closedGOP := false
	for i := 0; i+3 < len(data); i++ {
		if data[i] != 0 || data[i+1] != 0 || data[i+2] != 1 {
			continue
		}
		code, body := data[i+3], data[i+4:]
		switch {
		case code == mpeg2SequenceHeader && !pic.hasPicture:
			pic.seqHeader = true
		case code == mpeg2GroupStart && len(body) >= 4 && !pic.hasPicture:
			closedGOP = body[3]&mpeg2ClosedGOPBit != 0
		case code == mpeg2Extension && !pic.hasPicture && len(body) >= 2 && body[0]>>4 == mpeg2SequenceExtID:
			progressive := body[1]&0x08 != 0
			pic.progressiveSequence = &progressive
		case code == mpeg2PictureStart && len(body) >= 2 && !pic.hasPicture:
			// temporal_reference (10 bits), then picture_coding_type (3 bits).
			pic.temporalRef = uint16(body[0])<<2 | uint16(body[1]>>6)
			pic.picType = (body[1] >> 3) & 0x07
			pic.hasPicture = true
			pic.field = FieldFrame // MPEG-1, or MPEG-2 until the coding extension says otherwise
			pic.structure = FieldFrame
		case code == mpeg2Extension && pic.hasPicture && !pic.extension && len(body) >= 4 &&
			body[0]>>4 == mpeg2PictureCodingExtID:
			pic.extension = true
			switch body[2] & 0x03 { // picture_structure
			case 1:
				pic.field = FieldTop
			case 2:
				pic.field = FieldBottom
			}
			pic.structure = pic.field
			pic.topFirst = body[3]&0x80 != 0
			pic.repeatFirst = body[3]&0x02 != 0
		case code >= mpeg2SliceMin && code <= mpeg2SliceMax:
			if pic.hasPicture {
				pic.finish(data[i:], closedGOP)
				return pic
			}
		}
	}
	if pic.hasPicture {
		pic.finish(nil, closedGOP)
	}
	return pic
}

// finish sets the random-access classes once the first picture's headers are
// parsed. rest is the payload from the picture's first slice: a field picture
// that shares its PES with the other field of the frame makes the access unit
// a whole frame.
func (pic *mpeg2Picture) finish(rest []byte, closedGOP bool) {
	if pic.field != FieldFrame && bytes.Contains(rest, mpeg2PictureStartCode) {
		pic.field = FieldFrame
	}
	if pic.picType == mpeg2PicI && pic.seqHeader {
		pic.closedGOP = closedGOP
		pic.openGOP = !closedGOP
	}
}
