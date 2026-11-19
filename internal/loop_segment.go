package internal

import (
	"fmt"
	"math"
	"sort"
)

const (
	// maxPCRSpacing is DVB's limit between PCRs (ETSI TR 101 290, 5.2.2), and
	// pcrFillSpacing the spacing of the PCRs that fill a gap, short of it so
	// that audio packets the writer inserts between them do not push them past it.
	maxPCRSpacing  = 40 * 27_000
	pcrFillSpacing = 35 * 27_000
	pesLengthAt    = 4 // PES_packet_length, from the PES start code
	pesFixedHeader = 6 // start code, stream_id, PES_packet_length
)

// segmentPES is an audio PES of the loop, ready to place in a lap.
type segmentPES struct {
	packets    []byte // its packets with payload, without PCR
	targets    []int  // per packet: the lap packet it is due at
	packetEnds []int  // PES byte just past each packet's payload
	frameEnds  []int  // PES byte just past each of its loop frames
	firstFrame int    // into segmentAudio.frameStarts
	// lapShift is the lap it is sent in, relative to the lap whose audio it is:
	// +1 when the source sends it after the window, -1 before.
	lapShift int
}

func (p *segmentPES) packetCount() int { return len(p.packets) / PacketSize }

// segmentAudio is the audio a lap plays: the frames from the one nearest the
// loop's first picture on, so the audio seam is where the video's is.
type segmentAudio struct {
	pid       int
	timeScale int64
	// frameStarts is each frame's start relative to the first frame's, then
	// where the last ends.
	frameStarts []int64
	pes         []segmentPES
}

// loopSegment is one lap: the source packets of the loop window with partial
// payload units removed at both ends, null packets added or removed at the end
// to fit a constant rate, and the loop's first picture marked as a seam. Audio
// packets are taken out: each lap places its audio in the free packets (nulls
// and the window's audio packets) at or after where the source sends it.
type loopSegment struct {
	packets      []byte
	sendTimes    []int64 // 27 MHz from the start of the lap, per packet
	preamble     []byte  // the PAT and PMT, sent once before the first lap
	duration     int64   // 90 kHz
	firstPCR     uint64  // 27 MHz: the PCR of the lap's start in the first lap
	pcrPID       int
	constantRate bool // each lap is lapPackets long, sent at an even pace
	lapPackets   int
	scte35       []*scte35Patcher
	scte35PIDs   []int
	audio        []segmentAudio

	stripped        int // payload removed: partial units at the window's ends
	removedNulls    int
	addedNulls      int
	addedPCRPackets int // only a PCR, added at the seam: mostly null packets turned into them
	seamMarks       int // CRA to BLA, broken_link or broken_link_flag edits
	audioHeadCuts   int // audio PES that lose the frames before the loop's first
	scte35Injected  int // packets of SCTE-35 cues sent before the window, moved to the lap's start
	warnings        []string
}

func (s *loopSegment) packetCount() int { return len(s.packets) / PacketSize }

func packetAt(b []byte, i int) []byte { return b[i*PacketSize : (i+1)*PacketSize] }

func roundedDiv(v, d int64) int64 {
	if v >= 0 {
		return (v + d/2) / d
	}
	return -((-v + d/2) / d)
}

// evenTime is when packet index of count, sent evenly over total, is due:
// index * total / count without overflowing 64 bits.
func evenTime(index, count int, total int64) int64 {
	i, n := int64(index), int64(count)
	return i*(total/n) + i*(total%n)/n
}

func writePESLength(b []byte, h pesHeaderAt, length int) {
	b[h.start+pesLengthAt] = byte(length >> 8)
	b[h.start+pesLengthAt+1] = byte(length)
}

// truncatePES ends a PES, given as consecutive packets, after its first keep
// bytes: the packet holding the cut keeps the bytes before it, later packets
// lose their payload, and PES_packet_length shrinks to match.
func truncatePES(packets []byte, packetEnds []int, keep int) {
	begin := 0
	for i, e := range packetEnds {
		p := packetAt(packets, i)
		switch {
		case begin >= keep:
			stripPayload(p)
		case e > keep:
			shrinkPayload(p, keep-begin)
		}
		begin = e
	}
	if keep <= pesFixedHeader || len(packetEnds) == 0 {
		return
	}
	first := packetAt(packets, 0)
	if h, ok := findPESHeader(first); ok && h.packetLength != 0 {
		if length := keep - pesFixedHeader; length < h.packetLength {
			writePESLength(first, h, length)
		}
	}
}

