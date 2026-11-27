package internal

// Byte-level reading and patching of 188-byte TS packets, as mp2ts-loop does
// it: in place, changing only the fields that must change.

const (
	nullPID   = 0x1fff
	numPIDs   = 8192
	syncByte  = 0x47
	tsHeader  = 4
	maxAFSize = PacketSize - tsHeader - 1 // the longest adaptation field after its length byte

	afFlagDiscontinuity = 0x80
	afFlagRandomAccess  = 0x40
	afFlagPCR           = 0x10
	afFlagOPCR          = 0x08
	pcrFieldOffset      = 6
	pcrFieldSize        = 6

	// pcrPerTick is the number of 27 MHz PCR ticks in a 90 kHz timestamp tick.
	pcrPerTick  = 300
	stuffByte   = 0xff
	maxPayload  = PacketSize - tsHeader
	pusiBit     = 0x40
	ptsWrapU    = uint64(PtsWrap)
	pcrWrapU    = uint64(PcrWrap)
	pcrPerSec   = 27_000_000
	ticksPerSec = 90_000
)

// tsPID reads the PID from a raw 188-byte TS packet.
func tsPID(b []byte) int { return int(b[1]&0x1f)<<8 | int(b[2]) }

// tsPUSI reports the payload_unit_start_indicator of a raw TS packet.
func tsPUSI(b []byte) bool { return b[1]&pusiBit != 0 }

func tsHasAF(b []byte) bool        { return b[3]&0x20 != 0 }
func tsHasPayload(b []byte) bool   { return b[3]&0x10 != 0 }
func tsScrambled(b []byte) bool    { return b[3]&0xc0 != 0 }
func tsTransportErr(b []byte) bool { return b[1]&0x80 != 0 }
func tsCC(b []byte) byte           { return b[3] & 0x0f }

// tsOversizedAF reports an adaptation field that claims more bytes than the
// packet holds after its length byte: the packet is corrupt.
func tsOversizedAF(b []byte) bool { return tsHasAF(b) && int(b[4]) > maxAFSize }

// tsPayloadOffset returns the index of the first payload byte in a raw TS packet,
// or PacketSize if the packet carries no payload.
func tsPayloadOffset(b []byte) int {
	if !tsHasPayload(b) {
		return PacketSize
	}
	if !tsHasAF(b) {
		return tsHeader
	}
	return min(tsHeader+1+int(b[4]), PacketSize)
}

// tsAFFlags returns the adaptation field flags byte, or 0 without one.
func tsAFFlags(b []byte) byte {
	if tsHasAF(b) && b[4] > 0 {
		return b[5]
	}
	return 0
}

// tsCarriesPCR reports a PCR in the adaptation field.
func tsCarriesPCR(b []byte) bool {
	return tsAFFlags(b)&afFlagPCR != 0 && int(b[4]) >= 1+pcrFieldSize
}

// tsOPCROffset returns where the OPCR field lies in the packet, if it has one.
func tsOPCROffset(b []byte) (int, bool) {
	flags := tsAFFlags(b)
	if flags&afFlagOPCR == 0 {
		return 0, false
	}
	off := pcrFieldOffset
	if flags&afFlagPCR != 0 {
		off += pcrFieldSize
	}
	// The adaptation field occupies bytes [5, 5 + length).
	if off+pcrFieldSize > 5+int(b[4]) {
		return 0, false
	}
	return off, true
}

// readPCR returns the 27 MHz value of the 6-byte PCR/OPCR field f.
func readPCR(f []byte) uint64 {
	base := uint64(f[0])<<25 | uint64(f[1])<<17 | uint64(f[2])<<9 | uint64(f[3])<<1 | uint64(f[4])>>7
	ext := uint64(f[4]&0x1)<<8 | uint64(f[5])
	return base*pcrPerTick + ext
}

