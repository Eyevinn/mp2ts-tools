package internal

import (
	"math"
	"sort"
)

// audioLapController ends each lap's audio on the frame that keeps the audio
// continuous with the next lap, so the audio timestamps never jump. Each lap's
// audio is shifted by an offset relative to the video, which moves by the loop
// duration; the offset is never negative and always less than a frame, so it
// never drifts and audio is never earlier than in the source.
type audioLapController struct {
	frameStarts []int64
	lapDuration int64
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
	target := c.lapDuration - c.offset
	kept := 1 + sort.Search(len(c.frameStarts)-1, func(k int) bool { return c.frameStarts[1+k] >= target })
	kept = min(kept, frameCount)
	nextOffset := c.offset + c.frameStarts[kept] - c.lapDuration
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
	AudioGaps            int `json:"audioGaps"`
	SCTE35Patched        int `json:"scte35Patched"`
}

// audioLap is one lap's audio of one stream.
type audioLap struct {
	exists     bool
	keptFrames int
	offset     int64 // 90 kHz, added to its timestamps
	gap        bool
}

type audioPacket struct {
	stream   int
	target   int
	byteAt   int // into lapWriter.audioBytes
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
// the video and its event IDs by the plan's step. Audio the source sends after
// the window is sent at the start of the next lap, and audio it sends before the
// window at the end of the previous one.
type lapWriter struct {
	seg         *loopSegment
	laps        int // laps a bounded run writes, so the last leaves out the next lap's audio; 0: endless
	lap         int
	controllers []audioLapController
	// audioLaps holds per audio stream the audio of the previous, this and the next lap.
	audioLaps    [][3]audioLap
	audioPID     map[int]bool
	scte35PID    map[int]bool
	continuity   [numPIDs]lapContinuity
	audioPackets []audioPacket
	audioBytes   []byte
	owner        []int   // per lap packet: the audio packet in it, or freePacket/droppedPacket
	times        []int64 // 27 MHz from timeBase, per packet of the last write
	timeBase     int64
	stats        LapStatistics
}

func newLapWriter(seg *loopSegment, laps int) *lapWriter {
	w := &lapWriter{seg: seg, laps: laps, audioPID: make(map[int]bool), scte35PID: make(map[int]bool)}
	for s, a := range seg.audio {
		w.audioPID[a.pid] = true
		w.controllers = append(w.controllers, audioLapController{frameStarts: a.frameStarts,
			lapDuration: seg.duration * a.timeScale})
		w.audioLaps = append(w.audioLaps, [3]audioLap{})
		w.audioLaps[s][1] = w.nextAudioLap(s, 0)
		w.audioLaps[s][2] = w.nextAudioLap(s, 1)
	}
	for _, pid := range seg.scte35PIDs {
		w.scte35PID[pid] = true
	}
	return w
}

// sendTime returns when packet i of the last write is due, 27 MHz from the
// start of lap 0.
func (w *lapWriter) sendTime(i int) int64 { return w.timeBase + w.times[i] }

// writePreamble returns the PAT and PMT, then the first lap's audio that the
// source sends before the window.
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
	w.audioPackets, w.audioBytes = w.audioPackets[:0], w.audioBytes[:0]
	for s := range w.audioLaps {
		w.collectAudio(s, w.audioLaps[s][1], -1)
	}
	for _, ap := range w.audioPackets {
		out = append(out, w.audioBytes[ap.byteAt:ap.byteAt+PacketSize]...)
		w.patchPacket(packetAt(out, len(out)/PacketSize-1), 0)
	}
	w.stats.MovedAudioPackets += len(w.audioPackets)
	w.timeBase = 0
	w.times = make([]int64, len(out)/PacketSize)
	return out
}

