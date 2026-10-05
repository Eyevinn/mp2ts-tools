package internal

import (
	"bytes"
	"context"
	"testing"

	"github.com/Comcast/gots/v2"
	"github.com/Comcast/gots/v2/scte35"
)

// mkSCTE35 encodes a splice_info_section with gots, with pts_adjustment 0.
func mkSCTE35(cmd scte35.SpliceCommand, descs ...scte35.SegmentationDescriptor) []byte {
	msg := scte35.CreateSCTE35()
	msg.SetCommandInfo(cmd)
	if len(descs) > 0 {
		msg.SetDescriptors(descs)
	}
	sec := append([]byte(nil), msg.UpdateData()...)
	sec[4] &^= 0x01
	copy(sec[5:9], []byte{0, 0, 0, 0})
	copy(sec[len(sec)-4:], gots.ComputeCRC(sec[:len(sec)-4]))
	return sec
}

func spliceInsert(id uint32, out bool, pts, dur int64) []byte {
	cmd := scte35.CreateSpliceInsertCommand()
	cmd.SetEventID(id)
	cmd.SetIsOut(out)
	cmd.SetIsProgramSplice(true)
	cmd.SetHasPTS(true)
	cmd.SetPTS(gots.PTS(pts))
	if dur > 0 {
		cmd.SetHasDuration(true)
		cmd.SetIsAutoReturn(true)
		cmd.SetDuration(gots.PTS(dur))
	}
	return mkSCTE35(cmd)
}

func segDesc(id uint32, typ scte35.SegDescType, dur int64) scte35.SegmentationDescriptor {
	d := scte35.CreateSegmentationDescriptor()
	d.SetEventID(id)
	d.SetTypeID(typ)
	if dur > 0 {
		d.SetHasDuration(true)
		d.SetDuration(gots.PTS(dur))
	}
	return d
}

func timeSignal(pts int64, descs ...scte35.SegmentationDescriptor) []byte {
	cmd := scte35.CreateTimeSignalCommand()
	cmd.SetHasPTS(true)
	cmd.SetPTS(gots.PTS(pts))
	return mkSCTE35(cmd, descs...)
}

// at places a section in packet pkt (or from pkt on, if longer than a packet).
func at(pkt int, data []byte) SCTE35Section {
	s := SCTE35Section{Data: data}
	for j := range data {
		s.Pos = append(s.Pos, (pkt+j/180)*PacketSize+4+j%180)
	}
	return s
}

