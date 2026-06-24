package internal

import (
	"fmt"

	"github.com/Comcast/gots/v2"
	"github.com/Comcast/gots/v2/packet"
	"github.com/Comcast/gots/v2/packet/adaptationfield"
)

// ElStream is an elementary media stream that reassembles PES packets from TS
// packets and delivers each completed PES to its PESHandler.
type ElStream struct {
	pesHandler PESHandler
	currPES    *PESData
	nextPES    *PESData
	Language   string
	MediaType  string
	nrDiscInd  int64
	PID        int
	PktLosses  int
	Codec      Codec
	lastCC     byte
}

// NewElStream constructs a new elementary stream for the given pid and codec.
func NewElStream(pid int, codec Codec, language string) *ElStream {
	es := &ElStream{
		currPES:  &PESData{PID: pid},
		nextPES:  &PESData{PID: pid},
		PID:      pid,
		Codec:    codec,
		Language: language,
		lastCC:   255,
	}
	switch {
	case codec.IsVideo():
		es.MediaType = "video"
	case codec.IsAudio():
		es.MediaType = "audio"
	case codec == CODEC_TELETEXT || codec == CODEC_DVB_SUBTITLES:
		es.MediaType = "text"
	default:
		es.MediaType = "data"
	}
	return es
}

// SetPESHandler sets the handler called on every completed PES.
func (e *ElStream) SetPESHandler(pesHandler PESHandler) {
	e.pesHandler = pesHandler
}

// GetPESHandler returns the current PESHandler.
func (e *ElStream) GetPESHandler() PESHandler {
	return e.pesHandler
}

// Reset clears state so the stream can be parsed again.
func (e *ElStream) Reset() {
	e.currPES.Reset()
	e.nextPES.Reset()
	e.lastCC = 255 // avoid spurious packet-loss detection
}

// HandleLastPES delivers the final buffered PES, if any.
func (e *ElStream) HandleLastPES() error {
	if e.pesHandler != nil && e.currPES != nil {
		return e.pesHandler.HandlePES(e.currPES, true)
	}
	return nil
}

// IsVideo reports whether the media type is video.
func (e *ElStream) IsVideo() bool {
	return e.MediaType == "video"
}

// AddPacket adds a TS packet to the elementary stream. pktNr is propagated to
// the PES via StartPES.
func (e *ElStream) AddPacket(pkt *packet.Packet, pktNr uint32) error {
	cc := packet.ContinuityCounter(pkt)
	if e.lastCC != 255 {
		if !packet.ContainsPayload(pkt) {
			return nil
		}
		var discontinuityIndicator bool
		if packet.ContainsAdaptationField(pkt) && adaptationfield.Length(pkt) > 0 {
			discontinuityIndicator = adaptationfield.IsDiscontinuous(pkt)
			if discontinuityIndicator {
				e.nrDiscInd++
			}
		}
		if cc != (e.lastCC+1)&0xf {
			if e.currPES != nil && !discontinuityIndicator {
				e.currPES.PktLoss = true
				e.PktLosses++
			}
		}
	}
	if pkt.PayloadUnitStartIndicator() {
		if err := e.nextPES.StartPES(pkt, pktNr); err != nil {
			return fmt.Errorf("startPES: %w", err)
		}
		e.SwitchPES()
	} else if e.currPES.Size() != 0 {
		payload, err := pkt.Payload()
		if err != nil && err != gots.ErrNoPayload {
			return fmt.Errorf("pktPayload: %w", err)
		}
		e.currPES.Data = append(e.currPES.Data, payload...)
	}
	e.lastCC = cc
	return nil
}

// SwitchPES finishes the current PES (delivering it to the handler) and makes
// the freshly started PES current.
func (e *ElStream) SwitchPES() {
	if e.nextPES.Size() == 0 {
		return
	}
	if e.currPES.Size() != 0 && e.pesHandler != nil {
		if err := e.pesHandler.HandlePES(e.currPES, false); err != nil {
			panic(fmt.Sprintf("handler issue, pid %d: %s", e.PID, err))
		}
	}
	e.nextPES.AlignOffset = e.currPES.AlignOffset
	p := e.currPES
	p.Reset() // release payload
	e.currPES = e.nextPES
	e.nextPES = p
}