// writeLap returns the next lap.
func (w *lapWriter) writeLap() []byte {
	w.audioPackets, w.audioBytes = w.audioPackets[:0], w.audioBytes[:0]
	for s := range w.audioLaps {
		laps := &w.audioLaps[s]
		if laps[1].gap {
			w.stats.AudioGaps++
		}
		w.collectAudio(s, laps[0], 1)
		w.collectAudio(s, laps[1], 0)
		if w.laps <= 0 || w.lap+1 < w.laps {
			w.collectAudio(s, laps[2], -1)
		}
	}
	w.placeAudio()

	// A constant-rate lap keeps its length: unused free packets at its end make
	// room for audio packets that had to be inserted.
	baseCount := w.seg.packetCount()
	var inserted []int
	for i, ap := range w.audioPackets {
		if ap.position%2 != 0 {
			inserted = append(inserted, i)
		}
		if ap.lapShift != 0 {
			w.stats.MovedAudioPackets++
		}
	}
	w.stats.InsertedAudioPackets += len(inserted)
	sort.SliceStable(inserted, func(a, b int) bool {
		return w.audioPackets[inserted[a]].position < w.audioPackets[inserted[b]].position
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
	audio := func(i int) []byte {
		at := w.audioPackets[i].byteAt
		return w.audioBytes[at : at+PacketSize]
	}
	next := 0
	for slot := 0; slot <= baseCount; slot++ {
		for ; next < len(inserted) && w.audioPackets[inserted[next]].position == 2*slot-1; next++ {
			appendPacket(audio(inserted[next]), slot)
		}
		if slot == baseCount || w.owner[slot] == droppedPacket {
			continue
		}
		switch {
		case w.owner[slot] >= 0:
			appendPacket(audio(w.owner[slot]), slot)
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

// writeEnding returns the last lap's audio that the next lap would have
// carried, to end a bounded run without cutting it.
func (w *lapWriter) writeEnding() []byte {
	var out []byte
	w.times = w.times[:0]
	w.audioPackets, w.audioBytes = w.audioPackets[:0], w.audioBytes[:0]
	for s := range w.audioLaps {
		w.collectAudio(s, w.audioLaps[s][0], 1)
	}
	baseCount := w.seg.packetCount()
	lapTime := w.seg.duration * pcrPerTick
	w.timeBase = int64(w.lap) * lapTime
	t := int64(0)
	for _, ap := range w.audioPackets {
		out = append(out, w.audioBytes[ap.byteAt:ap.byteAt+PacketSize]...)
		slot := min(ap.target, baseCount-1)
		if w.seg.constantRate {
			t = max(t, evenTime(slot, w.seg.lapPackets, lapTime))
		} else {
			t = max(t, w.seg.sendTimes[slot])
		}
		w.times = append(w.times, t)
		w.patchPacket(packetAt(out, len(w.times)-1), t)
	}
	w.stats.MovedAudioPackets += len(w.audioPackets)
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
		begin := len(w.audioBytes)
		w.audioBytes = append(w.audioBytes, p.packets...)
		packets := w.audioBytes[begin:]
		frames := min(al.keptFrames-p.firstFrame, len(p.frameEnds))
		if keep := p.frameEnds[frames-1]; keep < p.packetEnds[len(p.packetEnds)-1] {
			truncatePES(packets, p.packetEnds, keep)
		}
		first := packetAt(packets, 0)
		if h, ok := findPESHeader(first); ok {
			for _, f := range []int{h.pts, h.dts} {
				if f >= 0 {
					writeTimestamp(first[f:], wrapAdd(readTimestamp(first[f:]), al.offset, ptsWrapU))
				}
			}
		}
		for i := 0; i < p.packetCount(); i++ {
			if tsPID(packetAt(packets, i)) == nullPID {
				continue // cut off with the frames after the lap's last
			}
			w.audioPackets = append(w.audioPackets, audioPacket{stream: s, target: p.targets[i],
				byteAt: begin + i*PacketSize, lapShift: lapShift})
		}
	}
}

func (w *lapWriter) placeAudio() {
	baseCount := w.seg.packetCount()
	w.owner = w.owner[:0]
	for range baseCount {
		w.owner = append(w.owner, freePacket)
	}
	isFree := func(slot int) bool {
		return w.owner[slot] == freePacket && tsPID(packetAt(w.seg.packets, slot)) == nullPID
	}

	// Audio the source sends inside the window goes back where it was.
	for i := range w.audioPackets {
		ap := &w.audioPackets[i]
		ap.position = unplaced
		if ap.lapShift == 0 && ap.target < baseCount && isFree(ap.target) {
			w.owner[ap.target] = i
			ap.position = 2 * ap.target
		}
	}

	// The rest takes the first free packet at or after where it is due that
	// keeps its stream in order, or goes just before the stream's next placed
	// packet when no free packet is left before it.
	bounds := make([]int, len(w.audioPackets))
	for begin := 0; begin < len(w.audioPackets); {
		stream := w.audioPackets[begin].stream
		end := begin
		for end < len(w.audioPackets) && w.audioPackets[end].stream == stream {
			end++
		}
		nextPlaced := baseCount
		for i := end - 1; i >= begin; i-- {
			bounds[i] = nextPlaced
			if w.audioPackets[i].position != unplaced {
				nextPlaced = w.audioPackets[i].target
			}
		}
		last := -2
		cursor := 0
		for i := begin; i < end; i++ {
			ap := &w.audioPackets[i]
			if ap.position == unplaced {
				slot := max(ap.target, (last+2)/2, cursor)
				for slot < bounds[i] && !isFree(slot) {
					slot++
				}
				cursor = slot
				if slot < bounds[i] {
					w.owner[slot] = i
					ap.position = 2 * slot
				} else {
					ap.position = 2*bounds[i] - 1
				}
			}
			last = ap.position
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
	if w.audioPID[pid] {
		// Audio comes from several laps' packets, so it is counted afresh.
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

	// Audio timestamps were set when its packets were collected, and SCTE-35
	// sections are re-stamped whole.
	if !tsPUSI(p) || !tsHasPayload(p) || w.audioPID[pid] || w.scte35PID[pid] {
		return
	}
	videoOffset := int64(w.lap) * w.seg.duration
	h, ok := findPESHeader(p)
	if !ok || videoOffset == 0 {
		return
	}
	for _, f := range []int{h.pts, h.dts} {
		if f >= 0 {
			writeTimestamp(p[f:], wrapAdd(readTimestamp(p[f:]), videoOffset, ptsWrapU))
		}
	}
}
