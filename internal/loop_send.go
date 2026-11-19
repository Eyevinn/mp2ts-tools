package internal

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"
)

// tsPacketsPerDatagram is the number of TS packets per UDP datagram (7*188 =
// 1316 bytes, the standard non-fragmenting MPEG-TS-over-UDP payload).
const tsPacketsPerDatagram = 7

// pcrToNs converts 27 MHz ticks to nanoseconds without overflow for any run
// shorter than millennia.
func pcrToNs(t int64) int64 { return t/27*1000 + t%27*1000/27 }

// UDPOutput sends the laps over UDP, 7 packets per datagram, each datagram when
// its last packet is due by the output PCR, on the monotonic clock.
type UDPOutput struct {
	conn     net.Conn
	datagram []byte
	started  bool
	start    time.Time
	due      int64
	failed   bool
}

// NewUDPOutput opens a UDP output to addr (host:port, unicast or multicast),
// with ttl as the multicast TTL.
func NewUDPOutput(addr string, ttl int) (*UDPOutput, error) {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial udp %s: %w", addr, err)
	}
	if err := setMulticastTTL(conn, ttl); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &UDPOutput{conn: conn, datagram: make([]byte, 0, tsPacketsPerDatagram*PacketSize)}, nil
}

func (u *UDPOutput) Write(ctx context.Context, packets []byte, due func(int) int64) error {
	for i := 0; i < len(packets)/PacketSize; i++ {
		if !u.started {
			u.started, u.start = true, time.Now()
		}
		u.datagram = append(u.datagram, packetAt(packets, i)...)
		u.due = due(i)
		if len(u.datagram) == cap(u.datagram) {
			if err := u.send(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (u *UDPOutput) send(ctx context.Context) error {
	if len(u.datagram) == 0 {
		return nil
	}
	if wait := time.Until(u.start.Add(time.Duration(pcrToNs(u.due)))); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	// A missing receiver (ICMP port unreachable) must not stop a live stream.
	if _, err := u.conn.Write(u.datagram); err != nil && !u.failed {
		u.failed = true
		slog.Warn("udp write failed", "err", err)
	}
	u.datagram = u.datagram[:0]
	return nil
}

// Flush sends a datagram that is not full.
func (u *UDPOutput) Flush() error { return u.send(context.Background()) }

func (u *UDPOutput) Close() error { return u.conn.Close() }
