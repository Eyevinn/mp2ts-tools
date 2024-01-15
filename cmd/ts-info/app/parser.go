package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/asticode/go-astits"
)

type Options struct {
	MaxNrPictures int
	ParameterSets bool
	Version       bool
	Indent        bool
}

const (
	packetSize = 188
)

type elementaryStream struct {
	PID   uint16 `json:"pid"`
	Codec string `json:"codec"`
	Type  string `json:"type"`
}

type jsonPrinter struct {
	w        io.Writer
	indent   bool
	accError error
}

func (p *jsonPrinter) print(data any) {
	var out []byte
	var err error
	if p.accError != nil {
		return
	}
	if p.indent {
		out, err = json.MarshalIndent(data, "", "  ")
	} else {
		out, err = json.Marshal(data)
	}
	if err != nil {
		p.accError = err
		return
	}
	_, p.accError = fmt.Fprintln(p.w, string(out))
}

func (p *jsonPrinter) error() error {
	return p.accError
}

func Parse(ctx context.Context, w io.Writer, f io.Reader, o Options) error {
	rd := bufio.NewReaderSize(f, 1000*packetSize)
	dmx := astits.NewDemuxer(ctx, rd)
	pmtPID := -1
	nrPics := 0
	sdtPrinted := false
	esKinds := make(map[uint16]string)
	avcPSs := make(map[uint16]*avcPS)
	jp := &jsonPrinter{w: w, indent: o.Indent}
dataLoop:
	for {
		d, err := dmx.NextData()
		if err != nil {
			if err.Error() == "astits: no more packets" {
				break
			}
			return fmt.Errorf("reading next data %w", err)
		}
		if d.SDT != nil && !sdtPrinted {
			parts := make([]string, 0, 4)
			for _, s := range d.SDT.Services {
				parts = append(parts, fmt.Sprintf("service_id: %d", s.ServiceID))
				for _, d := range s.Descriptors {
					switch d.Tag {
					case astits.DescriptorTagService:
						sd := d.Service
						parts = append(parts, fmt.Sprintf("service_name: %s", string(sd.Name)))
						parts = append(parts, fmt.Sprintf("provider_name: %s", string(sd.Provider)))
					}
				}
			}
			fmt.Fprintf(w, "SDT: %s\n", strings.Join(parts, ", "))
			sdtPrinted = true
		}
		if pmtPID < 0 && d.PMT != nil {
			// Loop through elementary streams
			for _, es := range d.PMT.ElementaryStreams {
				var e *elementaryStream
				switch es.StreamType {
				case astits.StreamTypeH264Video:
					e = &elementaryStream{PID: es.ElementaryPID, Codec: "AVC", Type: "video"}
					esKinds[es.ElementaryPID] = "AVC"
				case astits.StreamTypeAACAudio:
					e = &elementaryStream{PID: es.ElementaryPID, Codec: "AAC", Type: "audio"}
					esKinds[es.ElementaryPID] = "AAC"
				}
				if e != nil {
					jp.print(e)
				}
			}
			pmtPID = int(d.PID)
		}
		if pmtPID == -1 {
			continue
		}
		pes := d.PES
		if pes == nil {
			continue
		}
		switch esKinds[d.PID] {
		case "AVC":
			avcPS := avcPSs[d.PID]
			avcPS, err = parseAVCPES(jp, d, avcPS, o.ParameterSets)
			if err != nil {
				return err
			}
			if avcPS == nil {
				continue
			}
			if avcPSs[d.PID] == nil {
				avcPSs[d.PID] = avcPS
			}
			nrPics++
			if o.MaxNrPictures > 0 && nrPics == o.MaxNrPictures {
				break dataLoop
			}
		}
	}
	return jp.error()
}
