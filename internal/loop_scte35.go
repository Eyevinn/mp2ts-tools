package internal

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/Comcast/gots/v2"
	"github.com/Comcast/gots/v2/scte35"
)

// SCTE35Section is one splice_info_section, from table_id to CRC_32, with the
// byte position (in the file or segment it was read from) of each of its bytes.
type SCTE35Section struct {
	Data []byte
	Pos  []int
}

// firstPkt and lastPkt return the packet numbers the section starts and ends in.
func (s SCTE35Section) firstPkt() int { return s.Pos[0] / PacketSize }
func (s SCTE35Section) lastPkt() int  { return s.Pos[len(s.Pos)-1] / PacketSize }

// scte35TableID is the table_id of a splice_info_section.
const scte35TableID = 0xfc

// sectionAssembler reassembles the splice_info_sections of an SCTE-35 PID from
// its TS packets. After a section, anything but another section start (table_id
// 0xfc) ends the packet: the 0xff stuffing the standard requires, or the 0x00
// some muxers pad with.
type sectionAssembler struct {
	cur      []byte
	curPos   []int
	sections []SCTE35Section
}

// add feeds one TS packet of the PID; base is the byte position of the packet.
func (a *sectionAssembler) add(b []byte, base int) {
	off := tsPayloadOffset(b)
	if off >= PacketSize {
		return
	}
	if !tsPUSI(b) {
		if a.cur != nil {
			a.cont(b, base, off, PacketSize)
		}
		return
	}
	ptr := off + 1 + int(b[off]) // pointer_field: bytes before it finish the current section
	if ptr > PacketSize {
		a.cur, a.curPos = nil, nil
		return
	}
	if a.cur != nil {
		a.cont(b, base, off+1, ptr)
	}
	a.cur, a.curPos = nil, nil
	// Sections may follow each other in a packet, up to the stuffing.
	for o := ptr; o < PacketSize && b[o] == scte35TableID; {
		a.cur, a.curPos = []byte{}, []int{}
		n := a.cont(b, base, o, PacketSize)
		if a.cur != nil {
			return // continues in the next packet
		}
		o += n
	}
}

// cont appends b[from:to] to the current section and, once the section is
// complete, stores it. It returns how many bytes it consumed.
func (a *sectionAssembler) cont(b []byte, base, from, to int) int {
	n := 0
	for o := from; o < to; o++ {
		a.cur = append(a.cur, b[o])
		a.curPos = append(a.curPos, base+o)
		n++
		if len(a.cur) >= 3 && len(a.cur) == 3+int(binary.BigEndian.Uint16(a.cur[1:3])&0x0fff) {
			a.sections = append(a.sections, SCTE35Section{Data: a.cur, Pos: a.curPos})
			a.cur, a.curPos = nil, nil
			break
		}
	}
	return n
}

// LoopSCTE35 describes which SCTE-35 cues inside the loop are kept. Every kept
// message is re-sent each wrap with its time shifted by the loop duration
// (pts_adjustment) and its event IDs advanced by EventIDStep, so each wrap's
// cues are new events at the right time.
type LoopSCTE35 struct {
	PID         int           `json:"pid"`
	Sections    int           `json:"sections"`    // SCTE-35 sections in the loop's packets
	Moved       int           `json:"moved"`       // sections sent before the loop, moved to its start
	Kept        int           `json:"kept"`        // sections kept, moved ones included
	EventIDStep uint32        `json:"eventIdStep"` // added to every event ID per wrap
	Events      []SCTE35Event `json:"events,omitempty"`

	drop   map[int]bool // source packet numbers of the dropped sections
	inject [][]byte     // moved sections, to send at the start of the segment
}

