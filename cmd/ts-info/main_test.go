package main

import (
	"bytes"
	"testing"

	"github.com/Eyevinn/mp2ts-tools/cmd/ts-info/app"
	"github.com/stretchr/testify/require"
)

var expected_avc_with_time_output = `{"pid":512,"codec":"AVC","type":"video"}
{"pid":512,"parameterSet":"SPS","nr":0,"hex":"27640020ac2b402802dd80880000030008000003032742001458000510edef7c1da1c32a","length":36}
{"pid":512,"parameterSet":"PPS","nr":0,"hex":"28ee3cb0","length":4}
{"pid":512,"rai":true,"pts":5508000,"nalus":[{"type":"AUD_9","len":2},{"type":"SPS_7","len":36},{"type":"PPS_8","len":4},{"type":"SEI_6","len":29,"data":"Type 1: 13:40:57:15 offset=0"},{"type":"IDR_5","len":2096}]}
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
