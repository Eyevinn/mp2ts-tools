package app_test

import (
	"bytes"
	"context"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/Eyevinn/mp2ts-tools/cmd/ts-info/app"
	"github.com/stretchr/testify/require"
)

var (
	update = flag.Bool("update", false, "update the golden files of this test")
)

func TestParseFile(t *testing.T) {
	cases := []struct {
		name                 string
		file                 string
		options              app.Options
		expected_output_file string
	}{
		{"avc_with_time", "../testdata/avc_with_time.ts", app.Options{MaxNrPictures: 10, ParameterSets: true}, "testdata/golden_avc_with_time.txt"},
		{"bbb_1s_no_nalu_no_sei", "testdata/bbb_1s.ts", app.Options{MaxNrPictures: 35}, "testdata/golden_bbb_1s_no_nalu(no_sei).txt"},
		{"bbb_1s_no_nalu", "testdata/bbb_1s.ts", app.Options{MaxNrPictures: 35, ShowSEI: true}, "testdata/golden_bbb_1s_no_nalu(no_sei).txt"},
		{"bbb_1s", "testdata/bbb_1s.ts", app.Options{MaxNrPictures: 35, ShowNALU: true, ShowSEI: true}, "testdata/golden_bbb_1s.txt"},
		{"bbb_1s_indented", "testdata/bbb_1s.ts", app.Options{MaxNrPictures: 2, ShowNALU: true, ShowSEI: true, Indent: true}, "testdata/golden_bbb_1s_indented.txt"},
		{"obs_h265_aac_no_nalu_no_sei", "testdata/obs_h265_aac.ts", app.Options{MaxNrPictures: 35}, "testdata/golden_obs_h265_aac_no_nalu(no_sei).txt"},
		{"obs_h265_aac_no_nalu", "testdata/obs_h265_aac.ts", app.Options{MaxNrPictures: 35, ShowSEI: true}, "testdata/golden_obs_h265_aac_no_nalu(no_sei).txt"},
		{"obs_h265_aac", "testdata/obs_h265_aac.ts", app.Options{MaxNrPictures: 35, ShowNALU: true, ShowSEI: true}, "testdata/golden_obs_h265_aac.txt"},
		{"obs_h265_aac_indented", "testdata/obs_h265_aac.ts", app.Options{MaxNrPictures: 2, ShowNALU: true, ShowSEI: true, Indent: true}, "testdata/golden_obs_h265_aac_indented.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := bytes.Buffer{}
			ctx := context.TODO()
			f, err := os.Open(c.file)
			require.NoError(t, err)
			err = app.Parse(ctx, &buf, f, c.options)
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
