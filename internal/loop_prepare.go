package internal

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/Comcast/gots/v2"
	"github.com/Comcast/gots/v2/packet"
	"github.com/Comcast/gots/v2/packet/adaptationfield"
)

// audioSparePES is how many audio PES past the loop end are kept in the segment
// so the per-wrap drift controller can include the boundary-crossing PES. Two
// would suffice for the worst-case A/V offset; three is a safety margin.
const audioSparePES = 3

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
	audioPIDs := make(map[int]bool)
	for pid, es := range ts.ElStreams {
		switch es.MediaType {
		case "video":
			vpid = pid
		case "audio":
			audioPIDs[pid] = true
		}
	}
	pcrPid := ts.PCRPid
	if pcrPid < 0 {
		pcrPid = vpid
	}
	Vs, Ve := plan.StartPTS, plan.EndPTS
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
	content := make([]byte, 0, (endByte-startByte)*PacketSize)
	curAudioPTS := make(map[int]int64)
	audioPast := make(map[int]bool)
	sparePES := make(map[int]int) // audio PES kept at/after Ve (drift spares)

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

		keep := false
		switch {
		case pid == vpid:
			keep = pktNr < endByte
		case audioPIDs[pid]:
			// Keep audio whose PES is within [Vs, Ve), plus a few spare PES past
			// Ve so the per-wrap drift controller always has the boundary-
			// crossing PES available (spares are in PES units, which matters for
			// multi-frame-per-PES audio such as AC-3).
			if pkt.PayloadUnitStartIndicator() {
				if p := GetPTS(&pkt); p >= 0 {
					curAudioPTS[pid] = p
					if SignedPTSDiff(p, Ve) >= 0 {
						sparePES[pid]++
						if sparePES[pid] > audioSparePES {
							audioPast[pid] = true
						}
					}
				}
			}
			cur := curAudioPTS[pid]
			switch {
			case SignedPTSDiff(cur, Vs) < 0:
				keep = false
			case SignedPTSDiff(cur, Ve) < 0:
				keep = true
			default:
				keep = sparePES[pid] <= audioSparePES
			}
		default:
			keep = pktNr < endByte // PAT/PMT/pass-through PIDs
		}
		if keep {
			content = append(content, pkt[:]...)
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

	if patPkt == nil || pmtPkt == nil {
		return nil, fmt.Errorf("could not capture PAT/PMT packets")
	}

	// Assemble content with PAT/PMT prepended.
	seg := make([]byte, 0, len(content)+2*PacketSize)
	seg = append(seg, patPkt...)
	seg = append(seg, pmtPkt...)
	seg = append(seg, content...)
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

	// Regenerate a linear PCR across the segment spanning exactly LoopDurTicks.
	stampLinearPCR(data, plan.LoopDurTicks, n)

	audioStart := make(map[int]int64)
	for _, a := range plan.Audio {
		if a.StartPTS >= 0 {
			audioStart[a.PID] = a.StartPTS
		}
	}

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