// cutPESHead drops the frames before the loop's first from the PES that holds
// it. The PES header moves up to the first kept byte, which turns the packet
// holding the cut into one or two packets, and takes the first kept frame's PTS.
func cutPESHead(p *segmentPES, cut int, pts uint64) bool {
	first := packetAt(p.packets, 0)
	h, ok := findPESHeader(first)
	if !ok || cut <= h.size {
		return false
	}
	holding := sort.SearchInts(p.packetEnds, cut+1) // the first packet ending past the cut
	if holding >= p.packetCount() {
		return false
	}
	removed := cut - h.size
	holdingBegin := 0
	if holding > 0 {
		holdingBegin = p.packetEnds[holding-1]
	}
	source := append([]byte(nil), packetAt(p.packets, holding)...)

	header := append([]byte(nil), first[h.start:h.start+h.size]...)
	for _, f := range []int{h.pts, h.dts} {
		if f >= 0 {
			writeTimestamp(header[f-h.start:], pts)
		}
	}
	if h.packetLength != 0 {
		length := h.packetLength - removed
		header[pesLengthAt], header[pesLengthAt+1] = byte(length>>8), byte(length)
	}
	bytes := append(header, source[tsPayloadOffset(source)+(cut-holdingBegin):]...)

	var replacement []byte
	var ends []int
	for off := 0; off < len(bytes); off += maxPayload {
		n := min(maxPayload, len(bytes)-off)
		var pkt [PacketSize]byte
		pkt[0] = source[0]
		if off == 0 {
			pkt[1] = source[1] | pusiBit
		} else {
			pkt[1] = source[1] &^ pusiBit
		}
		pkt[2] = source[2]
		pkt[3] = source[3]&0x0f | 0x10 // payload only, not scrambled
		copy(pkt[tsHeader:], bytes[off:off+n])
		shrinkPayload(pkt[:], n)
		replacement = append(replacement, pkt[:]...)
		ends = append(ends, off+n)
	}

	target := p.targets[holding]
	p.packets = append(replacement, p.packets[(holding+1)*PacketSize:]...)
	targets := make([]int, len(ends), len(ends)+len(p.targets)-holding-1)
	for i := range targets {
		targets[i] = target
	}
	p.targets = append(targets, p.targets[holding+1:]...)
	rest := p.packetEnds[holding+1:]
	for i := range rest {
		rest[i] -= removed
	}
	p.packetEnds = append(ends, rest...)
	for i := range p.frameEnds {
		p.frameEnds[i] -= removed
	}
	return true
}

// stripPartialUnits removes payload units cut by the window's edges: a PES or
// section that started before the window (its packets at the start are orphans
// a decoder would append to the previous lap), or that ends after it. Audio is
// placed by each lap instead.
func stripPartialUnits(window []byte, idx *streamIndex, start, end int) int {
	orphanEnd := make(map[int]int)
	tailStart := make(map[int]int)
	isAudio := make(map[int]bool)
	for _, a := range idx.audio {
		isAudio[a.pid] = true
	}
	for pid, units := range idx.units {
		if isAudio[pid] {
			continue
		}
		first := sort.Search(len(units), func(k int) bool { return units[k].first >= start })
		orphanEnd[pid] = end
		if first < len(units) && units[first].first < end {
			orphanEnd[pid] = units[first].first
		}
		after := first + sort.Search(len(units)-first, func(k int) bool { return units[first+k].first >= end })
		if after != first && units[after-1].last >= end {
			tailStart[pid] = units[after-1].first
		}
	}
	stripped := 0
	for slot := 0; slot < end-start; slot++ {
		p := packetAt(window, slot)
		if !tsHasPayload(p) {
			continue
		}
		pid := tsPID(p)
		if pid == nullPID {
			continue
		}
		source := start + slot
		oe, ok := orphanEnd[pid]
		if !ok {
			oe = start
		}
		ts, ok := tailStart[pid]
		if !ok {
			ts = end
		}
		if source < oe || source >= ts {
			stripPayload(p)
			stripped++
		}
	}
	return stripped
}

