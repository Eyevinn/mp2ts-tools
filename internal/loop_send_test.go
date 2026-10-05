package internal

import (
	"math/big"
	"testing"
)

// TestPaceOffsetNs checks the UDP send schedule: exact at every whole wrap (so
// pacing never drifts from the PCR rate), equal to exact rational math in
// between, and never decreasing.
func TestPaceOffsetNs(t *testing.T) {
	cases := []struct {
		name         string
		loopDurTicks int64
		numPackets   int64
	}{
		{"5 Mbps, 29 s", 2_610_000, 96_410},
		{"22 Mbps, 28.16 s", 2_534_400, 411_915},
		{"27 Mbps, 27 s", 2_430_000, 486_503},
		{"duration not a multiple of 9 ticks", 2_477_475, 499_668},
	}
	for _, c := range cases {
		// Whole wraps, up to about a year of playout.
		for _, wraps := range []int64{1, 128, 1_000_000} {
			got := paceOffsetNs(wraps*c.numPackets, c.loopDurTicks, c.numPackets)
			want := new(big.Int).Div(big.NewInt(wraps*c.loopDurTicks*100_000), big.NewInt(9))
			if got != want.Int64() {
				t.Errorf("%s: after %d wraps got %d ns, want %s", c.name, wraps, got, want)
			}
		}
		// Arbitrary packet indexes match floor(idx*dur*1e9 / (90000*n)).
		prev := int64(-1)
		for _, idx := range []int64{0, 1, 2, 7, 1000, c.numPackets - 1, c.numPackets + 1, 123_456_789_012} {
			want := new(big.Int).Mul(big.NewInt(idx), big.NewInt(c.loopDurTicks))
			want.Mul(want, big.NewInt(1_000_000_000))
			want.Div(want, big.NewInt(90_000*c.numPackets))
			got := paceOffsetNs(idx, c.loopDurTicks, c.numPackets)
			if got != want.Int64() {
				t.Errorf("%s: idx %d got %d ns, want %s", c.name, idx, got, want)
			}
			if got < prev {
				t.Errorf("%s: idx %d went backwards: %d < %d", c.name, idx, got, prev)
			}
			prev = got
		}
	}
	if got := paceOffsetNs(100, 2_610_000, 0); got != 0 {
		t.Errorf("no packets: got %d, want 0", got)
	}
}