// writePCR writes a 27 MHz value into the 6-byte PCR/OPCR field f.
func writePCR(f []byte, pcr uint64) {
	base := pcr / pcrPerTick % ptsWrapU
	ext := pcr % pcrPerTick
	f[0] = byte(base >> 25)
	f[1] = byte(base >> 17)
	f[2] = byte(base >> 9)
	f[3] = byte(base >> 1)
	f[4] = byte((base&0x1)<<7 | 0x7e | ext>>8&0x1)
	f[5] = byte(ext)
}

// readTimestamp returns the 33-bit value of a 5-byte PTS/DTS field.
func readTimestamp(f []byte) uint64 {
	return uint64(f[0]>>1&0x7)<<30 | uint64(f[1])<<22 | uint64(f[2]>>1)<<15 | uint64(f[3])<<7 | uint64(f[4])>>1
}

// writeTimestamp writes the 33-bit value into a 5-byte PTS/DTS field and keeps
// its 4-bit prefix, so a PTS stays a PTS and a DTS stays a DTS.
func writeTimestamp(f []byte, v uint64) {
	v %= ptsWrapU
	f[0] = f[0]&0xf0 | byte(v>>29&0x0e) | 0x01
	f[1] = byte(v >> 22)
	f[2] = byte(v>>14&0xfe) | 0x01
	f[3] = byte(v >> 7)
	f[4] = byte(v<<1&0xfe) | 0x01
}

// pesHeaderAt is where the PES header of a PES-starting packet lies, as offsets
// into the packet. pts and dts are -1 when absent.
type pesHeaderAt struct {
	start        int // the 00 00 01 prefix
	streamID     byte
	packetLength int // PES_packet_length; 0 means unbounded
	size         int // header bytes from the prefix to the first payload byte
	pts, dts     int
}

// pesHasOptionalHeader reports whether PES of the stream_id carry the optional
// header, and so may carry PTS/DTS (ITU-T H.222.0, 2.4.3.7).
func pesHasOptionalHeader(streamID byte) bool {
	switch streamID {
	case 0xbc, // program_stream_map
		0xbe, // padding_stream
		0xbf, // private_stream_2
		0xf0, // ECM
		0xf1, // EMM
		0xf2, // DSMCC_stream
		0xf8, // ITU-T H.222.1 type E
		0xff: // program_stream_directory
		return false
	}
	return true
}

// findPESHeader returns the PES header of a packet that starts a PES. It fails
// when the packet does not start one, is scrambled, or the header does not fit.
func findPESHeader(b []byte) (pesHeaderAt, bool) {
	if !tsPUSI(b) || tsScrambled(b) {
		return pesHeaderAt{}, false
	}
	start := tsPayloadOffset(b)
	if start+6 > PacketSize {
		return pesHeaderAt{}, false
	}
	p := b[start:]
	if p[0] != 0 || p[1] != 0 || p[2] != 1 || p[3] < 0xbc {
		return pesHeaderAt{}, false
	}
	h := pesHeaderAt{start: start, streamID: p[3], packetLength: int(p[4])<<8 | int(p[5]), size: 6, pts: -1, dts: -1}
	if !pesHasOptionalHeader(h.streamID) {
		return h, true
	}
	if start+9 > PacketSize || p[6]&0xc0 != 0x80 {
		return pesHeaderAt{}, false
	}
	h.size = 9 + int(p[8])
	if start+h.size > PacketSize {
		return pesHeaderAt{}, false
	}
	flags := p[7] >> 6 & 0x3
	if flags&0x2 != 0 && h.size >= 14 {
		h.pts = start + 9
	}
	if flags == 0x3 && h.size >= 19 {
		h.dts = start + 14
	}
	return h, true
}

// writeNullPacket turns b into a null packet.
func writeNullPacket(b []byte) {
	b[0], b[1], b[2], b[3] = syncByte, nullPID>>8, nullPID&0xff, 0x10
	for i := tsHeader; i < PacketSize; i++ {
		b[i] = stuffByte
	}
}

