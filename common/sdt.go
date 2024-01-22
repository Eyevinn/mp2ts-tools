package common

import "github.com/asticode/go-astits"

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

func (p *JsonPrinter) PrintSdtInfo(sdt *astits.SDTData, show bool) {
	sdtInfo := ToSdtInfo(sdt)
	p.Print(sdtInfo, show)
}

func ToSdtInfo(sdt *astits.SDTData) sdtInfo {
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