// prepareAudio collects the loop's audio of one stream: copies of its source
// PES without clock references, each packet due where the source sends it
// relative to the window, in the lap before, during or after the one it plays in.
func prepareAudio(data []byte, idx *streamIndex, plan *loopPlan, fit audioFit, lapPacketOfSlot []int, headCuts *int) segmentAudio {
	st := &idx.audio[fit.stream]
	audio := segmentAudio{pid: st.pid, timeScale: st.timeScale}
	framesEnd := fit.firstFrame + fit.frameCount
	first := st.frames[fit.firstFrame]
	for f := fit.firstFrame; f < framesEnd; f++ {
		audio.frameStarts = append(audio.frameStarts, st.frames[f].start-first.start)
	}
	last := st.frames[framesEnd-1]
	audio.frameStarts = append(audio.frameStarts, last.start+last.duration-first.start)

	window := plan.endPacket - plan.startPacket
	for pi := fit.firstPES; pi < fit.firstPES+fit.pesCount; pi++ {
		src := st.pes[pi]
		pesFirst := max(src.firstFrame, fit.firstFrame)
		pesEnd := min(src.firstFrame+src.frameCount, framesEnd)
		if pesFirst >= pesEnd {
			continue
		}
		p := segmentPES{firstFrame: pesFirst - fit.firstFrame}
		switch {
		case src.firstPacket >= plan.endPacket:
			p.lapShift = 1
		case src.firstPacket < plan.startPacket:
			p.lapShift = -1
		}
		for c := src.firstChunk; c < src.firstChunk+src.chunkCount; c++ {
			ch := st.chunks[c]
			p.packets = append(p.packets, packetAt(data, ch.packet)...)
			pkt := p.packets[len(p.packets)-PacketSize:]
			clearDiscontinuity(pkt)
			removeClockReferences(pkt)
			slot := ch.packet - plan.startPacket - p.lapShift*window
			p.targets = append(p.targets, lapPacketOfSlot[min(max(slot, 0), window)])
			p.packetEnds = append(p.packetEnds, ch.endOffset)
		}
		for f := pesFirst; f < pesEnd; f++ {
			p.frameEnds = append(p.frameEnds, st.frames[f].endOffset)
		}
		if src.firstFrame < fit.firstFrame {
			pts := roundedDiv(first.start, st.timeScale)
			if cutPESHead(&p, st.frames[fit.firstFrame-1].endOffset, wrapAdd(0, pts, ptsWrapU)) {
				*headCuts++
			}
		}
		audio.pes = append(audio.pes, p)
	}
	return audio
}

// markSeamPictures marks an open GOP or a recovery point at the loop's first
// picture as an edit: it follows the last picture of the previous lap. When the
// first picture is a field whose second field is an access point too, such as
// two H.265 CRA fields, the second is marked as well: its leading pictures may
// reference the previous lap just as the first's do.
func markSeamPictures(seg *loopSegment, idx *streamIndex, first int) int {
	pictures := 1
	if next := first + 1; next < len(idx.pictures) && idx.pictures[next].info.secondField &&
		idx.pictures[next].info.accessPoint != apNone {
		pictures = 2
	}
	changed := 0
	var buf []byte
	var pos []int
	mark := func() {
		if n := markSeam(idx.videoCodec, buf); n > 0 {
			changed += n
			for k, b := range buf {
				seg.packets[pos[k]] = b
			}
		}
		buf, pos = buf[:0], pos[:0]
	}
	started := 0
	for i := 0; i < seg.packetCount(); i++ {
		p := packetAt(seg.packets, i)
		if tsPID(p) != idx.videoPID || !tsHasPayload(p) {
			continue
		}
		off := tsPayloadOffset(p)
		if tsPUSI(p) {
			if started > 0 {
				mark()
			}
			if started == pictures {
				return changed
			}
			h, ok := findPESHeader(p)
			if !ok {
				return changed
			}
			started++
			off = h.start + h.size
		}
		if started == 0 {
			continue
		}
		for ; off < PacketSize; off++ {
			buf = append(buf, p[off])
			pos = append(pos, i*PacketSize+off)
		}
	}
	mark()
	return changed
}

