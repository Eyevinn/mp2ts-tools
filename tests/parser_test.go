package tests

import (
	"bytes"
	"context"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/Eyevinn/mp2ts-tools/avc"
	"github.com/Eyevinn/mp2ts-tools/common"
	"github.com/stretchr/testify/require"
)

var (
	update = flag.Bool("update", false, "update the golden files of this test")
)

func TestParseFile(t *testing.T) {
	fullOptionsWith2Pic := common.CreateFullOptions(2)
	fullOptionsWith2Pic.Indent = true
	fullOptionsWith35Pic := common.CreateFullOptions(35)
	fullOptionsWith35PicWithoutNALUSEI := common.CreateFullOptions(35)
	fullOptionsWith35PicWithoutNALUSEI.ShowNALU = false
	fullOptionsWith35PicWithoutNALUSEI.ShowSEI = false

	cases := []struct {
		name                 string
		file                 string
		options              common.Options
		expected_output_file string
	}{
		{"avc_with_time", "testdata/avc_with_time.ts", common.Options{MaxNrPictures: 10, ShowStreamInfo: true, ShowPS: true, ShowStatistics: true}, "testdata/golden_avc_with_time.txt"},
		{"bbb_1s_no_nalu_no_sei", "testdata/bbb_1s.ts", fullOptionsWith35PicWithoutNALUSEI, "testdata/golden_bbb_1s_no_nalu(no_sei).txt"},
		{"bbb_1s", "testdata/bbb_1s.ts", fullOptionsWith35Pic, "testdata/golden_bbb_1s.txt"},
		{"bbb_1s_indented", "testdata/bbb_1s.ts", fullOptionsWith2Pic, "testdata/golden_bbb_1s_indented.txt"},
		{"obs_h265_aac_no_nalu_no_sei", "testdata/obs_h265_aac.ts", fullOptionsWith35PicWithoutNALUSEI, "testdata/golden_obs_h265_aac_no_nalu(no_sei).txt"},
		{"obs_h265_aac", "testdata/obs_h265_aac.ts", fullOptionsWith35Pic, "testdata/golden_obs_h265_aac.txt"},
		{"obs_h265_aac_indented", "testdata/obs_h265_aac.ts", fullOptionsWith2Pic, "testdata/golden_obs_h265_aac_indented.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := bytes.Buffer{}
			ctx := context.TODO()
			f, err := os.Open(c.file)
			require.NoError(t, err)
			err = avc.ParseAll(ctx, &buf, f, c.options)
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