// SCTE35Event is a cue in the loop: a CUE-OUT with the CUE-IN or duration that
// ends it, a point signal, or a message that cannot be kept.
type SCTE35Event struct {
	Kind     string `json:"kind"` // splice_insert, the segmentation type, or the command
	EventID  uint32 `json:"eventId"`
	StartPTS int64  `json:"startPTS"`         // CUE-OUT (or point) time
	EndPTS   int64  `json:"endPTS,omitempty"` // CUE-IN time, or CUE-OUT + duration
	Messages int    `json:"messages"`         // sections carrying it (repeats included)
	Kept     bool   `json:"kept"`
	Reason   string `json:"reason,omitempty"` // why it is dropped

	segment  bool                          // a segmentation descriptor, not a splice_insert
	outDesc  scte35.SegmentationDescriptor // the CUE-OUT descriptor, to pair a CUE-IN with
	closed   bool                          // a CUE-IN was seen
	duration int64                         // CUE-OUT duration (0 if none)
	point    bool                          // a signal that is neither CUE-OUT nor CUE-IN
	outSecs  []int                         // sections carrying the CUE-OUT (or point)
	inSecs   []int                         // sections carrying the CUE-IN
}

// planSCTE35 decides which SCTE-35 sections in the loop's packet window
// [start, end) to keep. A CUE-OUT is kept only if it is closed, by a CUE-IN or by
// its own duration, and the whole interval lies within the loop [vs, vs+loopDur].
// Cues are usually sent seconds ahead of their splice time, so a section sent
// before the window whose splice time is inside the loop also takes part; if
// kept, it is moved to the start of the segment, still ahead of its time.
// Its CUE-IN, repeats, and cancels share that decision. A CUE-IN or cancel
// without its CUE-OUT is dropped. splice_null heartbeats are kept; other
// commands (splice_schedule, private) and unparsable or encrypted sections are
// dropped. A section is kept only if every event it carries is kept, which may
// in turn drop an event that loses its only CUE-OUT or CUE-IN message.
func planSCTE35(pid int, all []SCTE35Section, start, end int, vs, loopDur int64) *LoopSCTE35 {
	ls := &LoopSCTE35{PID: pid, drop: make(map[int]bool)}
	ve := vs + loopDur
	var secs []SCTE35Section
	var moved []bool // per secs: sent before the window
	for _, s := range all {
		switch {
		case s.lastPkt() < start:
			if msg, err := scte35.NewSCTE35(append([]byte{0}, s.Data...)); err == nil && msg.HasPTS() &&
				SignedPTSDiff(int64(msg.PTS()), vs) >= 0 && SignedPTSDiff(int64(msg.PTS()), ve) < 0 {
				secs, moved = append(secs, s), append(moved, true)
			}
			continue
		case s.firstPkt() >= end:
			continue
		case s.firstPkt() < start || s.lastPkt() >= end:
			for _, p := range s.Pos { // cut by the window edge
				ls.drop[p/PacketSize] = true
			}
			continue
		}
		secs, moved = append(secs, s), append(moved, false)
		ls.Sections++
	}

	var events []*SCTE35Event
	secEvents := make([][]*SCTE35Event, len(secs))
	keep := make([]bool, len(secs))
	add := func(ev *SCTE35Event) *SCTE35Event { events = append(events, ev); return ev }
	// latest returns the most recent event matching f.
	latest := func(f func(*SCTE35Event) bool) *SCTE35Event {
		for k := len(events) - 1; k >= 0; k-- {
			if f(events[k]) {
				return events[k]
			}
		}
		return nil
	}
	for i, s := range secs {
		attach := func(ev *SCTE35Event, out bool) {
			ev.Messages++
			if out {
				ev.outSecs = append(ev.outSecs, i)
			} else {
				ev.inSecs = append(ev.inSecs, i)
			}
			secEvents[i] = append(secEvents[i], ev)
		}
		msg, err := scte35.NewSCTE35(append([]byte{0}, s.Data...))
		if err != nil {
			attach(add(&SCTE35Event{Kind: "invalid", Reason: err.Error()}), true)
			continue
		}
		keep[i] = true
		// Time of the cue: its splice time, or for an immediate cue when it
		// arrives (its position in the loop's packets; moved sections have one).
		t := vs + int64(max(s.firstPkt()-start, 0))*loopDur/int64(end-start)
		if msg.HasPTS() {
			t = int64(msg.PTS())
		}
		switch cmd := msg.CommandInfo().(type) {
		case scte35.SpliceInsertCommand:
			id := cmd.EventID()
			sameID := func(e *SCTE35Event) bool { return !e.segment && !e.point && e.EventID == id }
			switch {
			case cmd.IsEventCanceled():
				if ev := latest(sameID); ev != nil {
					attach(ev, false)
				} else {
					attach(add(&SCTE35Event{Kind: "splice_insert cancel", EventID: id, StartPTS: t,
						Reason: "cancel without its CUE-OUT in the loop"}), true)
				}
			case cmd.IsOut():
				ev := latest(func(e *SCTE35Event) bool { return sameID(e) && e.StartPTS == t })
				if ev == nil {
					ev = add(&SCTE35Event{Kind: "splice_insert", EventID: id, StartPTS: t})
					if cmd.HasDuration() {
						ev.duration = int64(cmd.Duration())
					}
				}
				attach(ev, true)
			default: // CUE-IN: closes the open CUE-OUT with its ID, else the latest open one without a duration
				ev := latest(func(e *SCTE35Event) bool { return sameID(e) && (!e.closed || e.EndPTS == t) })
				if ev == nil {
					ev = latest(func(e *SCTE35Event) bool { return !e.segment && !e.point && !e.closed && e.duration == 0 })
				}
				if ev == nil {
					attach(add(&SCTE35Event{Kind: "splice_insert CUE-IN", EventID: id, StartPTS: t,
						Reason: "CUE-IN without its CUE-OUT in the loop"}), true)
					continue
				}
				ev.closed, ev.EndPTS = true, t
				attach(ev, false)
			}
		default:
			switch msg.Command() {
			case scte35.SpliceNull:
				continue // heartbeat: kept, no event
			case scte35.TimeSignal:
			default:
				keep[i] = false
				attach(add(&SCTE35Event{Kind: scte35.SpliceCommandTypeNames[msg.Command()], StartPTS: t,
					Reason: "unsupported splice command"}), true)
				continue
			}
			for _, d := range msg.Descriptors() {
				id, kind := d.EventID(), scte35.SegDescTypeNames[d.TypeID()]
				seg := func(e *SCTE35Event) bool { return e.segment && e.EventID == id }
				switch {
				case d.IsEventCanceled():
					if ev := latest(seg); ev != nil {
						attach(ev, false)
					} else {
						attach(add(&SCTE35Event{Kind: kind + " cancel", EventID: id, StartPTS: t, segment: true,
							Reason: "cancel without its CUE-OUT in the loop"}), true)
					}
				case d.IsOut():
					ev := latest(func(e *SCTE35Event) bool { return seg(e) && e.Kind == kind && e.StartPTS == t })
					if ev == nil {
						ev = add(&SCTE35Event{Kind: kind, EventID: id, StartPTS: t, segment: true, outDesc: d})
						if d.HasDuration() {
							ev.duration = int64(d.Duration())
						}
					}
					attach(ev, true)
				case d.IsIn():
					ev := latest(func(e *SCTE35Event) bool {
						return e.outDesc != nil && d.CanClose(e.outDesc) && (!e.closed || e.EndPTS == t)
					})
					if ev == nil {
						attach(add(&SCTE35Event{Kind: kind, EventID: id, StartPTS: t, segment: true,
							Reason: "CUE-IN without its CUE-OUT in the loop"}), true)
						continue
					}
					ev.closed, ev.EndPTS = true, t
					attach(ev, false)
				default:
					ev := latest(func(e *SCTE35Event) bool { return seg(e) && e.Kind == kind && e.StartPTS == t })
					if ev == nil {
						ev = add(&SCTE35Event{Kind: kind, EventID: id, StartPTS: t, segment: true, point: true})
					}
					attach(ev, true)
				}
			}
		}
	}

	startsIn := func(t int64) bool { return SignedPTSDiff(t, vs) >= 0 && SignedPTSDiff(t, ve) < 0 }
	endsIn := func(t int64) bool { return SignedPTSDiff(t, vs) >= 0 && SignedPTSDiff(t, ve) <= 0 }
	for _, ev := range events {
		if ev.Reason != "" {
			continue
		}
		if ev.point {
			ev.Kept = startsIn(ev.StartPTS)
			if !ev.Kept {
				ev.Reason = "outside the loop"
			}
			continue
		}
		if !ev.closed {
			if ev.duration == 0 {
				ev.Reason = "CUE-OUT without CUE-IN or duration in the loop"
				continue
			}
			ev.EndPTS = AddPTS(ev.StartPTS, ev.duration)
		}
		ev.Kept = startsIn(ev.StartPTS) && endsIn(ev.EndPTS) && SignedPTSDiff(ev.EndPTS, ev.StartPTS) >= 0
		if !ev.Kept {
			ev.Reason = "not fully inside the loop"
		}
	}
	// A section is kept only if all its events are; an event is lost if all the
	// sections carrying its CUE-OUT, or (when closed by one) its CUE-IN, are.
	allDropped := func(idx []int) bool {
		for _, i := range idx {
			if keep[i] {
				return false
			}
		}
		return true
	}
	for changed := true; changed; {
		changed = false
		for i := range secs {
			for _, ev := range secEvents[i] {
				if keep[i] && !ev.Kept {
					keep[i], changed = false, true
				}
			}
		}
		for _, ev := range events {
			if ev.Kept && (allDropped(ev.outSecs) || (ev.closed && allDropped(ev.inSecs))) {
				ev.Kept, ev.Reason, changed = false, "shares a message with a dropped cue", true
			}
		}
	}

	var minID, maxID uint32
	seen := false
	for i, s := range secs {
		if !keep[i] {
			if !moved[i] {
				for _, p := range s.Pos {
					ls.drop[p/PacketSize] = true
				}
			}
			continue
		}
		if moved[i] {
			ls.Moved++
			ls.inject = append(ls.inject, s.Data)
		}
		ls.Kept++
		for _, o := range scte35EventIDOffsets(s.Data) {
			id := binary.BigEndian.Uint32(s.Data[o:])
			if !seen || id < minID {
				minID = id
			}
			if !seen || id > maxID {
				maxID = id
			}
			seen = true
		}
	}
	if seen {
		ls.EventIDStep = maxID - minID + 1
	}
	for _, ev := range events {
		ls.Events = append(ls.Events, *ev)
	}
	return ls
}

