package internal

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestScanReport golden-tests the `mp2ts-loop -scan` report for the small bundled
// fixtures: AVC and HEVC IDR loops, a longer AVC stream, a stream with too few
// loop points to loop, and an open-GOP MPEG-2 stream. Regenerate with: go test ./internal/... -run TestScanReport -update
func TestScanReport(t *testing.T) {
	cases := []struct {
		name   string
		file   string
		golden string
	}{
		{"avc_idr", "testdata/bbb_1s.ts", "testdata/golden_scan_bbb_1s.json"},
		{"hevc_idr", "testdata/obs_hevc_aac.ts", "testdata/golden_scan_obs_hevc_aac.json"},
		{"avc_long", "testdata/80s_with_ad.ts", "testdata/golden_scan_80s_with_ad.json"},
		{"no_loop", "testdata/avc_with_time.ts", "testdata/golden_scan_avc_with_time.json"},
		{"mpeg2_open_gop", "testdata/mpeg2_open_mp2.ts", "testdata/golden_scan_mpeg2_open_mp2.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep, err := Scan(context.TODO(), c.file, 0, 0, 0)
			require.NoError(t, err)
			b, err := json.MarshalIndent(rep, "", "  ")
			require.NoError(t, err)
			compareUpdateGolden(t, string(b)+"\n", c.golden, *update)
		})
	}
}
