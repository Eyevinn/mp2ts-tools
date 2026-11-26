package internal

import (
	"math"
	"sort"
)

// audioLapController ends each lap's audio on the frame that keeps the audio
// continuous with the next lap, so the audio timestamps never jump. Each lap's
// audio is shifted by an offset relative to the video, which moves by the loop
// duration; the offset is never negative and always less than a frame, so it
// never drifts and audio is never earlier than in the source. lapSpan is how
// long a lap's audio lasts from its first frame: the loop duration, less the gap
// at its start when the file lacks the audio there.
type audioLapController struct {
	frameStarts []int64
	lapSpan     int64
	offset      int64
}

type audioLapResult struct {
	keptFrames int
	offset     int64 // in the stream's time unit
	// gap: the segment holds too little audio to reach the next lap, which then
	// starts after a gap instead of drifting.
	gap bool
}

func (c *audioLapController) next() audioLapResult {
	frameCount := len(c.frameStarts) - 1
	// Keep the frames up to the first boundary at or after where the next lap
	// starts: the next lap's audio then follows on, shifted later than the video
	// by less than a frame, never earlier.
	target := c.lapSpan - c.offset
	kept := 1 + sort.Search(len(c.frameStarts)-1, func(k int) bool { return c.frameStarts[1+k] >= target })
	kept = min(kept, frameCount)
	nextOffset := c.offset + c.frameStarts[kept] - c.lapSpan
	if nextOffset < 0 {
		return audioLapResult{keptFrames: kept, offset: c.offset, gap: true}
	}
	r := audioLapResult{keptFrames: kept, offset: c.offset}
	c.offset = nextOffset
	return r
}

// LapStatistics counts what the lap writer did.
type LapStatistics struct {
	MovedAudioPackets    int `json:"movedAudioPackets"`    // sent in another lap than the one they play in
	InsertedAudioPackets int `json:"insertedAudioPackets"` // no free packet in time, so the lap grew
	MovedDataPackets     int `json:"movedDataPackets"`
	InsertedDataPackets  int `json:"insertedDataPackets"`
	AudioGaps            int `json:"audioGaps"` // laps whose audio has a gap at a seam, per stream
	SCTE35Patched        int `json:"scte35Patched"`
}

// audioLap is one lap's audio of one stream.
type audioLap struct {
	exists     bool
	keptFrames int
	offset     int64 // 90 kHz, added to its timestamps
	gap        bool
}

// placedPacket is an audio or data packet that a lap places.
type placedPacket struct {
	stream   int // audio streams first, then data streams
	target   int
	byteAt   int // into lapWriter.placedBytes
	lapShift int
	// position is 2 * lap packet when it takes a free one, 2 * lap packet - 1
	// when it goes before that one, as no free packet was left in time.
	position int
}

type lapContinuity struct {
	started   bool
	last      byte
	offset    byte
	offsetLap int // lap + 1 the offset was set for
}

const (
	freePacket    = -1
	droppedPacket = -2
	unplaced      = math.MinInt
)

// lapWriter writes the laps of a segment, patching each packet as little as it
// can: continuity counters continue across laps, PCR and PES timestamps advance
// by one loop duration per lap (audio by that plus its offset), the audio ends
// on the frame the controller picks, and SCTE-35 pts_adjustment advances with
// the video and its event IDs by the plan's step. Audio and data the source
// sends after the window are sent at the start of the next lap, and those it
// sends before the window at the end of the previous one.
type lapWriter struct {
	seg         *loopSegment
	laps        int // laps a bounded run writes, so the last leaves out the next lap's audio and data; 0: endless
	lap         int
	controllers []audioLapController
	// audioLaps holds per audio stream the audio of the previous, this and the next lap.
	audioLaps     [][3]audioLap
	placedPID     map[int]bool
	scte35PID     map[int]bool
	continuity    [numPIDs]lapContinuity
	placedPackets []placedPacket
	placedBytes   []byte
	owner         []int   // per lap packet: the placed packet in it, or freePacket/droppedPacket
	times         []int64 // 27 MHz from timeBase, per packet of the last write
	timeBase      int64
	stats         LapStatistics
}

func newLapWriter(seg *loopSegment, laps int) *lapWriter {
	w := &lapWriter{seg: seg, laps: laps, placedPID: make(map[int]bool), scte35PID: make(map[int]bool)}
	for s, a := range seg.audio {
		w.placedPID[a.pid] = true
		w.controllers = append(w.controllers, audioLapController{frameStarts: a.frameStarts,
			lapSpan: seg.duration*a.timeScale - a.headGap})
		w.audioLaps = append(w.audioLaps, [3]audioLap{})
		w.audioLaps[s][1] = w.nextAudioLap(s, 0)
		w.audioLaps[s][2] = w.nextAudioLap(s, 1)
	}
	for _, d := range seg.data {
		w.placedPID[d.pid] = true
	}
	for _, pid := range seg.scte35PIDs {
		w.scte35PID[pid] = true
	}
	return w
}

