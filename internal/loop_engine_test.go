package internal

import "testing"

func TestSignedPTSJitter(t *testing.T) {
	cases := []struct {
		ptsDiff, ideal, want int64
	}{
		{3600, 3600, 0},
		{3602, 3600, 2},
		{3598, 3600, -2},
		{-7202, 3600, -2}, // two ideal steps minus 2
		{1501, 1501, 0},
		{1502, 1501, 1}, // 59.94 dither: 1502 vs ideal 1501
	}
	for _, c := range cases {
		if got := SignedPTSJitter(c.ptsDiff, c.ideal); got != c.want {
			t.Errorf("SignedPTSJitter(%d,%d)=%d want %d", c.ptsDiff, c.ideal, got, c.want)
		}
	}
}

func TestAddPCRWrap(t *testing.T) {
	if got := AddPCR(PcrWrap-10, 20); got != 10 {
		t.Errorf("AddPCR wrap-around: got %d want 10", got)
	}
	if got := AddPCR(100, -200); got != PcrWrap-100 {
		t.Errorf("AddPCR negative: got %d want %d", got, PcrWrap-100)
	}
}

func TestScanRAP(t *testing.T) {
	sc := []byte{0, 0, 1}
	// AVC: nal_ref_idc=3, type=5 (IDR) -> 0x65 ; type=1 (non-IDR) -> 0x41
	avcIDR := append(append([]byte{}, sc...), 0x65, 0x88)
	avcNonIDR := append(append([]byte{}, sc...), 0x41, 0x88)
	// HEVC: first header byte = type<<1. CRA=21 -> 0x2a, IDR_W_RADL=19 -> 0x26,
	// TRAIL_R=1 -> 0x02, VPS=32 -> 0x40 (non-VCL, must be skipped).
	hevcVPS := append([]byte{}, sc...)
	hevcVPS = append(hevcVPS, 0x40, 0x01)
	hevcCRA := append(append(append([]byte{}, hevcVPS...), sc...), 0x2a, 0x00)
	hevcIDR := append(append([]byte{}, sc...), 0x26, 0x00)
	hevcTrail := append(append([]byte{}, sc...), 0x02, 0x00)

	cases := []struct {
		name               string
		codec              Codec
		data               []byte
		idr, cra, rap, vcl bool
	}{
		{"avc-idr", CODEC_AVC, avcIDR, true, false, true, true},
		{"avc-nonidr", CODEC_AVC, avcNonIDR, false, false, false, true},
		{"hevc-cra-after-vps", CODEC_HEVC, hevcCRA, false, true, true, true},
		{"hevc-idr", CODEC_HEVC, hevcIDR, true, false, true, true},
		{"hevc-trail", CODEC_HEVC, hevcTrail, false, false, false, true},
	}
	for _, c := range cases {
		got := ScanRAP(c.codec, c.data)
		if got.IsIDR != c.idr || got.IsCRA != c.cra || got.IsRAP != c.rap || got.IsVCL != c.vcl {
			t.Errorf("%s: ScanRAP=%+v want idr=%v cra=%v rap=%v vcl=%v",
				c.name, got, c.idr, c.cra, c.rap, c.vcl)
		}
	}
}

func TestScanAUParameterSets(t *testing.T) {
	sc := []byte{0, 0, 1}
	// HEVC: VPS(0x40), SPS(0x42), PPS(0x44), then IDR_W_RADL(0x26).
	hevc := []byte{}
	for _, b := range []byte{0x40, 0x42, 0x44, 0x26} {
		hevc = append(hevc, sc...)
		hevc = append(hevc, b, 0x00)
	}
	au := ScanAU(CODEC_HEVC, hevc)
	if !au.HasVPS || !au.HasSPS || !au.HasPPS || !au.IsIDR || !au.PSComplete(CODEC_HEVC) {
		t.Errorf("HEVC ScanAU=%+v want all PS + IDR + PSComplete", au)
	}
	// AVC: SPS(0x67), PPS(0x68), IDR(0x65). No VPS in AVC.
	avc := []byte{}
	for _, b := range []byte{0x67, 0x68, 0x65} {
		avc = append(avc, sc...)
		avc = append(avc, b, 0x00)
	}
	au = ScanAU(CODEC_AVC, avc)
	if !au.HasSPS || !au.HasPPS || !au.IsIDR || !au.PSComplete(CODEC_AVC) {
		t.Errorf("AVC ScanAU=%+v want SPS+PPS+IDR+PSComplete", au)
	}
}

func TestClassifyFrameRateLabel(t *testing.T) {
	cases := map[int64]string{
		1800: "50", 1500: "60", 1501: "59.94", 3000: "30",
		3003: "29.97", 3600: "25", 3750: "24", 3754: "23.976",
	}
	for step, want := range cases {
		if got := classifyFrameRateLabel(step); got != want {
			t.Errorf("classifyFrameRateLabel(%d)=%q want %q", step, got, want)
		}
	}
}

func TestSelectLoop(t *testing.T) {
	// 4 IDRs one GOP (90000 ticks) apart, all carrying parameter sets.
	vid := &scanTrack{pid: 256, codec: CODEC_HEVC, mediaType: "video"}
	for i := 0; i < 4; i++ {
		vid.pics = append(vid.pics, PicRecord{
			PTS: int64(i) * 90000, StartPktNr: uint32(i) * 100,
			IsIDR: true, IsRAP: true, PSPresent: true, Field: FieldFrame,
		})
	}
	// audio: one frame per PES, 3000 ticks apart (30 frames/GOP), covering the range.
	aud := &scanTrack{pid: 257, codec: CODEC_AAC, mediaType: "audio"}
	for i := 0; i <= 90; i++ {
		aud.audio = append(aud.audio, AudioPESRec{PTS: int64(i) * 3000, StartPktNr: uint32(i)})
	}

	// Longest loop.
	p := selectLoop(vid, []*scanTrack{aud}, 0, 256, 15_000_000, true)
	if p == nil || p.NumGOPs != 3 || p.LoopDurTicks != 270000 || p.StartPTS != 0 || p.EndPTS != 270000 {
		t.Fatalf("longest loop: %+v", p)
	}
	if !p.PSAtStart || p.LoopPointType != "IDR" {
		t.Errorf("expected IDR with PS at start: %+v", p)
	}
	if len(p.Audio) != 1 || p.Audio[0].FrameDurTicks != 3000 || p.Audio[0].WholeFrames != 90 || p.Audio[0].ResidualTicks != 0 {
		t.Errorf("audio fit: %+v", p.Audio)
	}
	if p.PCR == nil || p.PCR.SegmentPackets != 300 {
		t.Errorf("pcr: %+v", p.PCR)
	}

	// Duration cap to two GOPs.
	p2 := selectLoop(vid, []*scanTrack{aud}, 2000, 256, 15_000_000, true)
	if p2.NumGOPs != 2 || p2.LoopDurTicks != 180000 {
		t.Errorf("capped loop: numGops=%d dur=%d", p2.NumGOPs, p2.LoopDurTicks)
	}
}

func TestStatOf(t *testing.T) {
	s := statOf([]int64{1800, 1800, 1800})
	if s.Min != 1800 || s.Max != 1800 || s.Avg != 1800 {
		t.Errorf("statOf constant: %+v", s)
	}
	s = statOf([]int64{1501, 1502, 1501, 1502})
	if s.Min != 1501 || s.Max != 1502 {
		t.Errorf("statOf dither: %+v", s)
	}
}
