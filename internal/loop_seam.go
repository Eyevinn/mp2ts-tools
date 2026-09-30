package internal

import "github.com/Eyevinn/mp4ff/hevc"

// tsPID reads the PID from a raw 188-byte TS packet.
func tsPID(b []byte) int { return int(b[1]&0x1f)<<8 | int(b[2]) }

// tsPUSI reports the payload_unit_start_indicator of a raw TS packet.
func tsPUSI(b []byte) bool { return b[1]&0x40 != 0 }

// tsPayloadOffset returns the index of the first payload byte in a raw TS packet,
// or PacketSize if the packet carries no payload.
func tsPayloadOffset(b []byte) int {
	switch (b[3] >> 4) & 0x3 {
	case 1: // payload only
		return 4
	case 3: // adaptation field + payload
		return 5 + int(b[4])
	default: // 0 reserved, 2 adaptation only -> no payload
		return PacketSize
	}
}

// firstAUBytes reassembles the payload of the segment's first video access unit
// (from the first video PUSI up to the next one) and returns it along with the
// position in data of each byte, so a header field can be patched in place.
func firstAUBytes(data []byte, n, videoPID int) (buf []byte, pos []int) {
	start := -1
	for i := 0; i < n; i++ {
		b := data[i*PacketSize : i*PacketSize+PacketSize]
		if tsPID(b) == videoPID && tsPUSI(b) {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, nil
	}
	for i := start; i < n; i++ {
		b := data[i*PacketSize : i*PacketSize+PacketSize]
		if tsPID(b) != videoPID {
			continue
		}
		if i != start && tsPUSI(b) {
			break
		}
		for o := tsPayloadOffset(b); o < PacketSize; o++ {
			buf = append(buf, b[o])
			pos = append(pos, i*PacketSize+o)
		}
	}
	return buf, pos
}

// rewriteSeamCRAtoBLA rewrites every CRA slice NAL unit in the segment's first
// video access unit (the loop point) to BLA_W_LP, and returns how many it changed.
//
// Every loop seam splices the segment's tail directly before its head, so the
// loop-point CRA appears mid-stream. A mid-stream CRA does not set
// NoRaslOutputFlag, so a decoder tries to decode the picture's leading RASL units
// and looks for their pre-CRA references — which after the splice are the previous
// wrap's tail with unrelated POCs, corrupting the whole first GOP. Marking the
// picture BLA forces NoRaslOutputFlag=1: the decoder discards the RASL and resets
// POC, making the seam clean. Baking this into the segment is also correct for
// wrap 0, where a BLA at stream start behaves exactly like the CRA did.
func rewriteSeamCRAtoBLA(data []byte, n, videoPID int) int {
	buf, pos := firstAUBytes(data, n, videoPID)
	flips := 0
	for k := 0; k+3 < len(buf); k++ {
		if buf[k] != 0 || buf[k+1] != 0 || buf[k+2] != 1 {
			continue
		}
		h := buf[k+3]
		if (h>>1)&0x3f != byte(hevc.NALU_CRA) {
			continue
		}
		// Keep the forbidden_zero_bit (bit 7) and the layer-id MSB (bit 0); only
		// replace the 6-bit nal_unit_type field.
		data[pos[k+3]] = (h & 0x81) | (byte(hevc.NALU_BLA_W_LP) << 1)
		flips++
	}
	return flips
}

// markSeamBrokenLink sets broken_link in the GOP header of the segment's first
// video access unit (the loop point) if that GOP is open, and reports whether it
// did. It is the MPEG-2 counterpart of rewriteSeamCRAtoBLA.
//
// In an open GOP (closed_gop=0), the B-pictures that follow the I picture in
// coding order predict from the previous GOP's last anchor picture. At a loop
// seam that picture is the previous wrap's tail, not the one they were encoded
// against. They still decode without errors, since the reference exists, but the
// result is wrong. broken_link=1 is how ISO/IEC 13818-2 marks such an edit, so a
// decoder may drop or conceal those B-pictures. Like the BLA rewrite, it is also
// correct for wrap 0.
func markSeamBrokenLink(data []byte, n, videoPID int) bool {
	buf, pos := firstAUBytes(data, n, videoPID)
	for k := 0; k+7 < len(buf); k++ {
		if buf[k] != 0 || buf[k+1] != 0 || buf[k+2] != 1 {
			continue
		}
		switch buf[k+3] {
		case mpeg2GroupStart:
			if buf[k+7]&mpeg2ClosedGOPBit != 0 {
				return false // closed GOP: the B-pictures predict only from the I picture
			}
			data[pos[k+7]] |= mpeg2BrokenLinkBit
			return true
		case mpeg2PictureStart:
			return false // no GOP header before the first picture
		}
	}
	return false
}