// sendTime returns when packet i of the last write is due, 27 MHz from the
// start of lap 0.
func (w *lapWriter) sendTime(i int) int64 { return w.timeBase + w.times[i] }

// writePreamble returns the PAT and PMT, then the first lap's audio and data
// that the source sends before the window.
func (w *lapWriter) writePreamble() []byte {
	out := append([]byte(nil), w.seg.preamble...)
	for i := 0; i < len(out)/PacketSize; i++ {
		p := packetAt(out, i)
		c := &w.continuity[tsPID(p)]
		cc := tsCC(p)
		if c.started {
			cc = (c.last + 1) & 0x0f
		}
		c.started, c.last = true, cc
		p[3] = p[3]&0xf0 | cc
	}
	w.placedPackets, w.placedBytes = w.placedPackets[:0], w.placedBytes[:0]
	for s := range w.audioLaps {
		w.collectAudio(s, w.audioLaps[s][1], -1)
	}
	for s := range w.seg.data {
		w.collectData(s, 0, -1)
	}
	for _, pp := range w.placedPackets {
		out = append(out, w.placedBytes[pp.byteAt:pp.byteAt+PacketSize]...)
		w.patchPacket(packetAt(out, len(out)/PacketSize-1), 0)
	}
	w.countMoved()
	w.timeBase = 0
	w.times = make([]int64, len(out)/PacketSize)
	return out
}

// writeLap returns the next lap.
func (w *lapWriter) writeLap() []byte {
	w.placedPackets, w.placedBytes = w.placedPackets[:0], w.placedBytes[:0]
	nextLapFollows := w.laps <= 0 || w.lap+1 < w.laps
	for s := range w.audioLaps {
		laps := &w.audioLaps[s]
		if laps[1].gap || w.seg.audio[s].headGap > 0 {
			w.stats.AudioGaps++
		}
		w.collectAudio(s, laps[0], 1)
		w.collectAudio(s, laps[1], 0)
		if nextLapFollows {
			w.collectAudio(s, laps[2], -1)
		}
	}
	for s := range w.seg.data {
		if w.lap > 0 {
			w.collectData(s, w.lap-1, 1)
		}
		w.collectData(s, w.lap, 0)
		if nextLapFollows {
			w.collectData(s, w.lap+1, -1)
		}
	}
	w.placePackets()

	// A constant-rate lap keeps its length: unused free packets at its end make
	// room for placed packets that had to be inserted.
	baseCount := w.seg.packetCount()
	var inserted []int
	for i, pp := range w.placedPackets {
		if pp.position%2 != 0 {
			inserted = append(inserted, i)
			if pp.stream < len(w.seg.audio) {
				w.stats.InsertedAudioPackets++
			} else {
				w.stats.InsertedDataPackets++
			}
		}
	}
	w.countMoved()
	sort.SliceStable(inserted, func(a, b int) bool {
		return w.placedPackets[inserted[a]].position < w.placedPackets[inserted[b]].position
	})
	count := baseCount + len(inserted)
	for slot := baseCount - 1; slot >= 0 && w.seg.constantRate && count > w.seg.lapPackets; slot-- {
		if w.owner[slot] == freePacket && tsPID(packetAt(w.seg.packets, slot)) == nullPID {
			w.owner[slot] = droppedPacket
			count--
		}
	}

	var scte35 map[int][]byte
	if w.lap > 0 {
		for _, p := range w.seg.scte35 {
			for slot, pkt := range p.packets(w.seg.packets, w.lap, w.seg.duration) {
				if scte35 == nil {
					scte35 = make(map[int][]byte)
				}
				scte35[slot] = pkt
			}
			w.stats.SCTE35Patched += p.sectionCount()
		}
	}

	out := make([]byte, 0, count*PacketSize)
	w.times = w.times[:0]
	lapTime := w.seg.duration * pcrPerTick
	appendPacket := func(p []byte, slot int) {
		out = append(out, p...)
		if w.seg.constantRate {
			w.times = append(w.times, evenTime(len(w.times), count, lapTime))
		} else {
			w.times = append(w.times, w.seg.sendTimes[min(slot, baseCount-1)])
		}
	}
	placed := func(i int) []byte {
		at := w.placedPackets[i].byteAt
		return w.placedBytes[at : at+PacketSize]
	}
	next := 0
	for slot := 0; slot <= baseCount; slot++ {
		for ; next < len(inserted) && w.placedPackets[inserted[next]].position == 2*slot-1; next++ {
			appendPacket(placed(inserted[next]), slot)
		}
		if slot == baseCount || w.owner[slot] == droppedPacket {
			continue
		}
		switch {
		case w.owner[slot] >= 0:
			appendPacket(placed(w.owner[slot]), slot)
		case scte35[slot] != nil:
			appendPacket(scte35[slot], slot)
		default:
			appendPacket(packetAt(w.seg.packets, slot), slot)
		}
	}

	w.timeBase = int64(w.lap) * lapTime
	for i := range w.times {
		w.patchPacket(packetAt(out, i), w.times[i])
	}
	for s := range w.audioLaps {
		laps := &w.audioLaps[s]
		laps[0], laps[1], laps[2] = laps[1], laps[2], w.nextAudioLap(s, w.lap+2)
	}
	w.lap++
	return out
}

