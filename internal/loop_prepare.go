package internal

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/Comcast/gots/v2"
	"github.com/Comcast/gots/v2/packet"
	"github.com/Comcast/gots/v2/packet/adaptationfield"
	"github.com/Comcast/gots/v2/pes"
)

// audioSpareFrames is how many audio frames past the loop end are kept in the
// segment so the per-wrap drift controller can include the boundary-crossing
// frame. Two would suffice for the worst-case offset; three is a safety margin.
const audioSpareFrames = 3

// LoopSegment is the prepared, perfectly-loopable byte segment: emitting it once
// per wrap (with a per-wrap timestamp offset) yields a seamless constant-rate
// stream. Timestamps are baked for wrap 0; PCR is linear across the segment.
type LoopSegment struct {
	Data          []byte
	NumPackets    int
	LoopDurTicks  int64
	VideoPID      int
	PCRPid        int
	AudioStartPTS map[int]int64  // audio pid -> first audio frame PTS of the loop
	scte35        *scte35Patcher // re-stamps the kept SCTE-35 cues per wrap (nil if none)

	// audioRot is, per packet, which wrap an audio packet belongs to relative to
	// the wrap it is sent in: -1 for audio moved to the start of the segment (the
	// end of the previous wrap), +1 for audio moved to its end (the start of the
	// next wrap), else 0.
	audioRot []int8
}

// audioChunk is a kept audio PES (one or more TS packets) of the loop segment.
type audioChunk struct {
	pid  int
	src  int    // source packet the PES started in
	pos  int    // where it goes in the window's source packets, after a move
	rot  int8   // wrap it belongs to relative to the wrap it is sent in
	data []byte // its TS packets
}

// placeAudio places the kept audio PES into the free slots of the window (the
// source packets [start, end), marked used where a packet stays put) and returns
// each slot's audio rotation plus, after the window, the audio that did not fit.
// shift is one loop duration in packets: how far moved audio moves.
//
// Audio is selected by PTS, but it is not multiplexed next to the video of the
// same time: it usually lags, by about the difference in decoder buffer delays,
// so the audio for the end of the loop sits after the window's last packet.
// Appending it there would leave the start of every wrap without audio and send
// a burst before every seam. Instead, audio from after the window is moved to
// the same distance into the window: at the start of the next wrap, which is
// where it sits relative to the video in the source, and it is marked as
// belonging to the previous wrap (rot -1). Audio that leads the video, from
// before the window, moves to its end and belongs to the next wrap (rot +1).
// Each PES goes into the first free slots at or after its position, and never
// before the end of the previous PES of its PID, so in-place audio gets back the
// slots it came from, PES of a PID never interleave, and each PID's audio stays
// in PTS order: a moved PES is never placed past its PID's in-place audio.
func placeAudio(chunks []audioChunk, win []byte, used []bool, start, end, shift int) (rot []int8, overflow []byte, overflowRot []int8) {
	winLen := end - start
	first, last := make(map[int]int), make(map[int]int)
	for i := range chunks {
		c := &chunks[i]
		c.pos = c.src
		switch {
		case c.src >= end:
			c.pos, c.rot = c.src-shift, -1
		case c.src < start:
			c.pos, c.rot = c.src+shift, 1
		default:
			if p, ok := first[c.pid]; !ok || c.pos < p {
				first[c.pid] = c.pos
			}
			if p, ok := last[c.pid]; !ok || c.pos > p {
				last[c.pid] = c.pos
			}
		}
	}
	for i := range chunks {
		c := &chunks[i]
		if p, ok := first[c.pid]; ok && c.rot < 0 && c.pos > p {
			c.pos = p
		}
		if p, ok := last[c.pid]; ok && c.rot > 0 && c.pos < p {
			c.pos = p
		}
	}
	sort.SliceStable(chunks, func(i, j int) bool {
		if chunks[i].pos != chunks[j].pos {
			return chunks[i].pos < chunks[j].pos
		}
		return chunks[i].rot < chunks[j].rot
	})

	// nextFree[i] leads to the first free slot >= i (winLen if none), with path
	// halving, so placing all the audio is close to linear.
	nextFree := make([]int, winLen+1)
	for i := range nextFree {
		nextFree[i] = i
		if i < winLen && used[i] {
			nextFree[i] = i + 1
		}
	}
	find := func(i int) int {
		for nextFree[i] != i {
			nextFree[i] = nextFree[nextFree[i]]
			i = nextFree[i]
		}
		return i
	}
	rot = make([]int8, winLen)
	after := make(map[int]int) // per PID: first slot after its previous PES
	for _, c := range chunks {
		p := min(max(c.pos-start, after[c.pid], 0), winLen)
		for k := 0; k < len(c.data)/PacketSize; k++ {
			pkt := c.data[k*PacketSize : (k+1)*PacketSize]
			s := find(p)
			if s == winLen {
				overflow = append(overflow, pkt...)
				overflowRot = append(overflowRot, c.rot)
				p = winLen
				continue
			}
			copy(win[s*PacketSize:], pkt)
			used[s], rot[s] = true, c.rot
			nextFree[s] = s + 1
			p = s + 1
		}
		after[c.pid] = p
	}
	return rot, overflow, overflowRot
}

