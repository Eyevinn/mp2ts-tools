package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"

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

type sdtServiceDescriptor struct {
	ServiceName  string `json:"serviceName"`
	ProviderName string `json:"providerName"`
}

type sdtService struct {
	ServiceID   uint16                 `json:"serviceId"`
	Descriptors []sdtServiceDescriptor `json:"descriptors"`
}

type sdtInfo struct {
	SdtServices []sdtService `json:"SDT"`
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
			sdtInfo := toSdtInfo(d.SDT)
			jp.print(sdtInfo)
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

func toSdtInfo(sdt *astits.SDTData) sdtInfo {
	sdtInfo := sdtInfo{
		SdtServices: make([]sdtService, 0, len(sdt.Services)),
	}

	for _, s := range sdt.Services {
		sdtService := toSdtService(s)
		sdtInfo.SdtServices = append(sdtInfo.SdtServices, sdtService)
	}

	return sdtInfo
}

func toSdtService(s *astits.SDTDataService) sdtService {
	sdtService := sdtService{
		ServiceID:   s.ServiceID,
		Descriptors: make([]sdtServiceDescriptor, 0, len(s.Descriptors)),
	}

	for _, d := range s.Descriptors {
		if d.Tag == astits.DescriptorTagService {
			sdtServiceDescriptor := toSdtServiceDescriptor(d.Service)
			sdtService.Descriptors = append(sdtService.Descriptors, sdtServiceDescriptor)
		}
	}

	return sdtService
}

func toSdtServiceDescriptor(sd *astits.DescriptorService) sdtServiceDescriptor {
	return sdtServiceDescriptor{
		ProviderName: string(sd.Provider),
		ServiceName:  string(sd.Name),
	}
}