// writeEnding returns the last lap's audio and data that the next lap would
// have carried, to end a bounded run without cutting it.
func (w *lapWriter) writeEnding() []byte {
	var out []byte
	w.times = w.times[:0]
	w.placedPackets, w.placedBytes = w.placedPackets[:0], w.placedBytes[:0]
	for s := range w.audioLaps {
		w.collectAudio(s, w.audioLaps[s][0], 1)
	}
	for s := range w.seg.data {
		if w.lap > 0 {
			w.collectData(s, w.lap-1, 1)
		}
	}
	baseCount := w.seg.packetCount()
	lapTime := w.seg.duration * pcrPerTick
	w.timeBase = int64(w.lap) * lapTime
	t := int64(0)
	for _, pp := range w.placedPackets {
		out = append(out, w.placedBytes[pp.byteAt:pp.byteAt+PacketSize]...)
		slot := min(pp.target, baseCount-1)
		if w.seg.constantRate {
			t = max(t, evenTime(slot, w.seg.lapPackets, lapTime))
		} else {
			t = max(t, w.seg.sendTimes[slot])
		}
		w.times = append(w.times, t)
		w.patchPacket(packetAt(out, len(w.times)-1), t)
	}
	w.countMoved()
	return out
}

func (w *lapWriter) nextAudioLap(s, lap int) audioLap {
	r := w.controllers[s].next()
	return audioLap{
		exists:     true,
		keptFrames: r.keptFrames,
		offset:     int64(lap)*w.seg.duration + roundedDiv(r.offset, w.seg.audio[s].timeScale),
		gap:        r.gap,
	}
}

func (w *lapWriter) collectAudio(s int, al audioLap, lapShift int) {
	if !al.exists {
		return
	}
	for k := range w.seg.audio[s].pes {
		p := &w.seg.audio[s].pes[k]
		if p.firstFrame >= al.keptFrames {
			break
		}
		if p.lapShift != lapShift || len(p.frameEnds) == 0 {
			continue
		}
		begin := len(w.placedBytes)
		w.placedBytes = append(w.placedBytes, p.packets...)
		packets := w.placedBytes[begin:]
		frames := min(al.keptFrames-p.firstFrame, len(p.frameEnds))
		if keep := p.frameEnds[frames-1]; keep < p.packetEnds[len(p.packetEnds)-1] {
			truncatePES(packets, p.packetEnds, keep)
		}
		first := packetAt(packets, 0)
		if h, ok := findPESHeader(first); ok {
			shiftPESTimestamps(first, h, al.offset)
		}
		for i := 0; i < p.packetCount(); i++ {
			if tsPID(packetAt(packets, i)) == nullPID {
				continue // cut off with the frames after the lap's last
			}
			w.placedPackets = append(w.placedPackets, placedPacket{stream: s, target: p.targets[i],
				byteAt: begin + i*PacketSize, lapShift: lapShift})
		}
	}
}

// collectData collects the data PES of stream s's lap that are sent lapShift
// laps from it.
func (w *lapWriter) collectData(s, lap, lapShift int) {
	offset := int64(lap) * w.seg.duration
	for k := range w.seg.data[s].pes {
		p := &w.seg.data[s].pes[k]
		if p.lapShift != lapShift {
			continue
		}
		begin := len(w.placedBytes)
		w.placedBytes = append(w.placedBytes, p.packets...)
		if first := w.placedBytes[begin : begin+PacketSize]; offset != 0 {
			if h, ok := findPESHeader(first); ok {
				shiftPESTimestamps(first, h, offset)
			}
		}
		for i := 0; i < p.packetCount(); i++ {
			w.placedPackets = append(w.placedPackets, placedPacket{stream: len(w.seg.audio) + s, target: p.targets[i],
				byteAt: begin + i*PacketSize, lapShift: lapShift})
		}
	}
}