func TestPlanSCTE35(t *testing.T) {
	const (
		vs      = 900_000
		loopDur = 900_000 // the loop is [900000, 1800000]
		start   = 100     // and packets [100, 1100)
		end     = 1100
	)
	secs := []SCTE35Section{
		at(50, spliceInsert(5, true, 1_500_000, 90_000)),   // sent before the loop: moved
		at(60, spliceInsert(6, true, 500_000, 90_000)),     // before the loop, for a time before it
		at(200, spliceInsert(1, true, 1_000_000, 270_000)), // auto-return, inside
		at(210, spliceInsert(1, true, 1_000_000, 270_000)), // its repeat
		at(300, spliceInsert(2, true, 1_600_000, 300_000)), // ends after the loop
		at(400, spliceInsert(3, true, 1_100_000, 0)),       // CUE-OUT ...
		at(450, spliceInsert(3, false, 1_300_000, 0)),      // ... and its CUE-IN
		at(500, spliceInsert(99, false, 1_400_000, 0)),     // CUE-IN without CUE-OUT
		at(600, timeSignal(1_050_000, segDesc(7, scte35.SegDescProviderPOStart, 0))),
		at(650, timeSignal(1_150_000, segDesc(7, scte35.SegDescProviderPOEnd, 0))),
		at(700, timeSignal(1_200_000, segDesc(8, scte35.SegDescProviderPOStart, 0))), // never ended
		at(800, timeSignal(1_250_000, segDesc(9, scte35.SegDescProviderPOStart, 0))),
		// Ends 9 but also starts 10, which never ends: the section is dropped, so 9 is too.
		at(850, timeSignal(1_350_000, segDesc(9, scte35.SegDescProviderPOEnd, 0),
			segDesc(10, scte35.SegDescProviderPOStart, 0))),
		at(900, mkSCTE35(scte35.CreateSpliceNull())),                                   // heartbeat
		at(1099, timeSignal(1_700_000, segDesc(11, scte35.SegDescProviderPOStart, 0))), // fits
	}
	ls := planSCTE35(1001, secs, start, end, vs, loopDur)

	kept := map[string]bool{}
	reason := map[string]string{}
	for _, ev := range ls.Events {
		key := ev.Kind + ":" + string(rune('0'+ev.EventID))
		kept[key] = ev.Kept
		reason[key] = ev.Reason
	}
	want := map[string]bool{
		"splice_insert:5": true, "splice_insert:1": true, "splice_insert:2": false,
		"splice_insert:3": true, "splice_insert CUE-IN:" + string(rune('0'+99)): false,
		"SegDescProviderPOStart:7": true, "SegDescProviderPOStart:8": false,
		"SegDescProviderPOStart:9": false, "SegDescProviderPOStart:" + string(rune('0'+10)): false,
		"SegDescProviderPOStart:" + string(rune('0'+11)): false,
	}
	for k, w := range want {
		got, ok := kept[k]
		if !ok || got != w {
			t.Errorf("event %s: kept=%v (present %v, reason %q), want kept=%v", k, got, ok, reason[k], w)
		}
	}
	if _, ok := kept["splice_insert:6"]; ok {
		t.Errorf("a cue sent before the loop for a time before it should be ignored")
	}
	if r := reason["SegDescProviderPOStart:9"]; r != "shares a message with a dropped cue" {
		t.Errorf("event 9 reason %q", r)
	}
	for _, ev := range ls.Events {
		if ev.Kind == "splice_insert" && ev.EventID == 1 && (ev.Messages != 2 || ev.EndPTS != 1_270_000) {
			t.Errorf("event 1: %d messages, end %d; want 2 messages ending at 1270000", ev.Messages, ev.EndPTS)
		}
	}
	// Kept sections: 5 (moved), 1 twice, 3 twice, 7 twice, splice_null.
	if ls.Sections != 13 || ls.Moved != 1 || ls.Kept != 8 || ls.EventIDStep != 7 {
		t.Errorf("sections %d, moved %d, kept %d, step %d; want 13, 1, 8, 7",
			ls.Sections, ls.Moved, ls.Kept, ls.EventIDStep)
	}
	for _, pkt := range []int{300, 500, 700, 800, 850, 1099} {
		if !ls.dropped(pkt) {
			t.Errorf("packet %d should be dropped", pkt)
		}
	}
	for _, pkt := range []int{200, 210, 400, 450, 600, 650, 900} {
		if ls.dropped(pkt) {
			t.Errorf("packet %d should be kept", pkt)
		}
	}
}

func TestSectionAssembler(t *testing.T) {
	var descs []scte35.SegmentationDescriptor
	for id := uint32(1); id <= 12; id++ {
		descs = append(descs, segDesc(id, scte35.SegDescProviderPOStart, 0))
	}
	long := timeSignal(900_000, descs...)
	if len(long) <= PacketSize {
		t.Fatalf("test section too short to span packets: %d", len(long))
	}
	short := spliceInsert(1, true, 900_000, 0)
	// Two sections in one packet, padded with 0x00 as some muxers do.
	two := make([]byte, PacketSize)
	copy(two, packetizeSection(500, short)[:4])
	payload := append(append(append([]byte{0}, short...), short...), make([]byte, PacketSize)...)
	copy(two[4:], payload)
	data := append(append(packetizeSection(500, long), two...), packetizeSection(500, short)...)

	var a sectionAssembler
	for i := 0; i < len(data)/PacketSize; i++ {
		a.add(data[i*PacketSize:(i+1)*PacketSize], i*PacketSize)
	}
	want := [][]byte{long, short, short, short}
	if len(a.sections) != len(want) {
		t.Fatalf("got %d sections, want %d", len(a.sections), len(want))
	}
	for k, s := range a.sections {
		if !bytes.Equal(s.Data, want[k]) {
			t.Errorf("section %d differs", k)
		}
		for j, p := range s.Pos {
			if data[p] != s.Data[j] {
				t.Fatalf("section %d byte %d: position %d holds 0x%02x, want 0x%02x", k, j, p, data[p], s.Data[j])
			}
		}
	}
}

