package internal

import (
	"bytes"
	"fmt"
	"io"
	"log"

	"github.com/Eyevinn/mp4ff/bits"
	"github.com/asticode/go-astits"
)

// didMap maps SMPTE-20348 [did] values registered by SMPTE
// [dids]: https://smpte-ra.org/smpte-ancillary-data-smpte-st-291
type SMPTE291Identifier struct {
	did, sdid byte
}

var SMPTE291Map = map[SMPTE291Identifier]string{
	{0x41, 0x05}: "AFD and Bar Data",
	{0x41, 0x06}: "Pan and Scan Information",
	{0x41, 0x07}: "ANSI/SCTE 104 messages",
	{0x41, 0x08}: "DVB/SCTE VBI data",
	{0x43, 0x01}: "Inter Station Control Packet Data",
	{0x43, 0x02}: "OP-47 SDP - Subtitling Data",
	{0x43, 0x03}: "OP-47 Multi Packet ANC Data",
	{0x45, 0x01}: "Audio Metadata (Single Program)",
	{0x45, 0x02}: "Audio Metadata Ch 01/02",
	{0x45, 0x03}: "Audio Metadata Ch 03/04",
	{0x45, 0x04}: "Audio Metadata Ch 05/06",
	{0x45, 0x05}: "Audio Metadata Ch 07/08",
	{0x45, 0x06}: "Audio Metadata Ch 09/10",
	{0x45, 0x07}: "Audio Metadata Ch 11/12",
	{0x45, 0x08}: "Audio Metadata Ch 13/14",
	{0x45, 0x09}: "Audio Metadata Ch 15/16",
	{0x50, 0x01}: "WSS - Wide Screen Signaling",
	{0x50, 0x30}: "User Defined",
	{0x50, 0x49}: "User Defined",
	{0x60, 0x60}: "ATC - Ancillary Timecode",
	{0x61, 0x01}: "EIA 708B Data mapping into VANC space",
	{0x61, 0x02}: "EIA 608 Data mapping into VANC space",
	{0x62, 0x01}: "DTV Program Description",
	{0x62, 0x02}: "DTV Data Broadcast",
	{0x62, 0x03}: "VBI Data",
}

type smpte2038Data struct {
	PID     uint16 `json:"pid"`
	PTS     int64  `json:"pts"`
	Entries []smpte2038Entry
}

type smpte2038Entry struct {
	LineNr    byte   `json:"lineNr"`
	HorOffset byte   `json:"horOffset"`
	DID       byte   `json:"did"`
	SDID      byte   `json:"sdid"`
	DataCount byte   `json:"dataCount"`
	Type      string `json:"type"`
}

func ParseSMPTE2038(jp *JsonPrinter, d *astits.DemuxerData, o Options) {
	pl := d.PES.Data
	pdtDtsIndicator := d.PES.Header.OptionalHeader.PTSDTSIndicator
	if pdtDtsIndicator != 2 {
		fmt.Printf("SMPTE-2038: invalid PDT_DTS_Indicator=%d\n", pdtDtsIndicator)
	}
	pts := d.PES.Header.OptionalHeader.PTS
	rd := bytes.NewBuffer(pl)
	r := bits.NewReader(rd)
	smpteData := smpte2038Data{PID: d.PID, PTS: pts.Base}
	for {
		z := r.Read(6)
		if r.AccError() == io.EOF {
			break
		}
		if z == 0xffffffffffff {
			z2 := r.Read(2)
			if z2 != 0x3 {
				log.Printf("SMPTE-2038: invalid stuffing\n")
				return
			}
			_ = r.ReadRemainingBytes()
		}
		if z != 0 {
			log.Printf("SMPTE-2038: reserved bits not zero %x\n", z)
			return
		}
		_ = r.Read(1) // cNotYChFlag
		lineNr := r.Read(11)
		horOffset := r.Read(12)
		did := r.Read(10)
		did = did & 0xff // 8 bits
		sdid := r.Read(10)
		sdid = sdid & 0xff // 8 bits
		didStr := SMPTE291Map[SMPTE291Identifier{byte(did), byte(sdid)}]
		if didStr == "" {
			didStr = "unknown SID/DID"
		}
		dataCount := int(r.Read(10)) & 0xff // 8 bits
		for j := 0; j < dataCount; j++ {
			_ = r.Read(10)
		}
		_ = r.Read(10) // checkSumWord
		if r.NrBitsReadInCurrentByte() != 8 {
			_ = r.Read(8 - r.NrBitsReadInCurrentByte())
		}
		if r.AccError() != nil {
			fmt.Printf("SMPTE-2038: read error\n")
			return
		}
		smpteData.Entries = append(smpteData.Entries, smpte2038Entry{
			LineNr:    byte(lineNr),
			HorOffset: byte(horOffset),
			DID:       byte(did),
			SDID:      byte(sdid),
			DataCount: byte(dataCount),
			Type:      didStr,
		})
	}
	if jp != nil {
		jp.Print(smpteData, true)
	}
}
