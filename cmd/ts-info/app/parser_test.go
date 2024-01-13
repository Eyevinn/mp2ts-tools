package app_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Eyevinn/mp2ts-tools/cmd/ts-info/app"
	"github.com/stretchr/testify/require"
)

func TestParseFile(t *testing.T) {
	cases := []struct {
		name                 string
		file                 string
		options              app.Options
		expected_output_file string
	}{
		{"avc_with_time", "../testdata/avc_with_time.ts", app.Options{ParameterSets: true}, "testdata/golden_avc_with_time.txt"},
		{"bbb_1s", "testdata/bbb_1s.ts", app.Options{MaxNrPictures: 15}, "testdata/golden_bbb_1s.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := bytes.Buffer{}
			ctx := context.TODO()
			f, err := os.Open(c.file)
			require.NoError(t, err)
			err = app.Parse(ctx, &buf, f, c.options)
			require.NoError(t, err)
			expected_output := getExpectedOutput(t, c.expected_output_file)
			require.Equal(t, expected_output, buf.String(), "should produce expected output")
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