// writePCRPacket turns b into an adaptation-field-only packet on pid that
// carries a PCR, to be stamped when it is sent.
func writePCRPacket(b []byte, pid int, cc byte) {
	b[0], b[1], b[2], b[3] = syncByte, byte(pid>>8&0x1f), byte(pid), 0x20|cc&0x0f
	b[4], b[5] = maxAFSize, afFlagPCR
	for i := pcrFieldOffset; i < pcrFieldOffset+pcrFieldSize; i++ {
		b[i] = 0
	}
	for i := pcrFieldOffset + pcrFieldSize; i < PacketSize; i++ {
		b[i] = stuffByte
	}
}

// shrinkPayload keeps the first keep payload bytes and fills the space before
// them with adaptation-field stuffing, so the packet keeps its size. Keeping no
// bytes turns a packet that carries a PCR into an adaptation-field-only packet,
// and any other packet into a null packet.
func shrinkPayload(b []byte, keep int) {
	off := tsPayloadOffset(b)
	if keep >= PacketSize-off {
		return
	}
	if keep == 0 && !tsCarriesPCR(b) {
		writeNullPacket(b)
		return
	}
	var kept [PacketSize]byte
	copy(kept[:keep], b[off:off+keep])
	// keep < current payload <= maxAFSize here, so the new adaptation field is
	// at least as long as the old one.
	oldLen := 0
	if tsHasAF(b) {
		oldLen = int(b[4])
	}
	newLen := maxAFSize - keep
	b[3] |= 0x20
	if keep == 0 {
		b[3] &^= 0x10
	}
	b[4] = byte(newLen)
	fillFrom := 5 + oldLen
	if oldLen == 0 && newLen > 0 {
		b[5] = 0
		fillFrom = 6
	}
	fillTo := 5 + newLen
	for i := fillFrom; i < fillTo; i++ {
		b[i] = stuffByte
	}
	copy(b[fillTo:], kept[:keep])
}

// stripPayload removes the payload: shrinkPayload(b, 0).
func stripPayload(b []byte) { shrinkPayload(b, 0) }

// removeClockReferences drops PCR and OPCR from the adaptation field, for a
// packet sent at another time than in its source. The field keeps its length,
// with more stuffing.
func removeClockReferences(b []byte) {
	if !tsHasAF(b) || b[4] == 0 {
		return
	}
	flags := b[5]
	removed := 0
	if flags&afFlagPCR != 0 {
		removed += pcrFieldSize
	}
	if flags&afFlagOPCR != 0 {
		removed += pcrFieldSize
	}
	const fields = 6
	end := min(5+int(b[4]), PacketSize)
	if removed == 0 || fields+removed > end {
		return
	}
	copy(b[fields:], b[fields+removed:end])
	for i := end - removed; i < end; i++ {
		b[i] = stuffByte
	}
	b[5] = flags &^ (afFlagPCR | afFlagOPCR)
}

// clearDiscontinuity clears the discontinuity_indicator of a packet.
func clearDiscontinuity(b []byte) {
	if tsAFFlags(b)&afFlagDiscontinuity != 0 {
		b[5] &^= afFlagDiscontinuity
	}
}

// unwrapper follows a wrapping counter (33-bit timestamps or 27 MHz PCR) as a
// monotonic signed value, assuming consecutive values are less than half the
// range apart.
type unwrapper struct {
	wrap      uint64
	started   bool
	last      uint64
	unwrapped int64
}

func (u *unwrapper) unwrap(v uint64) int64 {
	if !u.started {
		u.started, u.last, u.unwrapped = true, v, int64(v)
		return u.unwrapped
	}
	forward := (v + u.wrap - u.last) % u.wrap
	step := int64(forward)
	if forward >= u.wrap/2 {
		step -= int64(u.wrap)
	}
	u.unwrapped += step
	u.last = v
	return u.unwrapped
}

// wrapAdd returns v + off reduced into [0, wrap).
func wrapAdd(v uint64, off int64, wrap uint64) uint64 {
	w := int64(wrap)
	return (v + uint64(off%w+w)) % wrap
}
