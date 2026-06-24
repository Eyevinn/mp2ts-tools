package internal

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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
// stream's constant rate. Send times are absolute (start + idx*step), so jitter
// never accumulates into drift.
type udpSink struct {
	conn    net.Conn
	buf     []byte
	count   int
	startNs int64
	stepNs  int64
	idx     int64
}

func newUDPSink(addr string, loopDurTicks int64, numPackets int) (*udpSink, error) {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial udp %s: %w", addr, err)
	}
	stepNs := int64(0)
	if numPackets > 0 {
		stepNs = int64(float64(loopDurTicks)/90000.0/float64(numPackets)*1e9 + 0.5)
	}
	return &udpSink{
		conn:    conn,
		buf:     make([]byte, 0, tsPacketsPerDatagram*PacketSize),
		stepNs:  stepNs,
		startNs: time.Now().UnixNano(),
	}, nil
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
	target := u.startNs + u.idx*u.stepNs
	if d := target - time.Now().UnixNano(); d > 0 {
		time.Sleep(time.Duration(d))
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
// all PTS/DTS/PCR by the loop duration; continuity counters stay seamless across
// the seam. maxWraps <= 0 loops forever (until ctx is cancelled).
func LoopToSink(ctx context.Context, seg *LoopSegment, sink packetSink, maxWraps int) error {
	cc := make(ccGen)
	var pkt packet.Packet
	for wrap := 0; maxWraps <= 0 || wrap < maxWraps; wrap++ {
		offset := int64(wrap) * seg.LoopDurTicks
		for i := 0; i < seg.NumPackets; i++ {
			select {
			case <-ctx.Done():
				_ = sink.Flush()
				return nil
			default:
			}
			base := i * PacketSize
			copy(pkt[:], seg.Data[base:base+PacketSize])
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