// fillSeamPCRGap keeps the PCR within 40 ms (ETSI TR 101 290, 5.2.2) from the
// lap's last PCR to the next lap's first: null packets there become packets
// with only a PCR, and a variable-rate lap that ends early gets more such
// packets at its end. The null packets marked in audioSlots are left for the
// audio. It returns how many packets carry an added PCR, and warns about a gap
// it cannot fill.
func fillSeamPCRGap(seg *loopSegment, audioSlots []bool) int {
	lapTime := seg.duration * pcrPerTick
	times := seg.sendTimes
	isPCR := func(i int) bool {
		p := packetAt(seg.packets, i)
		return tsPID(p) == seg.pcrPID && tsCarriesPCR(p)
	}
	var pcrPackets []int
	for i := 0; i < seg.packetCount(); i++ {
		if isPCR(i) {
			pcrPackets = append(pcrPackets, i)
		}
	}
	if len(pcrPackets) == 0 {
		return 0
	}
	firstPCR, lastPCR := pcrPackets[0], pcrPackets[len(pcrPackets)-1]
	nextLapPCR := lapTime + times[firstPCR]
	if nextLapPCR-times[lastPCR] <= maxPCRSpacing {
		return 0
	}

	// The packets from the lap's last PCR to the next lap's first, with their
	// times from this lap's start.
	type seamPacket struct {
		index int
		time  int64
	}
	var seam []seamPacket
	for i := lastPCR + 1; i < seg.packetCount(); i++ {
		seam = append(seam, seamPacket{i, times[i]})
	}
	tailSize := len(seam)
	for i := 0; i < firstPCR; i++ {
		seam = append(seam, seamPacket{i, lapTime + times[i]})
	}

	// Before the PID's first packet the writer keeps a packet's own counter, so
	// an added packet there takes the one before that packet's.
	var cc byte
	for i := 0; i < seg.packetCount(); i++ {
		if p := packetAt(seg.packets, i); tsPID(p) == seg.pcrPID {
			cc = tsCC(p)
			if tsHasPayload(p) {
				cc--
			}
			break
		}
	}

	// Each added PCR goes in the last null packet that keeps it within
	// pcrFillSpacing of the PCR before, or the first one after when none does.
	// The window's audio packets are left for its audio.
	isFreeNull := func(i int) bool { return !audioSlots[i] && tsPID(packetAt(seg.packets, i)) == nullPID }
	nextChance := make([]int64, len(seam))
	chance := nextLapPCR
	for k := len(seam) - 1; k >= 0; k-- {
		nextChance[k] = chance
		if isFreeNull(seam[k].index) {
			chance = seam[k].time
		}
	}
	added := 0
	last := times[lastPCR]
	tailLast := last
	headFirst, haveHead := int64(0), false
	for k, sp := range seam {
		if !isFreeNull(sp.index) || nextLapPCR-last <= pcrFillSpacing || nextChance[k]-last <= pcrFillSpacing {
			continue
		}
		writePCRPacket(packetAt(seg.packets, sp.index), seg.pcrPID, cc)
		added++
		last = sp.time
		if k < tailSize {
			tailLast = sp.time
		} else if !haveHead {
			headFirst, haveHead = sp.time, true
		}
	}

	// A variable-rate lap sent in less time than it lasts ends early: packets
	// added after its last one carry PCRs on to the next lap.
	if !seg.constantRate {
		next := nextLapPCR
		if haveHead {
			next = headFirst
		}
		for prev := tailLast; next-prev > maxPCRSpacing; {
			t := min(max(seg.sendTimes[len(seg.sendTimes)-1], prev+pcrFillSpacing), lapTime-1)
			if t <= prev {
				break
			}
			seg.packets = append(seg.packets, make([]byte, PacketSize)...)
			writePCRPacket(packetAt(seg.packets, seg.packetCount()-1), seg.pcrPID, cc)
			seg.sendTimes = append(seg.sendTimes, t)
			added++
			prev = t
		}
		times = seg.sendTimes
	}

	// A gap no null packet was near enough to fill, wider than any in the lap.
	widestInLap := int64(0)
	for k := 1; k < len(pcrPackets); k++ {
		widestInLap = max(widestInLap, times[pcrPackets[k]]-times[pcrPackets[k-1]])
	}
	widestAtSeam := int64(0)
	prev := times[lastPCR]
	stepTo := func(t int64) {
		widestAtSeam = max(widestAtSeam, t-prev)
		prev = t
	}
	for i := lastPCR + 1; i < seg.packetCount(); i++ {
		if isPCR(i) {
			stepTo(times[i])
		}
	}
	for i := 0; i < firstPCR; i++ {
		if isPCR(i) {
			stepTo(lapTime + times[i])
		}
	}
	stepTo(nextLapPCR)
	if widestAtSeam > max(int64(maxPCRSpacing), widestInLap) {
		seg.warnings = append(seg.warnings, fmt.Sprintf(
			"the PCR is %.0f ms apart across the seam: too few null packets there to carry more", float64(widestAtSeam)/27_000))
	}
	return added
}

