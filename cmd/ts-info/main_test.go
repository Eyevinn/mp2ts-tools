package main

import (
	"bytes"
	"testing"

	"github.com/Eyevinn/mp2ts-tools/cmd/ts-info/app"
	"github.com/stretchr/testify/require"
)

var expected_avc_with_time_output = `H264 video detected on PID: 512
PID 512, SPS 0 len 36B: 27640020ac2b402802dd80880000030008000003032742001458000510edef7c1da1c32a
PID 512, PPS 0 len 4B: 28ee3cb0
PID: 512, RAI: true, PTS: 5508000, NALUs: [AUD_9 2B][SPS_7 36B][PPS_8 4B][SEI_6 Type 1: 13:40:57:15 offset=0 29B][IDR_5 2096B]
`

func TestParseFile(t *testing.T) {
	o := app.Options{
		ParameterSets: false,
		MaxNrPictures: 0,
	}
	buf := bytes.Buffer{}
	err := run(&buf, o, "testdata/avc_with_time.ts")
	require.NoError(t, err)
	require.Equal(t, expected_avc_with_time_output, buf.String(), "testdata/avc_with_time.ts should produce expected output")
}
