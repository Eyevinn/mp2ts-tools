package internal

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Comcast/gots/v2"
	"github.com/Comcast/gots/v2/packet"
	"github.com/Comcast/gots/v2/packet/adaptationfield"
)

// movePCRToPID copies a constant-rate fixture with every PCR moved off the PID
// that carries it into a PCR-only packet on pcrPID (in the next null packet, its
// value advanced by the packets moved over; dropped if there is none), and the
// PMT's PCR_PID set to pcrPID.
func movePCRToPID(t *testing.T, src string, pcrPID int) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	n := len(data) / PacketSize
	pkt := func(i int) *packet.Packet { return (*packet.Packet)(data[i*PacketSize : (i+1)*PacketSize]) }
	type sample struct {
		i   int
		pcr int64
	}
	var pcrs []sample
	for i := 0; i < n; i++ {
		if pcr, ok := packetPCR(pkt(i)); ok {
			pcrs = append(pcrs, sample{i, pcr})
		}
	}
	perPkt := float64(pcrs[len(pcrs)-1].pcr-pcrs[0].pcr) / float64(pcrs[len(pcrs)-1].i-pcrs[0].i)
	pmtPID := -1
	for i := 0; i < n && pmtPID < 0; i++ {
		if b := data[i*PacketSize:]; tsPID(b) == 0 && tsPUSI(b) {
			sec := b[5+int(b[4]):]
			pmtPID = int(sec[10]&0x1f)<<8 | int(sec[11])
		}
	}
	for _, s := range pcrs {
		b := data[s.i*PacketSize : (s.i+1)*PacketSize]
		b[5] &^= 0x10 // PCR_flag off; the PCR bytes become stuffing
		for k := 6; k < 12; k++ {
			b[k] = 0xff
		}
		j := s.i + 1
		for j < n && tsPID(data[j*PacketSize:]) != stuffingPID {
			j++
		}
		if j == n {
			continue
		}
		p := data[j*PacketSize : (j+1)*PacketSize]
		for k := range p {
			p[k] = 0xff
		}
		p[0], p[1], p[2], p[3] = 0x47, byte(pcrPID>>8), byte(pcrPID), 0x20 // adaptation field only
		p[4], p[5] = PacketSize-5, 0x10                                    // PCR_flag
		pcrBytes, err := adaptationfield.PCR(pkt(j))
		if err != nil {
			t.Fatal(err)
		}
		gots.InsertPCR(pcrBytes, uint64(s.pcr+int64(math.Round(float64(j-s.i)*perPkt))))
	}
	for i := 0; i < n; i++ {
		b := data[i*PacketSize : (i+1)*PacketSize]
		if tsPID(b) != pmtPID || !tsPUSI(b) {
			continue
		}
		o := tsPayloadOffset(b)
		o += 1 + int(b[o]) // pointer_field
		end := o + 3 + (int(b[o+1]&0x0f)<<8 | int(b[o+2])) - 4
		b[o+8], b[o+9] = b[o+8]&0xe0|byte(pcrPID>>8), byte(pcrPID)
		copy(b[end:], gots.ComputeCRC(b[o:end]))
	}
	path := filepath.Join(t.TempDir(), "pcrpid.ts")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoopSeparatePCRPID loops a stream whose PCR is carried on its own PID in
// adaptation-field-only packets: the plan follows the PMT's PCR_PID, PCR is only
// on that PID, its continuity counter does not move (no payload), and the output
// PCR is linear across the wraps.
func TestLoopSeparatePCRPID(t *testing.T) {
	const pcrPID = 0x1f0
	path := movePCRToPID(t, "testdata/avc_aac_cbr.ts", pcrPID)
	seg, plan, err := PrepareLoop(context.TODO(), path, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PCR == nil || plan.PCR.PID != pcrPID || !plan.PCR.ConstantRate {
		t.Fatalf("PCR plan: %+v", plan.PCR)
	}
	var out bytes.Buffer
	const wraps = 3
	if err := LoopToWriter(context.TODO(), seg, &out, wraps); err != nil {
		t.Fatal(err)
	}
	b := out.Bytes()
	ticksPerPkt := float64(seg.LoopDurTicks*300) / float64(seg.NumPackets)
	first, prev, prevPCR, count := -1, 0, int64(0), 0
	cc := -1
	for i := 0; i < len(b)/PacketSize; i++ {
		p := (*packet.Packet)(b[i*PacketSize : (i+1)*PacketSize])
		pcr, ok := packetPCR(p)
		if !ok {
			continue
		}
		if pid := packet.Pid(p); pid != pcrPID {
			t.Fatalf("packet %d: PCR on PID %d", i, pid)
		}
		if c := int(packet.ContinuityCounter(p)); cc >= 0 && c != cc {
			t.Errorf("packet %d: CC %d on a payload-less PCR packet, want %d", i, c, cc)
		} else {
			cc = c
		}
		if first >= 0 {
			want := float64(i-prev) * ticksPerPkt
			if d := float64(pcr-prevPCR) - want; math.Abs(d) > 1 {
				t.Errorf("packet %d: PCR step %d, want %.1f (linear)", i, pcr-prevPCR, want)
			}
		} else {
			first = i
		}
		prev, prevPCR = i, pcr
		count++
	}
	if count < wraps*10 {
		t.Errorf("only %d PCRs in %d wraps", count, wraps)
	}
}
