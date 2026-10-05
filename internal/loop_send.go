package internal

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/bits"
	"net"
	"time"

	"github.com/Comcast/gots/v2"
	"github.com/Comcast/gots/v2/packet"
	"github.com/Comcast/gots/v2/pes"
)

// tsPacketsPerDatagram is the number of TS packets per UDP datagram (7*188 =
// 1316 bytes, the standard non-fragmenting MPEG-TS-over-UDP payload).
const tsPacketsPerDatagram = 7

// ccGen generates gapless continuity counters per PID across the whole output,
// including across loop wraps (it is never reset). CC only advances on packets
// that carry a payload, per the MPEG-2 TS rule.
type ccGen map[int]int

func (g ccGen) next(pid int, hasPayload bool) int {
	last, ok := g[pid]
	if !ok {
		last = 15 // so the first payload packet becomes 0
	}
	if hasPayload {
		last = (last + 1) & 0xf
	}
	g[pid] = last
	return last
}

// rewriteTimestampsOffset shifts PTS (and DTS, if present) in a PES header by
// offset (90 kHz), fixing the marker nibbles. It is a no-op without a PTS.
func rewriteTimestampsOffset(pesHeaderBytes []byte, offset int64) {
	ph, err := pes.NewPESHeader(pesHeaderBytes)
	if err != nil || !ph.HasPTS() {
		return
	}
	pts := (int64(ph.PTS()) + offset) % PtsWrap
	if pts < 0 {
		pts += PtsWrap
	}
	gots.InsertPTS(pesHeaderBytes[9:14], uint64(pts))
	if ph.HasDTS() {
		pesHeaderBytes[9] = 0x30 | pesHeaderBytes[9]&0x0f
		dts := (int64(ph.DTS()) + offset) % PtsWrap
		if dts < 0 {
			dts += PtsWrap
		}
		gots.InsertPTS(pesHeaderBytes[14:19], uint64(dts))
		pesHeaderBytes[14] = 0x10 | pesHeaderBytes[14]&0x0f
	}
}

// applyWrap rewrites one packet in place for the given wrap offset and stamps a
// fresh continuity counter. Null packets are left untouched.
func (seg *LoopSegment) applyWrap(pkt *packet.Packet, offset int64, cc ccGen) {
	pid := packet.Pid(pkt)
	if pid == stuffingPID {
		return
	}
	if offset != 0 {
		if _, ok := packetPCR(pkt); ok {
			RewritePCR(pkt, uint64(offset))
		}
		if pkt.PayloadUnitStartIndicator() {
			if ph, err := packet.PESHeader(pkt); err == nil && ph != nil {
				rewriteTimestampsOffset(ph, offset)
			}
		}
	}
	c := cc.next(pid, packet.ContainsPayload(pkt))
	pkt[3] = (pkt[3] & 0xf0) | byte(c)
}

// packetSink consumes whole 188-byte TS packets.
type packetSink interface {
	WritePacket(pkt []byte) error
	Flush() error
	Close() error
}

// writerSink writes packets to an io.Writer as fast as possible (file output).
type writerSink struct{ w io.Writer }

func (s writerSink) WritePacket(p []byte) error { _, err := s.w.Write(p); return err }
func (s writerSink) Flush() error               { return nil }
func (s writerSink) Close() error               { return nil }

// multiSink fans packets out to several sinks.
type multiSink []packetSink

func (m multiSink) WritePacket(p []byte) error {
	for _, s := range m {
		if err := s.WritePacket(p); err != nil {
			return err
		}
	}
	return nil
}

func (m multiSink) Flush() error {
	for _, s := range m {
		if err := s.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func (m multiSink) Close() error {
	var err error
	for _, s := range m {
		if e := s.Close(); e != nil {
			err = e
		}
	}
	return err
}

// udpSink batches TS packets into 1316-byte datagrams and paces them at the
// stream's constant rate: loopDurTicks per numPackets, the same rate the PCR is
// stamped with. Each send time is computed exactly from the packet index (no
// rounded per-packet step), and measured on the monotonic clock, so neither
// rounding nor sleep jitter accumulates and wall-clock steps have no effect.
type udpSink struct {
	conn         net.Conn
	buf          []byte
	count        int
	start        time.Time // carries a monotonic clock reading
	loopDurTicks int64
	numPackets   int64
	idx          int64
}

func newUDPSink(addr string, loopDurTicks int64, numPackets int) (*udpSink, error) {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial udp %s: %w", addr, err)
	}
	return &udpSink{
		conn:         conn,
		buf:          make([]byte, 0, tsPacketsPerDatagram*PacketSize),
		start:        time.Now(),
		loopDurTicks: loopDurTicks,
		numPackets:   int64(numPackets),
	}, nil
}

// paceOffsetNs returns when packet idx is due, in ns after the start, at a rate
// of loopDurTicks (90 kHz) per numPackets packets: idx*loopDurTicks*1e9 /
// (90000*numPackets), rounded down. The product is formed in 128 bits, so it is
// exact for any realistic run length. It returns 0 (send at once) if
// numPackets is not positive.
func paceOffsetNs(idx, loopDurTicks, numPackets int64) int64 {
	if numPackets <= 0 || idx <= 0 || loopDurTicks <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(idx), uint64(loopDurTicks)*100_000) // 1e9/90000 = 100000/9
	q, _ := bits.Div64(hi, lo, 9*uint64(numPackets))
	return int64(q)
}