// dropped reports whether the source packet pktNr carries a dropped section.
func (ls *LoopSCTE35) dropped(pktNr int) bool { return ls != nil && ls.drop[pktNr] }

// droppedEvents returns how many events are dropped.
func (ls *LoopSCTE35) droppedEvents() int {
	n := 0
	for _, ev := range ls.Events {
		if !ev.Kept {
			n++
		}
	}
	return n
}

const (
	scte35SpliceInsert        = 0x05
	scte35SegmentationDescTag = 0x02
)

// scte35EventIDOffsets returns the offsets in a splice_info_section of its
// 32-bit event IDs: the splice_event_id of a splice_insert, and the
// segmentation_event_id of every segmentation descriptor.
func scte35EventIDOffsets(sec []byte) []int {
	if len(sec) < 18 {
		return nil
	}
	var offs []int
	if sec[13] == scte35SpliceInsert {
		offs = append(offs, 14)
	}
	cmdLen := int(binary.BigEndian.Uint16(sec[11:13]) & 0x0fff)
	if cmdLen == 0xfff { // legacy: length not given; take it from the parsed command
		msg, err := scte35.NewSCTE35(append([]byte{0}, sec...))
		if err != nil {
			return offs
		}
		cmdLen = len(msg.CommandInfo().Data())
	}
	p := 14 + cmdLen
	crc := len(sec) - 4
	if p+2 > crc {
		return offs
	}
	end := min(p+2+int(binary.BigEndian.Uint16(sec[p:p+2])), crc)
	for p += 2; p+2 <= end; p += 2 + int(sec[p+1]) {
		if sec[p] == scte35SegmentationDescTag && p+10 <= end && bytes.Equal(sec[p+2:p+6], []byte("CUEI")) {
			offs = append(offs, p+6)
		}
	}
	return offs
}

