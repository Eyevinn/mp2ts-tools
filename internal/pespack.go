package internal

// PES re-packetization helpers used to split a multi-frame audio PES into one
// PES per frame, so the loop boundary can be cut on a frame (not PES) boundary.

// putPTS writes a 33-bit PTS into the 5 bytes b using the PTS-only ('0010')
// marker encoding (ISO/IEC 13818-1 2.4.3.7).
func putPTS(b []byte, pts int64) {
	b[0] = 0x21 | byte((pts>>29)&0x0e) // '0010' + PTS[32:30] + marker
	b[1] = byte(pts >> 22)             // PTS[29:22]
	b[2] = byte((pts>>14)&0xfe) | 0x01 // PTS[21:15] + marker
	b[3] = byte(pts >> 7)              // PTS[14:7]
	b[4] = byte((pts<<1)&0xfe) | 0x01  // PTS[6:0] + marker
}

// buildAudioPES builds a complete PES (header + payload) for a single audio
// frame, with a PTS-only optional header. streamID is cloned from the source PES.
func buildAudioPES(streamID byte, pts int64, frame []byte) []byte {
	// PES header = 6 fixed bytes + 3 optional-header bytes (flags + header length)
	// + 5 PTS bytes = 14 bytes. PES_packet_length counts everything after byte 5:
	// the 3 optional-header bytes + 5 PTS bytes + the payload.
	pesPacketLength := 3 + 5 + len(frame)
	pes := make([]byte, 14+len(frame))
	pes[0], pes[1], pes[2] = 0x00, 0x00, 0x01
	pes[3] = streamID
	pes[4] = byte(pesPacketLength >> 8)
	pes[5] = byte(pesPacketLength)
	pes[6] = 0x80 // '10' marker, no scrambling/priority
	pes[7] = 0x80 // PTS present, no DTS
	pes[8] = 0x05 // PES_header_data_length
	putPTS(pes[9:14], pts)
	copy(pes[14:], frame)
	return pes
}

// packetizePES splits PES bytes into 188-byte TS packets for pid. The first
// packet has PUSI set; the final packet is padded with adaptation-field stuffing
// so the PES ends on a packet boundary. Continuity counters are left at 0 (the
// emitter stamps them).
func packetizePES(pid int, pes []byte) []byte {
	var out []byte
	pos := 0
	first := true
	for pos < len(pes) {
		var pkt [PacketSize]byte
		pkt[0] = 0x47
		pkt[1] = byte(pid>>8) & 0x1f
		if first {
			pkt[1] |= 0x40 // PUSI
		}
		pkt[2] = byte(pid)
		remaining := len(pes) - pos
		if remaining >= PacketSize-4 {
			pkt[3] = 0x10 // payload only
			copy(pkt[4:], pes[pos:pos+(PacketSize-4)])
			pos += PacketSize - 4
		} else {
			// Last packet: adaptation-field stuffing then payload at the tail.
			pkt[3] = 0x30 // adaptation field + payload
			adLen := PacketSize - 5 - remaining
			pkt[4] = byte(adLen)
			if adLen >= 1 {
				pkt[5] = 0x00 // AF flags (none)
				for i := 6; i < 5+adLen; i++ {
					pkt[i] = 0xff
				}
			}
			copy(pkt[PacketSize-remaining:], pes[pos:])
			pos = len(pes)
		}
		out = append(out, pkt[:]...)
		first = false
	}
	return out
}

// repackageFramesAsPES builds one PES (and its TS packets) per frame. frames are
// byte slices into a PES payload; ptss are the matching frame PTS values.
func repackageFramesAsPES(pid int, streamID byte, frames [][]byte, ptss []int64) []byte {
	var out []byte
	for i, fr := range frames {
		out = append(out, packetizePES(pid, buildAudioPES(streamID, ptss[i], fr))...)
	}
	return out
}

// extractPESPayload concatenates the TS-packet payloads of a buffered PES (its
// run of 188-byte packets), yielding the PES bytes (header + elementary data).
func extractPESPayload(raw []byte) []byte {
	var out []byte
	for i := 0; i+PacketSize <= len(raw); i += PacketSize {
		p := raw[i : i+PacketSize]
		afc := (p[3] >> 4) & 3
		if afc == 2 {
			continue // adaptation field only, no payload
		}
		off := 4
		if afc == 3 {
			off = 5 + int(p[4])
		}
		if off < PacketSize {
			out = append(out, p[off:]...)
		}
	}
	return out
}