// renumberCC makes the continuity counters of pid continue without a gap
// through the segment, from its first packet's.
func renumberCC(seg *loopSegment, pid int) {
	started := false
	var cc byte
	for i := 0; i < seg.packetCount(); i++ {
		p := packetAt(seg.packets, i)
		if tsPID(p) != pid || !tsHasPayload(p) {
			continue
		}
		if started {
			cc = (cc + 1) & 0x0f
			p[3] = p[3]&0xf0 | cc
		} else {
			started, cc = true, tsCC(p)
		}
	}
}

// buildSegment builds one lap from the source packets of the loop window.
func buildSegment(data []byte, idx *streamIndex, plan *loopPlan) (*loopSegment, error) {
	start, end := plan.startPacket, plan.endPacket
	windowPackets := end - start
	window := append([]byte(nil), data[start*PacketSize:end*PacketSize]...)
	seg := &loopSegment{
		duration:     plan.duration,
		pcrPID:       idx.pcrPID,
		constantRate: plan.constantRate,
		scte35PIDs:   idx.scte35PIDs,
	}

	for slot := 0; slot < windowPackets; slot++ {
		p := packetAt(window, slot)
		if p[0] != syncByte || tsOversizedAF(p) {
			writeNullPacket(p)
		}
		// Timestamps and counters are made continuous, so nothing is discontinuous.
		clearDiscontinuity(p)
	}
	seg.stripped = stripPartialUnits(window, idx, start, end)

	// SCTE-35 cues the plan leaves out free their packets, and cues sent before
	// the window for a break inside the loop go at the start of the lap. The
	// counters of a PID that changed are renumbered.
	var injected []byte
	renumber := make(map[int]bool)
	for _, ls := range plan.scte35 {
		for pkt := range ls.drop {
			if pkt >= start && pkt < end {
				stripPayload(packetAt(window, pkt-start))
				renumber[ls.PID] = true
			}
		}
		for _, sec := range ls.inject {
			injected = append(injected, packetizeSection(ls.PID, sec)...)
			renumber[ls.PID] = true
		}
	}
	nInjected := len(injected) / PacketSize
	seg.scte35Injected = nInjected

	// A constant-rate lap must last exactly the loop duration at the source
	// rate: pad with null packets before the seam, or drop the last null packets.
	kept := make([]bool, windowPackets)
	for i := range kept {
		kept[i] = true
	}
	count := windowPackets + nInjected
	if plan.constantRate {
		for slot := windowPackets - 1; slot >= 0 && count > plan.lapPackets; slot-- {
			if tsPID(packetAt(window, slot)) == nullPID {
				kept[slot] = false
				count--
				seg.removedNulls++
			}
		}
		seg.addedNulls = max(plan.lapPackets-count, 0)
	}

	// Each lap places its own audio. A packet carrying the PCR keeps it.
	isAudio := make(map[int]bool)
	for _, a := range idx.audio {
		isAudio[a.pid] = true
	}
	wasAudio := make([]bool, windowPackets)
	for slot := 0; slot < windowPackets; slot++ {
		if p := packetAt(window, slot); isAudio[tsPID(p)] {
			stripPayload(p)
			wasAudio[slot] = true
		}
	}

	var sourceIndex []int
	var audioSlots []bool
	lapPacketOfSlot := make([]int, windowPackets+1)
	seg.packets = make([]byte, 0, (count+seg.addedNulls)*PacketSize)
	seg.packets = append(seg.packets, injected...)
	for range nInjected {
		sourceIndex = append(sourceIndex, start)
		audioSlots = append(audioSlots, false)
	}
	for slot := 0; slot < windowPackets; slot++ {
		lapPacketOfSlot[slot] = seg.packetCount()
		if kept[slot] {
			seg.packets = append(seg.packets, packetAt(window, slot)...)
			sourceIndex = append(sourceIndex, start+slot)
			audioSlots = append(audioSlots, wasAudio[slot])
		}
	}
	lapPacketOfSlot[windowPackets] = seg.packetCount()
	for range seg.addedNulls {
		seg.packets = append(seg.packets, make([]byte, PacketSize)...)
		writeNullPacket(packetAt(seg.packets, seg.packetCount()-1))
		sourceIndex = append(sourceIndex, end)
		audioSlots = append(audioSlots, false)
	}
	for pid := range renumber {
		renumberCC(seg, pid)
	}

	timeline := newPCRTimeline(idx)
	stretch := timeline.stretchOf(start)
	if stretch == nil {
		return nil, fmt.Errorf("no PCR to time the loop by")
	}
	origin := timeline.at(stretch, start)
	seg.firstPCR = wrapAdd(0, origin, pcrWrapU)
	lapTime := plan.duration * pcrPerTick
	seg.sendTimes = make([]int64, seg.packetCount())
	if plan.constantRate {
		for i := range seg.sendTimes {
			seg.sendTimes[i] = evenTime(i, seg.packetCount(), lapTime)
		}
	} else {
		// Packets keep their source timing; a window that takes longer than the
		// loop lasts is compressed into it.
		scale := 1.0
		if plan.windowTime > lapTime {
			scale = float64(lapTime) / float64(plan.windowTime)
		}
		prev := int64(0)
		for i := range seg.sendTimes {
			t := int64(math.Round(float64(timeline.at(stretch, sourceIndex[i])-origin) * scale))
			prev = min(max(t, prev), lapTime-1)
			seg.sendTimes[i] = prev
		}
	}
	seg.addedPCRPackets = fillSeamPCRGap(seg, audioSlots)
	seg.lapPackets = seg.packetCount()
	if plan.constantRate {
		seg.lapPackets = plan.lapPackets
	}

	if idx.pictures[plan.startPicture].info.accessPoint != apClosedGOP {
		seg.seamMarks = markSeamPictures(seg, idx, plan.startPicture)
	}

	for _, ls := range plan.scte35 {
		if ls.Kept > 0 {
			if p := newSCTE35Patcher(seg.packets, seg.packetCount(), ls.PID, ls.EventIDStep); p != nil {
				seg.scte35 = append(seg.scte35, p)
			}
		}
	}

	for _, fit := range plan.audio {
		if fit.frameCount > 0 {
			seg.audio = append(seg.audio, prepareAudio(data, idx, plan, fit, lapPacketOfSlot, &seg.audioHeadCuts))
		}
	}

	// The PAT and PMT in force at the loop start, when each fits in one packet.
	for _, pid := range []int{0, idx.pmtPID} {
		units := idx.units[pid]
		after := sort.Search(len(units), func(k int) bool { return units[k].first >= start })
		if after > 0 && units[after-1].first == units[after-1].last {
			seg.preamble = append(seg.preamble, packetAt(data, units[after-1].first)...)
		}
	}
	return seg, nil
}
