package internal

import (
	"context"
	"fmt"
	"io"
	"os"

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
	AudioStartPTS map[int]int64 // audio pid -> first audio frame PTS of the loop
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
	bitrate, cbr := loopBitrate(ts)
	plan := selectLoop(vid, auds, durCapMS, ts.PCRPid, bitrate, cbr)
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
	startByte := int(plan.StartPktNr)
	for _, a := range plan.Audio {
		if a.StartPTS >= 0 && int(a.StartPktNr) < startByte {
			startByte = int(a.StartPktNr)
		}
	}
	endByte := int(plan.EndPktNr)

	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()

	var patPkt, pmtPkt []byte
	// Non-audio packets in original order, and the re-packetized audio to insert
	// before a given video-packet index (its original PES start position, so the
	// audio keeps roughly its original timing — slightly earlier, which is safe).
	videoOther := make([]byte, 0, (endByte-startByte)*PacketSize)
	videoCount := 0
	inserts := make(map[int][]byte)

	audioFramers := make(map[int]AudioFramer)
	for pid := range audioPIDs {
		audioFramers[pid] = newAudioFramer(ts.ElStreams[pid].Codec)
	}
	audioBuf := make(map[int][]byte) // raw packets of the current audio PES per PID
	pesInsertIdx := make(map[int]int)
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
	// are copied unchanged. The result is queued to be inserted at the PES's
	// original start position.
	finalizeAudio := func(pid int) {
		raw := audioBuf[pid]
		audioBuf[pid] = nil
		if len(raw) == 0 {
			return
		}
		idx := pesInsertIdx[pid]
		emit := func(b []byte) { inserts[idx] = append(inserts[idx], b...) }
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
		if pktNr < startByte {
			continue
		}
		if pid == stuffingPID {
			continue // drop original stuffing; we re-pad below
		}

		if audioPIDs[pid] {
			// Buffer the whole audio PES (across interleaved video) and process it
			// at the next PES start of this PID.
			if pkt.PayloadUnitStartIndicator() {
				finalizeAudio(pid)
				pesInsertIdx[pid] = videoCount
				audioBuf[pid] = append([]byte(nil), pkt[:]...)
			} else if len(audioBuf[pid]) > 0 {
				audioBuf[pid] = append(audioBuf[pid], pkt[:]...)
			}
		} else if pktNr < endByte {
			videoOther = append(videoOther, pkt[:]...)
			videoCount++
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

	// Assemble: PAT/PMT, then video/other packets with the re-packetized audio
	// inserted at each PES's original start position.
	seg := make([]byte, 0, len(videoOther)+2*PacketSize)
	seg = append(seg, patPkt...)
	seg = append(seg, pmtPkt...)
	for i := 0; i < videoCount; i++ {
		if a := inserts[i]; len(a) > 0 {
			seg = append(seg, a...)
		}
		seg = append(seg, videoOther[i*PacketSize:(i+1)*PacketSize]...)
	}
	if a := inserts[videoCount]; len(a) > 0 {
		seg = append(seg, a...)
	}
	m := len(seg) / PacketSize

	// Target packet count for the loop duration at the bitrate; re-pad with nulls.
	n := m
	if bitrate > 0 {
		n = int(float64(plan.LoopDurTicks)/90000.0*float64(bitrate)/8.0/float64(PacketSize) + 0.5)
		if n < m {
			n = m
		}
	}
	nulls := n - m
	np := nullPacket()
	data := make([]byte, 0, n*PacketSize)
	errAcc := 0
	for i := 0; i < m; i++ {
		data = append(data, seg[i*PacketSize:(i+1)*PacketSize]...)
		errAcc += nulls
		for errAcc >= m {
			data = append(data, np...)
			errAcc -= m
		}
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

	return &LoopSegment{
		Data:          data,
		NumPackets:    n,
		LoopDurTicks:  plan.LoopDurTicks,
		VideoPID:      vpid,
		PCRPid:        pcrPid,
		AudioStartPTS: audioStart,
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