// packetizeSection carries one section in TS packets of pid: pointer_field 0,
// then the section, padded with 0xff. Continuity counters are set when sent.
func packetizeSection(pid int, sec []byte) []byte {
	var out []byte
	payload := append([]byte{0}, sec...)
	for first := true; len(payload) > 0; first = false {
		p := make([]byte, PacketSize)
		p[0], p[1], p[2], p[3] = 0x47, byte(pid>>8)&0x1f, byte(pid), 0x10
		if first {
			p[1] |= 0x40
		}
		n := copy(p[4:], payload)
		for k := 4 + n; k < PacketSize; k++ {
			p[k] = 0xff
		}
		payload = payload[n:]
		out = append(out, p...)
	}
	return out
}

// scte35Patcher re-stamps the kept SCTE-35 sections of a prepared segment for
// each wrap.
type scte35Patcher struct {
	step uint32
	secs []SCTE35Section // positions are byte offsets in the segment
	ids  [][]int         // event ID offsets per section
}

// newSCTE35Patcher finds the SCTE-35 sections in a prepared segment.
func newSCTE35Patcher(data []byte, n, pid int, step uint32) *scte35Patcher {
	var a sectionAssembler
	for i := 0; i < n; i++ {
		if b := data[i*PacketSize : (i+1)*PacketSize]; tsPID(b) == pid {
			a.add(b, i*PacketSize)
		}
	}
	if len(a.sections) == 0 {
		return nil
	}
	p := &scte35Patcher{step: step, secs: a.sections}
	for _, s := range a.sections {
		p.ids = append(p.ids, scte35EventIDOffsets(s.Data))
	}
	return p
}

