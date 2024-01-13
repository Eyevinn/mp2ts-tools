package app

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"

	"github.com/Eyevinn/mp4ff/avc"
)

type avcPS struct {
	spss     map[uint32]*avc.SPS
	ppss     map[uint32]*avc.PPS
	spsnalu  []byte
	ppsnalus [][]byte
}

func (a *avcPS) getSPS() *avc.SPS {
	return a.spss[0]
}

func (a *avcPS) setSPS(nalu []byte) error {
	if a.spss == nil {
		a.spss = make(map[uint32]*avc.SPS, 1)
		a.ppss = make(map[uint32]*avc.PPS, 1)
		a.ppsnalus = make([][]byte, 1)
	}
	sps, err := avc.ParseSPSNALUnit(nalu, true)
	if err != nil {
		return err
	}
	a.spsnalu = nalu
	a.spss[sps.ParameterID] = sps
	if len(a.spss) > 1 {
		return fmt.Errorf("more than one SPS")
	}
	return nil
}

func (a *avcPS) setPPS(nalu []byte) error {
	pps, err := avc.ParsePPSNALUnit(nalu, a.spss)
	if err != nil {
		return err
	}
	a.ppss[pps.PicParameterSetID] = pps
	a.ppsnalus[pps.PicParameterSetID] = nalu
	return nil
}

func printPS(w io.Writer, name string, nr uint32, ps []byte, psInfo any, verbose bool) {
	hexStr := hex.EncodeToString(ps)
	length := len(hexStr) / 2
	fmt.Fprintf(w, "%s %d len %dB: %+v\n", name, nr, length, hexStr)
	if verbose && psInfo != nil {
		jsonPS, err := json.MarshalIndent(psInfo, "", "  ")
		if err != nil {
			log.Fatalf(err.Error())
		}
		fmt.Fprintf(w, "%s\n", string(jsonPS))
	}
}
