package internal

import (
	"fmt"

	"github.com/Comcast/gots/v2/packet"
	"github.com/Comcast/gots/v2/packet/adaptationfield"
	"github.com/Comcast/gots/v2/pes"
)

// PESHandler handles a complete PES as it is assembled by an ElStream.
type PESHandler interface {
	HandlePES(pes *PESData, last bool) error
}

// PESData accumulates the bytes and metadata of a single PES packet.
type PESData struct {
	PID           int
	PTS           int64
	DTS           int64
	HasPTS        bool // the PES header carries a PTS (PTS and DTS are 0 otherwise)
	PayloadLength int  // from the PES_packet_length header field (0 if unbounded)
	AlignOffset   int  // bytes at the start belonging to a frame begun in the previous PES
	Data          []byte
	StartPktNr    uint32
	PktLoss       bool
	IDR           bool
	RAI           bool // random_access_indicator on the first TS packet
}

// Size returns the length of payload data accumulated so far.
func (p *PESData) Size() int {
	return len(p.Data)
}

// StartPES initializes the PESData from the first (PUSI) packet of a PES.
func (p *PESData) StartPES(pkt *packet.Packet, pktNr uint32) error {
	if p.Size() != 0 {
		return fmt.Errorf("non-empty PESData")
	}
	pesHeaderBytes, err := packet.PESHeader(pkt)
	if err != nil {
		return err
	}
	if pesHeaderBytes == nil {
		return fmt.Errorf("no pesHeaderBytes")
	}
	p.StartPktNr = pktNr
	if packet.ContainsAdaptationField(pkt) && adaptationfield.Length(pkt) > 0 {
		p.RAI = adaptationfield.IsRandomAccess(pkt)
	}
	pesHdr, err := pes.NewPESHeader(pesHeaderBytes)
	if err != nil {
		return fmt.Errorf("newPesHeader: %w", err)
	}
	pesPacketLength := PESPktLen(pesHeaderBytes)
	if pesPacketLength > 0 {
		// The 6 comes from where PES_packet_length is counted; the 9 from where
		// PES_header_data_length is counted. See ISO/IEC 13818-1 2018 Sec. 2.4.3.6.
		totPESLen := pesPacketLength + 6
		totPESHdrLen := 9 + PESHdrDataLen(pesHeaderBytes)
		p.PayloadLength = totPESLen - totPESHdrLen
	}
	if pesHdr.HasPTS() {
		p.HasPTS = true
		p.PTS = int64(pesHdr.PTS())
		if pesHdr.HasDTS() {
			p.DTS = int64(pesHdr.DTS())
		} else {
			p.DTS = p.PTS
		}
	}
	p.Data = append(p.Data, pesHdr.Data()...)
	return nil
}

// Reset clears PESData while keeping the allocated data slice.
func (p *PESData) Reset() {
	if p == nil {
		return
	}
	p.PTS = 0
	p.DTS = 0
	p.HasPTS = false
	p.PayloadLength = 0
	p.AlignOffset = 0
	p.Data = p.Data[:0]
	p.StartPktNr = 0
	p.PktLoss = false
	p.IDR = false
	p.RAI = false
}

// PESHdrLen returns the total length of the PES header in pkt.
func PESHdrLen(pkt *packet.Packet) int {
	pesHeaderBytes, err := packet.PESHeader(pkt)
	if err != nil || len(pesHeaderBytes) == 0 {
		return 0
	}
	// PESHdrDataLen returns the length counted after 9 bytes.
	return PESHdrDataLen(pesHeaderBytes) + 9
}

// PESPktLen extracts the PES_packet_length from PES header bytes.
func PESPktLen(pesHeaderBytes []byte) int {
	return int(pesHeaderBytes[4])<<8 | int(pesHeaderBytes[5])
}

// PESHdrDataLen extracts the PES_header_data_length from PES header bytes. The
// total header length is 9 bytes more.
func PESHdrDataLen(pesHeaderBytes []byte) int {
	return int(pesHeaderBytes[8])
}

// SetPESPktLen sets the PES_packet_length in PES header bytes.
func SetPESPktLen(pesHeaderBytes []byte, length uint16) {
	pesHeaderBytes[4] = byte(length >> 8)
	pesHeaderBytes[5] = byte(length & 0xff)
}

// GetPTS returns the PTS of a PES-start packet, or -1 if absent.
func GetPTS(pkt *packet.Packet) int64 {
	pesHeaderBytes, err := packet.PESHeader(pkt)
	if err != nil {
		return -1
	}
	pesHeader, err := pes.NewPESHeader(pesHeaderBytes)
	if err != nil {
		return -1
	}
	if !pesHeader.HasPTS() {
		return -1
	}
	return int64(pesHeader.PTS())
}