// TestSCTE35PatcherWrap re-stamps a single- and a two-packet section for wrap 2:
// the time moves by two loop durations, event IDs by two steps, and the CRC holds.
func TestSCTE35PatcherWrap(t *testing.T) {
	const pid, loopDur, step = 500, 900_000, 7
	descs := []scte35.SegmentationDescriptor{}
	for id := uint32(20); id < 28; id++ {
		descs = append(descs, segDesc(id, scte35.SegDescProviderPOStart, 0))
	}
	data := append(packetizeSection(pid, spliceInsert(3, true, 1_000_000, 90_000)),
		packetizeSection(pid, timeSignal(PtsWrap-100_000, descs...))...)
	p := newSCTE35Patcher(data, len(data)/PacketSize, pid, step)
	if p == nil || len(p.secs) != 2 {
		t.Fatal("patcher did not find the two sections")
	}
	out := append([]byte(nil), data...)
	for i, pkt := range p.packets(data, 2, loopDur) {
		copy(out[i*PacketSize:], pkt)
	}
	var a sectionAssembler
	for i := 0; i < len(out)/PacketSize; i++ {
		a.add(out[i*PacketSize:(i+1)*PacketSize], i*PacketSize)
	}
	if len(a.sections) != 2 {
		t.Fatalf("got %d sections after patching", len(a.sections))
	}
	wantPTS := []int64{1_000_000 + 2*loopDur, (PtsWrap - 100_000 + 2*loopDur) % PtsWrap}
	for k, s := range a.sections {
		if !bytes.Equal(gots.ComputeCRC(s.Data[:len(s.Data)-4]), s.Data[len(s.Data)-4:]) {
			t.Errorf("section %d: CRC_32 not updated", k)
		}
		msg, err := scte35.NewSCTE35(append([]byte{0}, s.Data...))
		if err != nil {
			t.Fatal(err)
		}
		if got := int64(msg.PTS()); got != wantPTS[k] {
			t.Errorf("section %d: PTS %d, want %d", k, got, wantPTS[k])
		}
	}
	msg, _ := scte35.NewSCTE35(append([]byte{0}, a.sections[0].Data...))
	if id := msg.CommandInfo().(scte35.SpliceInsertCommand).EventID(); id != 3+2*step {
		t.Errorf("splice_event_id %d, want %d", id, 3+2*step)
	}
	msg, _ = scte35.NewSCTE35(append([]byte{0}, a.sections[1].Data...))
	for k, d := range msg.Descriptors() {
		if want := uint32(20+k) + 2*step; d.EventID() != want {
			t.Errorf("segmentation_event_id %d, want %d", d.EventID(), want)
		}
	}
}

// TestLoopSCTE35Fixture loops the bundled stream with an ad cue sent before the
// loop start: the cue is moved into the loop and re-sent every wrap with its
// time shifted by the loop duration and its event ID increased.
func TestLoopSCTE35Fixture(t *testing.T) {
	seg, plan, err := PrepareLoop(context.TODO(), "testdata/80s_with_ad.ts", 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SCTE35 == nil || plan.SCTE35.Kept != 1 || plan.SCTE35.Moved != 1 {
		t.Fatalf("SCTE-35 plan: %+v", plan.SCTE35)
	}
	var out bytes.Buffer
	const wraps = 3
	if err := LoopToWriter(context.TODO(), seg, &out, wraps); err != nil {
		t.Fatal(err)
	}
	b := out.Bytes()
	var a sectionAssembler
	for i := 0; i < len(b)/PacketSize; i++ {
		if tsPID(b[i*PacketSize:]) == plan.SCTE35.PID {
			a.add(b[i*PacketSize:(i+1)*PacketSize], i*PacketSize)
		}
	}
	if len(a.sections) != wraps {
		t.Fatalf("got %d SCTE-35 sections in %d wraps", len(a.sections), wraps)
	}
	for w, s := range a.sections {
		if got := s.firstPkt() - w*seg.NumPackets; got != 2 {
			t.Errorf("wrap %d: cue in packet %d of the wrap, want 2 (after PAT and PMT)", w, got)
		}
		msg, err := scte35.NewSCTE35(append([]byte{0}, s.Data...))
		if err != nil {
			t.Fatal(err)
		}
		ins := msg.CommandInfo().(scte35.SpliceInsertCommand)
		if ins.EventID() != 255+uint32(w) || int64(msg.PTS()) != 1_032_000+int64(w)*seg.LoopDurTicks {
			t.Errorf("wrap %d: event %d at %d, want %d at %d", w, ins.EventID(), msg.PTS(),
				255+w, 1_032_000+int64(w)*seg.LoopDurTicks)
		}
	}
}