// countMoved counts the collected packets sent in another lap than their own.
func (w *lapWriter) countMoved() {
	for _, pp := range w.placedPackets {
		switch {
		case pp.lapShift == 0:
		case pp.stream < len(w.seg.audio):
			w.stats.MovedAudioPackets++
		default:
			w.stats.MovedDataPackets++
		}
	}
}

// shiftPESTimestamps adds offset to the PTS and DTS of a PES header.
func shiftPESTimestamps(p []byte, h pesHeaderAt, offset int64) {
	for _, f := range []int{h.pts, h.dts} {
		if f >= 0 {
			writeTimestamp(p[f:], wrapAdd(readTimestamp(p[f:]), offset, ptsWrapU))
		}
	}
}

func (w *lapWriter) placePackets() {
	baseCount := w.seg.packetCount()
	w.owner = w.owner[:0]
	for range baseCount {
		w.owner = append(w.owner, freePacket)
	}
	isFree := func(slot int) bool {
		return w.owner[slot] == freePacket && tsPID(packetAt(w.seg.packets, slot)) == nullPID
	}

	// A packet the source sends inside the window goes back where it was.
	for i := range w.placedPackets {
		pp := &w.placedPackets[i]
		pp.position = unplaced
		if pp.lapShift == 0 && pp.target < baseCount && isFree(pp.target) {
			w.owner[pp.target] = i
			pp.position = 2 * pp.target
		}
	}

	// The rest takes the first free packet at or after where it is due that
	// keeps its stream in order, or goes just before the stream's next placed
	// packet when no free packet is left before it.
	bounds := make([]int, len(w.placedPackets))
	for begin := 0; begin < len(w.placedPackets); {
		stream := w.placedPackets[begin].stream
		end := begin
		for end < len(w.placedPackets) && w.placedPackets[end].stream == stream {
			end++
		}
		nextPlaced := baseCount
		for i := end - 1; i >= begin; i-- {
			bounds[i] = nextPlaced
			if w.placedPackets[i].position != unplaced {
				nextPlaced = w.placedPackets[i].target
			}
		}
		last := -2
		cursor := 0
		for i := begin; i < end; i++ {
			pp := &w.placedPackets[i]
			if pp.position == unplaced {
				slot := max(pp.target, (last+2)/2, cursor)
				for slot < bounds[i] && !isFree(slot) {
					slot++
				}
				cursor = slot
				if slot < bounds[i] {
					w.owner[slot] = i
					pp.position = 2 * slot
				} else {
					pp.position = 2*bounds[i] - 1
				}
			}
			last = pp.position
		}
		begin = end
	}
}

func (w *lapWriter) nextCC(pid int, source byte, hasPayload bool) byte {
	c := &w.continuity[pid]
	if !hasPayload {
		// Only packets with payload advance the counter.
		if c.started {
			return c.last
		}
		return source
	}
	if w.placedPID[pid] {
		// Placed packets come from several laps, so they are counted afresh.
		if c.started {
			c.last = (c.last + 1) & 0x0f
		} else {
			c.last = source
		}
		c.started = true
		return c.last
	}
	// One offset per PID and lap keeps the source's own counter pattern, such as
	// a duplicate packet, while joining the lap to the previous one.
	if c.offsetLap != w.lap+1 {
		c.offset = 0
		if c.started {
			c.offset = (c.last + 1 - source) & 0x0f
		}
		c.offsetLap = w.lap + 1
	}
	c.started = true
	c.last = (source + c.offset) & 0x0f
	return c.last
}

func (w *lapWriter) patchPacket(p []byte, t int64) {
	if p[0] != syncByte {
		return
	}
	pid := tsPID(p)
	if pid == nullPID {
		return
	}
	p[3] = p[3]&0xf0 | w.nextCC(pid, tsCC(p), tsHasPayload(p))

	pcrOffset := int64(w.lap) * w.seg.duration * pcrPerTick
	if tsCarriesPCR(p) {
		f := p[pcrFieldOffset:]
		if pid == w.seg.pcrPID {
			writePCR(f, wrapAdd(w.seg.firstPCR, pcrOffset+t, pcrWrapU))
		} else if pcrOffset != 0 {
			writePCR(f, wrapAdd(readPCR(f), pcrOffset, pcrWrapU))
		}
	}
	if o, ok := tsOPCROffset(p); ok && pcrOffset != 0 {
		writePCR(p[o:], wrapAdd(readPCR(p[o:]), pcrOffset, pcrWrapU))
	}

	// Placed packets got their timestamps when they were collected, and SCTE-35
	// sections are re-stamped whole.
	if !tsPUSI(p) || !tsHasPayload(p) || w.placedPID[pid] || w.scte35PID[pid] {
		return
	}
	if h, ok := findPESHeader(p); ok && w.lap > 0 {
		shiftPESTimestamps(p, h, int64(w.lap)*w.seg.duration)
	}
}