// packets returns the segment packets carrying SCTE-35 sections, rewritten for
// the given wrap: pts_adjustment advanced by wrap*loopDur (mod 2^33), event IDs
// by wrap*step (mod 2^32), and CRC_32 recomputed. Keys are packet indexes.
func (p *scte35Patcher) packets(data []byte, wrap int, loopDur int64) map[int][]byte {
	out := make(map[int][]byte)
	for k, s := range p.secs {
		sec := append([]byte(nil), s.Data...)
		adj := uint64(sec[4]&1)<<32 | uint64(binary.BigEndian.Uint32(sec[5:9]))
		adj = (adj + uint64(wrap)*uint64(loopDur)) % PtsWrap
		sec[4] = sec[4]&0xfe | byte(adj>>32)
		binary.BigEndian.PutUint32(sec[5:9], uint32(adj))
		for _, o := range p.ids[k] {
			binary.BigEndian.PutUint32(sec[o:], binary.BigEndian.Uint32(sec[o:])+uint32(wrap)*p.step)
		}
		copy(sec[len(sec)-4:], gots.ComputeCRC(sec[:len(sec)-4]))
		for j, b := range sec {
			pkt := s.Pos[j] / PacketSize
			buf, ok := out[pkt]
			if !ok {
				buf = append([]byte(nil), data[pkt*PacketSize:(pkt+1)*PacketSize]...)
				out[pkt] = buf
			}
			buf[s.Pos[j]%PacketSize] = b
		}
	}
	return out
}

// finishPlan adds what the plan needs from the whole stream: how far its PCR is
// from a constant rate, and which SCTE-35 cues of the chosen loop are kept.
func finishPlan(ts *TSStream, plan *LoopPlan) {
	if plan == nil || plan.NumGOPs < 1 {
		return
	}
	if plan.PCR != nil {
		plan.PCR.SourceDeviationMs = math.Round(ts.PCRDeviationMs()*10) / 10
		if !plan.PCR.ConstantRate {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"not a constant-rate stream (its PCR is up to %.0f ms from a constant rate): the PCR is "+
					"regenerated at the average rate, which moves packets that far from their source timing",
				plan.PCR.SourceDeviationMs))
		}
	}
	planLoopSCTE35(ts, plan)
}

// planLoopSCTE35 adds to the plan which SCTE-35 cues of the chosen loop are kept.
func planLoopSCTE35(ts *TSStream, plan *LoopPlan) {
	if ts.SCTE35Pid < 0 {
		return
	}
	start, end := plan.packetWindow()
	plan.SCTE35 = planSCTE35(ts.SCTE35Pid, ts.scte35.sections, start, end, plan.StartPTS, plan.LoopDurTicks)
	if plan.SCTE35.droppedEvents() > 0 {
		plan.Warnings = append(plan.Warnings, scte35Warning(plan.SCTE35))
	}
}

// scte35Warning summarizes dropped SCTE-35 events for the plan.
func scte35Warning(ls *LoopSCTE35) string {
	return fmt.Sprintf("SCTE-35: dropped %d of %d cues (not fully inside the loop, or cannot be kept); see loop.scte35.events",
		ls.droppedEvents(), len(ls.Events))
}