func (u *udpSink) WritePacket(p []byte) error {
	u.buf = append(u.buf, p...)
	u.count++
	u.idx++
	if u.count == tsPacketsPerDatagram {
		return u.send()
	}
	return nil
}

func (u *udpSink) send() error {
	if u.count == 0 {
		return nil
	}
	due := time.Duration(paceOffsetNs(u.idx, u.loopDurTicks, u.numPackets))
	if d := due - time.Since(u.start); d > 0 {
		time.Sleep(d)
	}
	// UDP write errors (e.g. ICMP "connection refused" when no receiver is
	// listening) must not stop a live stream; log and keep going.
	if _, err := u.conn.Write(u.buf); err != nil {
		slog.Warn("udp write failed", "err", err)
	}
	u.buf = u.buf[:0]
	u.count = 0
	return nil
}

func (u *udpSink) Flush() error { return u.send() }
func (u *udpSink) Close() error {
	_ = u.send()
	return u.conn.Close()
}

// LoopToSink emits the prepared segment to a sink repeatedly. Each wrap advances
// video/PCR by the loop duration; audio is advanced by the loop duration plus a
// per-wrap drift-free deltaPTS, dropping (nulling) the frame that crosses the
// loop boundary so the audio cadence stays continuous without accumulating
// drift. Continuity counters stay seamless across the seam. maxWraps <= 0 loops
// forever (until ctx is cancelled).
func LoopToSink(ctx context.Context, seg *LoopSegment, sink packetSink, maxWraps int) error {
	cc := make(ccGen)
	audio := make(map[int]*audioWrapState, len(seg.AudioStartPTS))
	for pid, start := range seg.AudioStartPTS {
		audio[pid] = newAudioWrapState(start, seg.LoopDurTicks)
	}
	keepPES := make(map[int]bool)   // current audio PES kept this wrap?
	audioOff := make(map[int]int64) // current audio PES output offset
	null := nullPacket()

	var pkt packet.Packet
	for wrap := 0; maxWraps <= 0 || wrap < maxWraps; wrap++ {
		offset := int64(wrap) * seg.LoopDurTicks
		for _, a := range audio {
			a.onWrap()
		}
		var scte map[int][]byte // this wrap's SCTE-35 packets; wrap 0 is the segment as is
		if seg.scte35 != nil && wrap > 0 {
			scte = seg.scte35.packets(seg.Data, wrap, seg.LoopDurTicks)
		}
		for i := 0; i < seg.NumPackets; i++ {
			select {
			case <-ctx.Done():
				_ = sink.Flush()
				return nil
			default:
			}
			base := i * PacketSize
			if p, ok := scte[i]; ok {
				copy(pkt[:], p)
			} else {
				copy(pkt[:], seg.Data[base:base+PacketSize])
			}
			pid := packet.Pid(&pkt)

			if st, isAudio := audio[pid]; isAudio {
				if pkt.PayloadUnitStartIndicator() {
					if p := GetPTS(&pkt); p >= 0 {
						d, keep := st.frame(p)
						keepPES[pid] = keep
						audioOff[pid] = offset + d
					}
				}
				if !keepPES[pid] {
					if err := sink.WritePacket(null); err != nil {
						return err
					}
					continue
				}
				if ao := audioOff[pid]; ao != 0 && pkt.PayloadUnitStartIndicator() {
					if ph, err := packet.PESHeader(&pkt); err == nil && ph != nil {
						rewriteTimestampsOffset(ph, ao)
					}
				}
				c := cc.next(pid, packet.ContainsPayload(&pkt))
				pkt[3] = (pkt[3] & 0xf0) | byte(c)
				if err := sink.WritePacket(pkt[:]); err != nil {
					return err
				}
				continue
			}

			seg.applyWrap(&pkt, offset, cc)
			if err := sink.WritePacket(pkt[:]); err != nil {
				return err
			}
		}
	}
	return sink.Flush()
}

// LoopToWriter emits the segment maxWraps times to a single io.Writer (no
// pacing). Used for file output and for writing the prepared segment.
func LoopToWriter(ctx context.Context, seg *LoopSegment, w io.Writer, maxWraps int) error {
	return LoopToSink(ctx, seg, writerSink{w: w}, maxWraps)
}

// LoopToOutputs emits the segment to a file writer and/or a UDP address. When a
// UDP address is given the output is paced at the stream's constant rate; a file
// written alongside is captured at that same real-time rate.
func LoopToOutputs(ctx context.Context, seg *LoopSegment, fileW io.Writer, udpAddr string, maxWraps int) error {
	var sinks multiSink
	if fileW != nil {
		sinks = append(sinks, writerSink{w: fileW})
	}
	if udpAddr != "" {
		u, err := newUDPSink(udpAddr, seg.LoopDurTicks, seg.NumPackets)
		if err != nil {
			return err
		}
		defer func() { _ = u.Close() }()
		sinks = append(sinks, u)
	}
	if len(sinks) == 0 {
		return fmt.Errorf("no outputs selected")
	}
	return LoopToSink(ctx, seg, sinks, maxWraps)
}
