package internal

import (
	"bytes"
	"context"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	update = flag.Bool("update", false, "update the golden files of this test")
)

func TestParseFile(t *testing.T) {
	fullOptionsWith2Pic := CreateFullOptions(2)
	fullOptionsWith2Pic.Indent = true
	fullOptionsWith35Pic := CreateFullOptions(35)
	fullOptionsWith35PicWithoutNALUSEI := CreateFullOptions(35)
	fullOptionsWith35PicWithoutNALUSEI.ShowNALU = false
	fullOptionsWith35PicWithoutNALUSEI.ShowSEIDetails = false

	parseInfoFunc := ParseInfo
	parseInfoAndSCTE35Func := ParseInfoAndSCTE35
	parseAllFunc := ParseAll

	cases := []struct {
		name                 string
		file                 string
		options              Options
		expected_output_file string
		f                    RunableFunc
	}{
		{"avc", "testdata/avc_with_time.ts", Options{MaxNrPictures: 10, Indent: true, ShowStreamInfo: true, ShowPS: true, ShowStatistics: true}, "testdata/golden_avc.txt", parseAllFunc},
		{"avc_without_ps", "testdata/avc_with_time.ts", Options{MaxNrPictures: 10, ShowStreamInfo: true}, "testdata/golden_avc_without_ps.txt", parseInfoFunc},
		{"avc_with_scte35", "testdata/80s_with_ad.ts", Options{MaxNrPictures: 0, ShowStreamInfo: true, ShowService: true, ShowSCTE35: true}, "testdata/golden_avc_with_scte35.txt", parseInfoAndSCTE35Func},
		{"bbb_1s", "testdata/bbb_1s.ts", fullOptionsWith35Pic, "testdata/golden_bbb_1s.txt", parseAllFunc},
		{"bbb_1s_indented", "testdata/bbb_1s.ts", fullOptionsWith2Pic, "testdata/golden_bbb_1s_indented.txt", parseAllFunc},
		{"bbb_1s_no_nalu_no_sei", "testdata/bbb_1s.ts", fullOptionsWith35PicWithoutNALUSEI, "testdata/golden_bbb_1s_no_nalu(no_sei).txt", parseAllFunc},
		{"obs_hevc_aac", "testdata/obs_hevc_aac.ts", fullOptionsWith35Pic, "testdata/golden_obs_hevc_aac.txt", parseAllFunc},
		{"obs_hevc_aac_indented", "testdata/obs_hevc_aac.ts", fullOptionsWith2Pic, "testdata/golden_obs_hevc_aac_indented.txt", parseAllFunc},
		{"obs_hevc_aac_no_nalu_no_sei", "testdata/obs_hevc_aac.ts", fullOptionsWith35PicWithoutNALUSEI, "testdata/golden_obs_hevc_aac_no_nalu(no_sei).txt", parseAllFunc},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := bytes.Buffer{}
			ctx := context.TODO()
			f, err := os.Open(c.file)
			require.NoError(t, err)
			err = c.f(ctx, &buf, f, c.options)
			require.NoError(t, err)
			compareUpdateGolden(t, buf.String(), c.expected_output_file, *update)
		})
	}
}

func getExpectedOutput(t *testing.T, file string) string {
	t.Helper()
	expected_output, err := os.ReadFile(file)
	require.NoError(t, err)
	expected_output_str := strings.ReplaceAll(string(expected_output), "\r\n", "\n")
	return expected_output_str
}

func compareUpdateGolden(t *testing.T, actual string, goldenFile string, update bool) {
	t.Helper()
	if update {
		err := os.WriteFile(goldenFile, []byte(actual), 0644)
		require.NoError(t, err)
	} else {
		expected := getExpectedOutput(t, goldenFile)
		require.Equal(t, expected, actual, "should produce expected output")
	}
}

// TestMain is to set flags for tests. In particular, the update flag to update golden files.
func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(m.Run())
}