// nullPacket returns a standard MPEG-TS null/stuffing packet (PID 8191).
func nullPacket() []byte {
	p := make([]byte, PacketSize)
	p[0] = 0x47
	p[1] = 0x1f
	p[2] = 0xff
	p[3] = 0x10 // payload only, CC 0
	for i := 4; i < PacketSize; i++ {
		p[i] = 0xff
	}
	return p
}

// PrepareLoop scans the file, selects the loop, and builds the loopable segment.
func PrepareLoop(ctx context.Context, path string, fpsNum, fpsDen, durCapMS int) (*LoopSegment, *LoopPlan, error) {
	ts, vid, auds, err := runScan(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	if vid == nil {
		return nil, nil, fmt.Errorf("no video elementary stream found")
	}
	if err := requirePTS(vid, auds); err != nil {
		return nil, nil, err
	}
	if err := ts.requireConstantRate(); err != nil {
		return nil, nil, err
	}
	bitrate, cbr := loopBitrate(ts)
	plan := selectLoop(vid, auds, fpsNum, fpsDen, durCapMS, ts.PCRPid, bitrate, cbr, ts.discontinuities)
	finishPlan(ts, plan)
	if plan == nil || plan.LoopPointType == "none" || plan.NumGOPs < 1 {
		return nil, plan, fmt.Errorf("no loopable interval found (need at least two %s loop points)", plan.LoopPointType)
	}
	seg, err := BuildLoopSegment(ctx, path, ts, plan, bitrate)
	if err != nil {
		return nil, plan, err
	}
	return seg, plan, nil
}

// BuildLoopSegment performs the second pass: it selects the loop's packets
// (dropping null stuffing and out-of-window audio), prepends PAT/PMT, re-pads
// to the target bitrate with fresh null stuffing, and regenerates a linear PCR.
func BuildLoopSegment(ctx context.Context, path string, ts *TSStream, plan *LoopPlan, bitrate int64) (*LoopSegment, error) {
	vpid := -1
	vcodec := CODEC_UNKNOWN
	audioPIDs := make(map[int]bool)
	for pid, es := range ts.ElStreams {
		switch es.MediaType {
		case "video":
			vpid, vcodec = pid, es.Codec
		case "audio":
			audioPIDs[pid] = true
		}
	}
	pcrPid := ts.PCRPid
	if pcrPid < 0 {
		pcrPid = vpid
	}
	Vs := plan.StartPTS
	startByte, endByte := plan.packetWindow()
	readStart := plan.audioReadStart()

	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()

	var patPkt, pmtPkt []byte
	// The window's packets, one slot per source packet so that every packet
	// keeps its source timing. Video and other non-audio packets stay in their
	// slots; stuffing, audio, and dropped SCTE-35 slots are free for the audio,
	// which is collected per PES with the source packet each started in.
	winLen := endByte - startByte
	win := make([]byte, winLen*PacketSize)
	used := make([]bool, winLen)
	var chunks []audioChunk

	audioFramers := make(map[int]AudioFramer)
	for pid := range audioPIDs {
		audioFramers[pid] = newAudioFramer(ts.ElStreams[pid].Codec)
	}
	audioBuf := make(map[int][]byte)  // raw packets of the current audio PES per PID
	pesSrc := make(map[int]int)       // source packet the current audio PES started in
	audioStart := make(map[int]int64) // first kept audio frame PTS per PID
	spareFrames := make(map[int]int)  // audio frames kept past the loop end (drift spares)
	audioPast := make(map[int]bool)

	// keepFrame decides whether an audio frame at PTS p belongs in the segment.
	// The audio loop for a PID runs [audioStart, audioStart+loopDur) where
	// audioStart is its first frame at/after Vs; a few spare frames past the end
	// are kept so the per-wrap drift controller has the boundary-crossing frame.
	// keepFrame must use the same audioStart+loopDur boundary the emitter does.
	keepFrame := func(pid int, p int64) bool {
		if SignedPTSDiff(p, Vs) < 0 {
			return false
		}
		if _, ok := audioStart[pid]; !ok {
			audioStart[pid] = p
		}
		if SignedPTSDiff(p, audioStart[pid]+plan.LoopDurTicks) < 0 {
			return true
		}
		spareFrames[pid]++
		if spareFrames[pid] > audioSpareFrames {
			audioPast[pid] = true
			return false
		}
		return true
	}

	// finalizeAudio processes one fully-buffered audio PES. A multi-frame PES
	// whose frames tile the payload is re-packetized into one PES per frame (so
	// the loop can be cut on a frame boundary); single-frame or non-aligned PES
	// are copied unchanged. The result is placed later, from its source position.
	finalizeAudio := func(pid int) {
		raw := audioBuf[pid]
		audioBuf[pid] = nil
		if len(raw) == 0 {
			return
		}
		src := pesSrc[pid]
		emit := func(b []byte) { chunks = append(chunks, audioChunk{pid: pid, src: src, data: b}) }
		pesBytes := extractPESPayload(raw)
		ph, err := pes.NewPESHeader(pesBytes)
		if err != nil || !ph.HasPTS() {
			return
		}
		pts := int64(ph.PTS())
		payload := ph.Data()
		framer := audioFramers[pid]
		if framer == nil {
			if keepFrame(pid, pts) {
				emit(raw)
			}
			return
		}
		frames, missing, serr := framer.Split(payload, len(payload), 0, pts)
		total := 0
		for _, f := range frames {
			total += f.Size
		}
		aligned := serr == nil && missing == 0 && len(frames) > 0 && total == len(payload)
		if !aligned || len(frames) == 1 {
			if keepFrame(pid, pts) {
				emit(raw) // copy unchanged
			}
			return
		}
		streamID := ph.StreamId()
		off := 0
		for _, f := range frames {
			fb := payload[off : off+f.Size]
			off += f.Size
			if keepFrame(pid, f.PTS) {
				emit(packetizePES(pid, buildAudioPES(streamID, f.PTS, fb)))
			}
		}
	}

	var pkt packet.Packet
	pktNr := -1
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if _, err := io.ReadFull(fh, pkt[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}
		pktNr++
		pid := packet.Pid(&pkt)
		if pid == 0 && patPkt == nil {
			patPkt = append([]byte{}, pkt[:]...)
		}
		if pid == ts.PMTPid && pmtPkt == nil {
			pmtPkt = append([]byte{}, pkt[:]...)
		}
		if pktNr < readStart {
			continue
		}
		if pid == stuffingPID {
			continue // drop original stuffing; we re-pad below
		}
		if pid == ts.SCTE35Pid && plan.SCTE35.dropped(pktNr) {
			continue // a cue not fully inside the loop; re-padded like stuffing
		}

		if audioPIDs[pid] {
			// Buffer the whole audio PES (across interleaved video) and process it
			// at the next PES start of this PID.
			if pkt.PayloadUnitStartIndicator() {
				finalizeAudio(pid)
				pesSrc[pid] = pktNr
				audioBuf[pid] = append([]byte(nil), pkt[:]...)
			} else if len(audioBuf[pid]) > 0 {
				audioBuf[pid] = append(audioBuf[pid], pkt[:]...)
			}
		} else if pktNr >= startByte && pktNr < endByte {
			copy(win[(pktNr-startByte)*PacketSize:], pkt[:])
			used[pktNr-startByte] = true
		}

		if pktNr >= endByte {
			allDone := true
			for ap := range audioPIDs {
				if !audioPast[ap] {
					allDone = false
					break
				}
			}
			if allDone || pktNr > endByte+200000 {
				break
			}
		}
	}
	for pid := range audioPIDs {
		finalizeAudio(pid)
	}

	if patPkt == nil || pmtPkt == nil {
		return nil, fmt.Errorf("could not capture PAT/PMT packets")
	}

	// Assemble: PAT/PMT (and SCTE-35 cues moved in from before the loop), then
	// the window's slots with the audio placed into free ones, then audio that
	// did not fit. rot marks, per packet, the wrap the audio belongs to relative
	// to the wrap it is sent in.
	var prefix []byte
	prefix = append(prefix, patPkt...)
	prefix = append(prefix, pmtPkt...)
	if plan.SCTE35 != nil {
		for _, sec := range plan.SCTE35.inject { // cues sent before the loop, for times inside it
			prefix = append(prefix, packetizeSection(ts.SCTE35Pid, sec)...)
		}
	}
	// Audio moves across the boundary by one loop duration. For a constant-rate
	// stream that is the loop's packet count at the bitrate, which differs from
	// the window's length by the seam adjustment below.
	shift := winLen
	if plan.PCR != nil && plan.PCR.ConstantRate && plan.PCR.IdealPackets > 0 {
		shift = int(plan.PCR.IdealPackets)
	}
	winRot, overflow, overflowRot := placeAudio(chunks, win, used, startByte, endByte, shift)
	np := nullPacket()
	nPre := len(prefix) / PacketSize
	var slots [][]byte // nil for a free slot (null stuffing)
	removedSlot := []byte{}
	var rot []int8
	for i := 0; i < nPre; i++ {
		slots, rot = append(slots, prefix[i*PacketSize:(i+1)*PacketSize]), append(rot, 0)
	}
	for j := 0; j < winLen; j++ {
		if used[j] {
			slots = append(slots, win[j*PacketSize:(j+1)*PacketSize])
		} else {
			slots = append(slots, nil)
		}
		rot = append(rot, winRot[j])
	}
	for k := 0; k < len(overflow)/PacketSize; k++ {
		slots, rot = append(slots, overflow[k*PacketSize:(k+1)*PacketSize]), append(rot, overflowRot[k])
	}

	// A constant-rate segment must hold exactly the packets its duration takes at
	// the bitrate. The window's transmission time differs from the loop duration
	// by the change in decoder buffer delay between the two loop points, so null
	// slots are added before the seam, or free slots removed from the end of the
	// window, leaving the timing of the rest of the segment as in the source.
	n := len(slots)
	if plan.PCR != nil && plan.PCR.ConstantRate && bitrate > 0 {
		target := int(float64(plan.LoopDurTicks)/90000.0*float64(bitrate)/8.0/float64(PacketSize) + 0.5)
		for k := len(slots) - 1; k >= 0 && n > target; k-- {
			if slots[k] == nil {
				slots[k] = removedSlot
				n--
			}
		}
		for ; n < target; n++ {
			slots, rot = append(slots, nil), append(rot, 0)
		}
		if n > target {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"the loop needs %d more packets than the bitrate allows; the output rate is raised to fit", n-target))
		}
	}
	data := make([]byte, 0, n*PacketSize)
	audioRot := make([]int8, 0, n)
	for k, sl := range slots {
		switch {
		case sl == nil:
			data = append(data, np...)
		case len(sl) == 0: // removedSlot
			continue
		default:
			data = append(data, sl...)
		}
		audioRot = append(audioRot, rot[k])
	}
	n = len(data) / PacketSize

	// For an open-GOP loop point, mark the splice so the pictures leading each
	// seam are handled: HEVC CRA becomes BLA (RASL discarded instead of corrupting
	// the first GOP); MPEG-2 gets broken_link. Both are no-ops for closed GOPs.
	switch vcodec {
	case CODEC_HEVC:
		rewriteSeamCRAtoBLA(data, n, vpid)
	case CODEC_MPEG2V:
		markSeamBrokenLink(data, n, vpid)
	}

	// Regenerate a linear PCR across the segment spanning exactly LoopDurTicks.
	stampLinearPCR(data, plan.LoopDurTicks, n)

	var scte *scte35Patcher
	if plan.SCTE35 != nil && plan.SCTE35.Kept > 0 {
		scte = newSCTE35Patcher(data, n, ts.SCTE35Pid, plan.SCTE35.EventIDStep)
	}

	return &LoopSegment{
		Data:          data,
		NumPackets:    n,
		LoopDurTicks:  plan.LoopDurTicks,
		VideoPID:      vpid,
		PCRPid:        pcrPid,
		AudioStartPTS: audioStart,
		audioRot:      audioRot,
		scte35:        scte,
	}, nil
}

// stampLinearPCR rewrites every PCR in data so that PCR is linear in packet
// index, spanning loopDur over n packets and anchored to the first PCR value.
func stampLinearPCR(data []byte, loopDur int64, n int) {
	i0 := -1
	var basePCR int64
	var pkt packet.Packet
	for i := 0; i < n; i++ {
		copy(pkt[:], data[i*PacketSize:(i+1)*PacketSize])
		pcr, ok := packetPCR(&pkt)
		if !ok {
			continue
		}
		if i0 < 0 {
			i0 = i
			basePCR = pcr
		}
		newPCR := (basePCR + int64(i-i0)*(loopDur*300)/int64(n)) % PcrWrap
		if newPCR < 0 {
			newPCR += PcrWrap
		}
		pcrBytes, err := adaptationfield.PCR(&pkt)
		if err != nil {
			continue
		}
		gots.InsertPCR(pcrBytes, uint64(newPCR))
		copy(data[i*PacketSize:(i+1)*PacketSize], pkt[:])
	}
}
