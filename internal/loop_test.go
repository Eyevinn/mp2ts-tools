package internal

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/Comcast/gots/v2"
	"github.com/Comcast/gots/v2/scte35"
)

// sourcePTS reads the source PTS a test audio frame carries.
func sourcePTS(frame []byte) int64 {
	b := frame[tSourcePTSOffset:]
	return int64(b[0])<<24 | int64(b[1])<<16 | int64(b[2])<<8 | int64(b[3])
}

func scanData(t *testing.T, data []byte) (*streamIndex, error) {
	t.Helper()
	return scanStream(context.Background(), data)
}

// TestLoopConstantRate loops a constant-rate stream with audio sent with the
// video, after it (moved to the next lap's start) and before it (moved to the
// previous lap's end), and checks the seams.
func TestLoopConstantRate(t *testing.T) {
	for _, delay := range []int64{0, 13_500, -13_500} {
		t.Run(fmt.Sprintf("audio mux delay %d", delay), func(t *testing.T) {
			source := buildStream(t, streamOptions{seconds: 6, audioMuxDelay: delay})
			const laps = 30
			l, out, stats := loopData(t, source, LoopOptions{MaxDurationMS: 2000}, laps)
			rep, seg := l.Report, l.seg
			lp := rep.Loop
			if lp.AccessPoint != "closed-gop" || !lp.WholeFrames || !lp.ConstantRate || lp.FrameRate != "25" ||
				lp.DurationTicks%90_000 != 0 {
				t.Fatalf("loop %+v", lp)
			}
			if len(lp.Audio) != 2 {
				t.Fatalf("audio fits %+v", lp.Audio)
			}
			for _, a := range lp.Audio {
				if a.ExcessMS < 0 || a.SeamOffsetMS < -21.4 || a.SeamOffsetMS > 21.4 {
					t.Errorf("audio fit %+v", a)
				}
			}
			// The stream with three frames per PES starts the loop mid-PES; the other cannot.
			if rep.Lap.AudioHeadCuts != 1 || seg.packetCount() != seg.lapPackets || stats.AudioGaps != 0 {
				t.Errorf("lap %+v, %d packets of %d, stats %+v", rep.Lap, seg.packetCount(), seg.lapPackets, stats)
			}
			if delay != 0 && stats.MovedAudioPackets == 0 {
				t.Error("no audio moved across seams")
			}
			if n := continuityErrors(out); n != 0 {
				t.Errorf("%d continuity errors", n)
			}

			var video []*pesRecord
			audio := make(map[int][]*pesRecord)
			for _, r := range collectPES(out) {
				if r.pid == tVideoPID {
					video = append(video, r)
				} else {
					audio[r.pid] = append(audio[r.pid], r)
				}
			}
			if len(video) != laps*lp.Pictures || len(audio) != 2 {
				t.Fatalf("%d video PES, want %d; %d audio streams", len(video), laps*lp.Pictures, len(audio))
			}
			// Video timestamps continue across every seam.
			for i := 1; i < len(video); i++ {
				if video[i].dts-video[i-1].dts != tFrameTicks || video[i].pts-video[i].dts != 2*tFrameTicks {
					t.Fatalf("video PES %d: DTS step %d, PTS - DTS %d", i, video[i].dts-video[i-1].dts, video[i].pts-video[i].dts)
				}
			}
			loopStart := int64(lp.StartPTS)
			loopEnd := loopStart + lp.DurationTicks
			for pid, recs := range audio {
				// Audio timestamps continue across every seam.
				for i := 0; i+1 < len(recs); i++ {
					if len(recs[i].payload)%tAudioFrameSize != 0 {
						t.Fatalf("audio PID %d PES %d: %d bytes", pid, i, len(recs[i].payload))
					}
					n := int64(len(recs[i].payload) / tAudioFrameSize)
					if recs[i+1].pts-recs[i].pts != n*tAudioFrameTicks {
						t.Fatalf("audio PID %d PES %d at packet %d: PTS step %d for %d frames", pid, i, recs[i].packet,
							recs[i+1].pts-recs[i].pts, n)
					}
				}
				// Every audio frame plays with the video it was made with, up to a frame later.
				if d := recs[0].pts - loopStart; d < -tAudioFrameTicks/2 || d > tAudioFrameTicks/2 {
					t.Errorf("audio PID %d starts %d ticks from the loop", pid, d)
				}
				audioEnd := int64(0)
				for _, r := range recs {
					for k := 0; k+tAudioFrameSize <= len(r.payload); k += tAudioFrameSize {
						src := sourcePTS(r.payload[k:])
						outPTS := r.pts + int64(k/tAudioFrameSize)*tAudioFrameTicks
						if src < loopStart-tAudioFrameTicks/2 || src >= loopEnd+tAudioFrameTicks/2 {
							t.Fatalf("audio PID %d: frame from %d outside the loop %d..%d", pid, src, loopStart, loopEnd)
						}
						shift := outPTS - src
						if shift < 0 || shift%lp.DurationTicks >= tAudioFrameTicks {
							t.Fatalf("audio PID %d: frame from %d plays at %d", pid, src, outPTS)
						}
						audioEnd = outPTS + tAudioFrameTicks
					}
				}
				// The ending carries the last lap's audio to where its video ends.
				videoEnd := loopStart + laps*lp.DurationTicks
				if audioEnd < videoEnd-tAudioFrameTicks/2 || audioEnd >= videoEnd+tAudioFrameTicks*3/2 {
					t.Errorf("audio PID %d ends at %d, the video at %d", pid, audioEnd, videoEnd)
				}
			}

			// PCR is exactly linear at the lap's packet rate.
			pre := rep.Lap.PreamblePackets
			lapTime := lp.DurationTicks * pcrPerTick
			n := int64(seg.packetCount())
			first, firstPCR := -1, int64(0)
			for i := pre; i < len(out)/PacketSize; i++ {
				p := packetAt(out, i)
				if !tsCarriesPCR(p) {
					continue
				}
				pcr := int64(readPCR(p[pcrFieldOffset:]))
				if first < 0 {
					first, firstPCR = i, pcr
					continue
				}
				d := int64(i - pre)
				want := d/n*lapTime + d%n*lapTime/n - int64(first-pre)*lapTime/n
				if diff := pcr - firstPCR - want; diff < -1 || diff > 1 {
					t.Fatalf("packet %d: PCR %d off a linear PCR", i, diff)
				}
			}

			// The first lap carries the source's video bytes unchanged.
			var srcVideo, lapVideo []byte
			pic := 0
			for _, r := range collectPES(source) {
				if r.pid != tVideoPID {
					continue
				}
				if pic >= l.chosen.startPicture && pic < l.chosen.endPicture {
					srcVideo = append(srcVideo, r.payload...)
				}
				pic++
			}
			for _, r := range video[:lp.Pictures] {
				lapVideo = append(lapVideo, r.payload...)
			}
			if string(srcVideo) != string(lapVideo) {
				t.Error("the first lap's video differs from the source's")
			}
		})
	}
}

// TestLoop2398 checks that a 23.98 fps loop is a multiple of four frames, so the
// DTS cadence continues across the seam: a frame is 3753.75 ticks, and with an
// access point every 30 frames a loop of one GOP cannot be exact.
func TestLoop2398(t *testing.T) {
	source := buildStream(t, streamOptions{seconds: 4, rate: FrameRate{24000, 1001}, gop: 30})
	const laps = 8
	l, out, _ := loopData(t, source, LoopOptions{}, laps)
	lp := l.Report.Loop
	if lp.FrameRate != "24000/1001" || !lp.WholeFrames || lp.Pictures != 60 || lp.DurationTicks != 225_225 {
		t.Fatalf("loop %+v", lp)
	}
	var dts []int64
	for _, r := range collectPES(out) {
		if r.pid == tVideoPID {
			dts = append(dts, r.dts)
		}
	}
	if len(dts) != laps*lp.Pictures {
		t.Fatalf("%d video PES", len(dts))
	}
	for i := 4; i < len(dts); i++ {
		if s := dts[i] - dts[i-1]; (s != 3753 && s != 3754) || dts[i]-dts[i-4] != 15_015 {
			t.Fatalf("video PES %d: DTS step %d", i, s)
		}
	}
}

// TestLoopVariableRate checks that a variable-rate stream keeps its source
// timing and its PCRs across every seam. A clock running 2% slow sends each lap
// in less time than it lasts, which leaves a gap before the next lap.
func TestLoopVariableRate(t *testing.T) {
	source := variableRate(buildStream(t, streamOptions{seconds: 6}), 0.98)
	srcSpacing := int64(0)
	src := outputPCRs(source, tVideoPID, 0)
	for i := 1; i < len(src); i++ {
		srcSpacing = max(srcSpacing, src[i]-src[i-1])
	}
	const laps = 5
	l, out, stats := loopData(t, source, LoopOptions{}, laps)
	lp := l.Report.Loop
	if lp.ConstantRate || !lp.WholeFrames || l.chosen.windowTime >= lp.DurationTicks*pcrPerTick {
		t.Fatalf("loop %+v", lp)
	}
	if l.Report.Lap.AddedPCRPackets == 0 || stats.AudioGaps != 0 {
		t.Errorf("lap %+v, stats %+v", l.Report.Lap, stats)
	}
	if n := continuityErrors(out); n != 0 {
		t.Errorf("%d continuity errors", n)
	}
	pcrs := outputPCRs(out, tVideoPID, l.Report.Lap.PreamblePackets)
	for i := 1; i < len(pcrs); i++ {
		if d := pcrs[i] - pcrs[i-1]; d <= 0 || d > max(maxPCRSpacing, srcSpacing) {
			t.Fatalf("PCR %d: step %d", i, d)
		}
	}
	if pcrs[len(pcrs)-1]-pcrs[0] >= laps*lp.DurationTicks*pcrPerTick {
		t.Error("the PCR runs past the laps")
	}
	var video, audio []*pesRecord
	for _, r := range collectPES(out) {
		switch r.pid {
		case tVideoPID:
			video = append(video, r)
		case tAudioPID:
			audio = append(audio, r)
		}
	}
	if len(video) != laps*lp.Pictures {
		t.Fatalf("%d video PES", len(video))
	}
	for i := 1; i < len(video); i++ {
		if video[i].dts-video[i-1].dts != tFrameTicks {
			t.Fatalf("video PES %d: DTS step %d", i, video[i].dts-video[i-1].dts)
		}
	}
	for i := 0; i+1 < len(audio); i++ {
		if n := int64(len(audio[i].payload) / tAudioFrameSize); audio[i+1].pts-audio[i].pts != n*tAudioFrameTicks {
			t.Fatalf("audio PES %d: PTS step %d", i, audio[i+1].pts-audio[i].pts)
		}
	}
}

// TestLoopPaddedSeamPCR checks that a constant-rate seam padded with null
// packets keeps the PCR within 40 ms.
func TestLoopPaddedSeamPCR(t *testing.T) {
	source := buildStream(t, streamOptions{seconds: 6})
	opts := LoopOptions{MaxDurationMS: 2000}
	plain, _, _ := loopData(t, source, opts, 1)
	const moved = 60 // 45 ms at 1.5 Mbit/s
	padded := nullsMovedBefore(t, source, plain.Report.Loop.StartPacket, plain.Report.Loop.EndPacket, moved)
	const laps = 4
	l, out, _ := loopData(t, padded, opts, laps)
	if !l.Report.Loop.ConstantRate || l.chosen.startPicture != plain.chosen.startPicture ||
		l.chosen.endPicture != plain.chosen.endPicture {
		t.Fatalf("loop %+v", l.Report.Loop)
	}
	if lap := l.Report.Lap; lap.AddedNulls < moved || lap.AddedPCRPackets == 0 || lap.Packets != l.seg.lapPackets {
		t.Errorf("lap %+v", lap)
	}
	if len(l.seg.warnings) != 0 {
		t.Errorf("warnings %v", l.seg.warnings)
	}
	if n := continuityErrors(out); n != 0 {
		t.Errorf("%d continuity errors", n)
	}
	pcrs := outputPCRs(out, tVideoPID, l.Report.Lap.PreamblePackets)
	for i := 1; i < len(pcrs); i++ {
		if d := pcrs[i] - pcrs[i-1]; d > maxPCRSpacing {
			t.Fatalf("PCR %d: %d ms after the one before", i, d/27_000)
		}
	}
}

// TestLoopFieldPictures loops H.264 coded as two field pictures a frame, each
// in its own PES, top or bottom field first: the frame rate is the frame rate,
// not the field rate, a lap is whole frames from a first field, and the fields
// step on evenly across every seam.
func TestLoopFieldPictures(t *testing.T) {
	for _, bottomFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("bottom first %v", bottomFirst), func(t *testing.T) {
			source := buildStream(t, streamOptions{seconds: 4, fields: true, bottomFirst: bottomFirst})
			const laps = 3
			l, out, _ := loopData(t, source, LoopOptions{}, laps)
			first, order := FieldTop, "top-first"
			if bottomFirst {
				first, order = FieldBottom, "bottom-first"
			}
			// The 5 frames before the first IDR come before the parameter sets.
			if v := l.Report.Video; v.FrameRate != "25" || v.FieldPictures != v.Pictures-10 || v.FieldOrder != order {
				t.Errorf("video %+v", v)
			}
			// IDR fields at frames 5, 30, 55 and 80: the longest loop is 75 frames.
			lp := l.Report.Loop
			if lp.FrameRate != "25" || !lp.WholeFrames || lp.Frames != 75 || lp.Pictures != 150 ||
				lp.DurationTicks != 75*tFrameTicks {
				t.Errorf("loop %+v", lp)
			}
			var video []*pesRecord
			for _, r := range collectPES(out) {
				if r.pid == tVideoPID {
					video = append(video, r)
				}
			}
			if len(video) != laps*lp.Pictures {
				t.Fatalf("%d video PES, want %d", len(video), laps*lp.Pictures)
			}
			insp := newPictureInspector(CODEC_AVC)
			for i, r := range video {
				info := insp.inspect(r.payload)
				if i%lp.Pictures == 0 && (info.accessPoint != apClosedGOP || info.field != first) {
					t.Errorf("lap %d starts with %+v", i/lp.Pictures, info)
				}
				if i > 0 && (r.dts-video[i-1].dts != tFrameTicks/2 || r.pts-r.dts != 2*tFrameTicks) {
					t.Fatalf("video PES %d: DTS step %d, PTS - DTS %d", i, r.dts-video[i-1].dts, r.pts-r.dts)
				}
			}
		})
	}
}

// TestLoopHEVCOpenGOP checks that an HEVC loop starting at a CRA turns it into
// a BLA in every lap.
func TestLoopHEVCOpenGOP(t *testing.T) {
	source := buildStream(t, streamOptions{seconds: 6, hevc: true})
	const laps = 3
	l, out, _ := loopData(t, source, LoopOptions{}, laps)
	if l.Report.Loop.AccessPoint != "open-gop" || l.Report.Lap.SeamMarks != 1 {
		t.Fatalf("loop %+v lap %+v", l.Report.Loop, l.Report.Lap)
	}
	if n := continuityErrors(out); n != 0 {
		t.Errorf("%d continuity errors", n)
	}
	var types []byte
	for _, r := range collectPES(out) {
		if r.pid != tVideoPID {
			continue
		}
		forEachStartCode(r.payload, func(pos int) bool {
			if typ := hevcNaluType(r.payload[pos]); typ < 32 {
				types = append(types, typ)
				return false
			}
			return true
		})
	}
	frames := l.Report.Loop.Pictures
	if len(types) != laps*frames {
		t.Fatalf("%d pictures", len(types))
	}
	for i, typ := range types {
		want := byte(1) // TRAIL_R
		switch {
		case i%frames == 0:
			want = hevcNaluBLAWLP
		case i%frames%25 == 0:
			want = hevcNaluCRA
		}
		if typ != want {
			t.Errorf("picture %d: NAL unit type %d, want %d", i, typ, want)
		}
	}
}

// TestLoopSCTE35EventIDs checks that each lap's SCTE-35 cues are new events at
// their lap's time: event IDs advance past the loop's, and pts_adjustment
// advances with the video.
func TestLoopSCTE35EventIDs(t *testing.T) {
	source := buildStream(t, streamOptions{seconds: 6, scte35: true})
	const laps = 4
	l, out, stats := loopData(t, source, LoopOptions{MaxDurationMS: 2000}, laps)
	if pids := l.Report.SCTE35PIDs; len(pids) != 1 || pids[0] != tSCTE35PID {
		t.Fatalf("SCTE-35 PIDs %v", pids)
	}
	type cue struct {
		id  uint32
		pts int64
	}
	var cues []cue
	var a sectionAssembler
	for i := l.Report.Lap.PreamblePackets; i < len(out)/PacketSize; i++ {
		if p := packetAt(out, i); tsPID(p) == tSCTE35PID {
			a.add(p, i*PacketSize)
		}
	}
	for _, s := range a.sections {
		msg, err := scte35.NewSCTE35(append([]byte{0}, s.Data...))
		if err != nil {
			t.Fatal(err)
		}
		if ins, ok := msg.CommandInfo().(scte35.SpliceInsertCommand); ok {
			cues = append(cues, cue{ins.EventID(), int64(msg.PTS())})
		}
		for _, d := range msg.Descriptors() {
			cues = append(cues, cue{d.EventID(), int64(msg.PTS())})
		}
	}
	if len(cues) == 0 || len(cues)%laps != 0 {
		t.Fatalf("%d cues in %d laps", len(cues), laps)
	}
	perLap := len(cues) / laps
	lo, hi := cues[0].id, cues[0].id
	for _, c := range cues[:perLap] {
		lo, hi = min(lo, c.id), max(hi, c.id)
	}
	step := hi - lo + 1
	if s := l.Report.Loop.SCTE35[0]; s.EventIDStep != step || s.Kept != perLap {
		t.Errorf("SCTE-35 plan %+v, want step %d and %d kept", s, step, perLap)
	}
	for lap := 0; lap < laps; lap++ {
		for k := 0; k < perLap; k++ {
			c, first := cues[lap*perLap+k], cues[k]
			if c.id != first.id+uint32(lap)*step || c.pts != (first.pts+int64(lap)*l.LoopDurationTicks())%PtsWrap {
				t.Errorf("lap %d cue %d: %+v from %+v", lap, k, c, first)
			}
		}
	}
	if stats.SCTE35Patched != (laps-1)*perLap { // one cue per section
		t.Errorf("%d sections patched", stats.SCTE35Patched)
	}
	if n := continuityErrors(out); n != 0 {
		t.Errorf("%d continuity errors", n)
	}
}

func TestScanFindsProgram(t *testing.T) {
	idx, err := scanData(t, buildStream(t, streamOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if idx.videoPID != tVideoPID || idx.videoCodec != CODEC_AVC || idx.pcrPID != tVideoPID || len(idx.pictures) != 75 {
		t.Fatalf("index: video %d %v, PCR %d, %d pictures", idx.videoPID, idx.videoCodec, idx.pcrPID, len(idx.pictures))
	}
	idr := 0
	for _, p := range idx.pictures {
		if p.info.accessPoint == apClosedGOP {
			idr++
		}
	}
	if idr != 3 || len(idx.continuityErrors) != 0 || len(idx.audio) != 2 {
		t.Fatalf("%d IDR, %d continuity errors, %d audio streams", idr, len(idx.continuityErrors), len(idx.audio))
	}
	for _, a := range idx.audio {
		if a.codec != audioADTS || !a.frameLevel || a.sampleRate != 48_000 {
			t.Errorf("audio PID %d: %v frame level %v at %d Hz", a.pid, a.codec, a.frameLevel, a.sampleRate)
		}
	}
	if a := idx.audio[0]; a.pid != tAudioPID || len(a.frames) < (len(a.pes)-1)*tAudioFramesPerPES {
		t.Errorf("audio PID %d: %d frames in %d PES", a.pid, len(a.frames), len(a.pes))
	}
	if a := idx.audio[1]; len(a.frames) != len(a.pes) {
		t.Errorf("audio PID %d: %d frames in %d PES", a.pid, len(a.frames), len(a.pes))
	}
}

// misplacePES makes the first payload unit of pid start with the end of a
// previous PES, not a header.
func misplacePES(data []byte, pid int) {
	for i := 0; i < len(data)/PacketSize; i++ {
		if p := packetAt(data, i); tsPID(p) == pid && tsPUSI(p) {
			o := tsPayloadOffset(p)
			p[o], p[o+1], p[o+2] = 0xff, 0xff, 0xff
			return
		}
	}
}

func TestMisplacedPES(t *testing.T) {
	source := buildStream(t, streamOptions{})
	misplacePES(source, tAudioPID)
	if _, err := scanData(t, source); err == nil || !strings.Contains(err.Error(), "PID 257") ||
		!strings.Contains(err.Error(), "cannot be looped") {
		t.Errorf("audio PES not at a packet start: %v", err)
	}
	// A private PES stream that does not start PES at TS packets is looped with a warning.
	source = buildStream(t, streamOptions{audio2StreamType: stPrivateData})
	misplacePES(source, tAudio2PID)
	idx, err := scanData(t, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.audio) != 1 || !anyHas(idx.warnings, "PID 258") || !anyHas(idx.warnings, "break at the seam") {
		t.Errorf("%d audio streams, warnings %v", len(idx.audio), idx.warnings)
	}
}

// TestScanContinuityErrors checks that a packet sent twice in a row is allowed
// and a lost one is an error.
func TestScanContinuityErrors(t *testing.T) {
	source := buildStream(t, streamOptions{})
	lost, repeated := -1, -1
	for i := 1000; i < len(source)/PacketSize && (lost < 0 || repeated < 0); i++ {
		p := packetAt(source, i)
		if !tsHasPayload(p) || tsPUSI(p) {
			continue
		}
		switch {
		case lost < 0 && tsPID(p) == tVideoPID:
			lost = i
		case repeated < 0 && tsPID(p) == tAudioPID:
			repeated = i
		}
	}
	dup := append([]byte(nil), packetAt(source, repeated)...)
	source = append(source[:(repeated+1)*PacketSize], append(dup, source[(repeated+1)*PacketSize:]...)...)
	removed := lost
	if lost > repeated {
		removed++
	}
	source = append(source[:removed*PacketSize], source[(removed+1)*PacketSize:]...)
	idx, err := scanData(t, source)
	if err != nil {
		t.Fatal(err)
	}
	if e := idx.continuityErrors; len(e) != 1 || e[0].pid != tVideoPID || e[0].packet != removed {
		t.Fatalf("continuity errors %+v, want one on the video at packet %d", e, removed)
	}
	if !anyHas(idx.warnings, "1 continuity errors") {
		t.Errorf("warnings %v", idx.warnings)
	}
}

func TestBTSRefused(t *testing.T) {
	if _, err := scanData(t, asBTS(buildStream(t, streamOptions{seconds: 1}))); err == nil ||
		!strings.Contains(err.Error(), "BTS") {
		t.Errorf("BTS: %v", err)
	}
}

func TestInputNotTS(t *testing.T) {
	for name, data := range map[string][]byte{"short": {0x47, 0, 0}, "no sync": make([]byte, 10*PacketSize)} {
		if _, err := scanData(t, data); err == nil || !strings.Contains(err.Error(), "188-byte TS") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := PrepareLoop(context.Background(), t.TempDir(), LoopOptions{}); err == nil {
		t.Error("a directory is not a TS")
	}
}

func TestTwoProgramsWarned(t *testing.T) {
	idx, err := scanData(t, buildStream(t, streamOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if anyHas(idx.warnings, "in the PAT") {
		t.Errorf("one program: %v", idx.warnings)
	}
	idx, err = scanData(t, buildStream(t, streamOptions{secondProgram: true}))
	if err != nil {
		t.Fatal(err)
	}
	if idx.programNumber != 1 || !anyHas(idx.warnings, "only program 1 of the 2 in the PAT") ||
		!anyHas(idx.warnings, "program 2 is cut at the same points, not its own") {
		t.Errorf("program %d, warnings %v", idx.programNumber, idx.warnings)
	}
}

// TestOversizedAdaptationField checks that a packet whose adaptation field is
// longer than the packet becomes a null packet.
func TestOversizedAdaptationField(t *testing.T) {
	source := buildStream(t, streamOptions{seconds: 6})
	corrupted := -1
	for i := len(source) / PacketSize / 2; i < len(source)/PacketSize; i++ {
		if p := packetAt(source, i); tsPID(p) == tAudioPID && tsHasPayload(p) && !tsPUSI(p) {
			p[3] |= 0x20
			p[4], p[5] = 255, afFlagPCR
			corrupted = i
			break
		}
	}
	if corrupted < 0 {
		t.Fatal("no audio packet to corrupt")
	}
	idx, err := scanData(t, source)
	if err != nil {
		t.Fatal(err)
	}
	if !anyHas(idx.warnings, "1 packets have an adaptation field longer than the packet") {
		t.Errorf("warnings %v", idx.warnings)
	}
	_, out, _ := loopData(t, source, LoopOptions{MaxDurationMS: 4000}, 3)
	for i := 0; i < len(out)/PacketSize; i++ {
		if tsOversizedAF(packetAt(out, i)) {
			t.Fatalf("output packet %d has an oversized adaptation field", i)
		}
	}
}

func TestPictureRateRefused(t *testing.T) {
	source := buildStream(t, streamOptions{})
	if _, _, _ = loopData(t, source, LoopOptions{}, 1); t.Failed() {
		return
	}
	counting := countingDTS(source)
	_, err := prepareLoop(context.Background(), "test", counting, LoopOptions{})
	if err == nil || !strings.Contains(err.Error(), "1 tick a picture, 90000 pictures a second") ||
		strings.Contains(err.Error(), "PCR") {
		t.Errorf("counting DTS: %v", err)
	}
	// -fps does not make the DTS a decoding time.
	_, err = prepareLoop(context.Background(), "test", counting, LoopOptions{FrameRate: FrameRate{25, 1}})
	if err == nil || !strings.Contains(err.Error(), "pictures a second") {
		t.Errorf("counting DTS with -fps: %v", err)
	}
}

// TestPCRAgainstVideoRefused checks that a PCR running 1500 times too slow, as
// when it follows a DTS that counts pictures at 59.94 fps, is refused before a
// lap of gigabytes is built.
func TestPCRAgainstVideoRefused(t *testing.T) {
	source := variableRate(buildStream(t, streamOptions{seconds: 6}), 1.0/1500)
	l, err := prepareLoop(context.Background(), "test", source, LoopOptions{})
	if err == nil || !strings.Contains(err.Error(), "the PCR does not keep time with the video") ||
		strings.Contains(err.Error(), "pictures a second") {
		t.Errorf("slow PCR: %v", err)
	}
	if l == nil || l.Report.Error == "" || l.Report.Loop != nil {
		t.Errorf("the report should carry the error and no loop: %+v", l)
	}
}

// TestLoopSeparatePCRPID loops a stream whose PCR is carried on its own PID in
// adaptation-field-only packets: the plan follows the PMT's PCR_PID, its
// continuity counter does not move, and the output PCR is linear across laps.
func TestLoopSeparatePCRPID(t *testing.T) {
	const pcrPID = 0x1f0
	source := buildStream(t, streamOptions{seconds: 6})
	for i := 0; i < len(source)/PacketSize; i++ {
		p := packetAt(source, i)
		switch {
		case tsPID(p) == tVideoPID && tsCarriesPCR(p) && !tsHasPayload(p):
			p[1], p[2], p[3] = p[1]&0xe0|pcrPID>>8, pcrPID&0xff, p[3]&0xf0 // without payload, its counter stays
		case tsPID(p) == tPMTPID && tsPUSI(p):
			sec := p[tsPayloadOffset(p)+1:]
			n := 3 + (int(sec[1]&0x0f)<<8 | int(sec[2]))
			sec[8], sec[9] = sec[8]&0xe0|pcrPID>>8, pcrPID&0xff
			copy(sec[n-4:], gots.ComputeCRC(sec[:n-4]))
		}
	}
	idx, err := scanData(t, source)
	if err != nil {
		t.Fatal(err)
	}
	if idx.pcrPID != pcrPID {
		t.Fatalf("PCR PID %d", idx.pcrPID)
	}
	const laps = 3
	l, out, _ := loopData(t, source, LoopOptions{MaxDurationMS: 2000}, laps)
	if !l.Report.Loop.ConstantRate {
		t.Fatalf("loop %+v", l.Report.Loop)
	}
	n := int64(l.seg.packetCount())
	lapTime := l.LoopDurationTicks() * pcrPerTick
	pre := l.Report.Lap.PreamblePackets
	prev, prevPCR, count := -1, int64(0), 0
	cc := -1
	for i := pre; i < len(out)/PacketSize; i++ {
		p := packetAt(out, i)
		if !tsCarriesPCR(p) {
			continue
		}
		if pid := tsPID(p); pid != pcrPID {
			t.Fatalf("packet %d: PCR on PID %d", i, pid)
		}
		if c := int(tsCC(p)); cc >= 0 && c != cc {
			t.Errorf("packet %d: CC %d on a payload-less PCR packet, want %d", i, c, cc)
		} else {
			cc = c
		}
		pcr := int64(readPCR(p[pcrFieldOffset:]))
		// Packet d of the output after the preamble is due at this time.
		due := func(d int) int64 { return int64(d)/n*lapTime + evenTime(d%int(n), int(n), lapTime) }
		if prev >= 0 {
			if want := due(i-pre) - due(prev-pre); pcr-prevPCR != want {
				t.Errorf("packet %d: PCR step %d, want %d", i, pcr-prevPCR, want)
			}
		}
		prev, prevPCR = i, pcr
		count++
	}
	if count < laps*10 {
		t.Errorf("only %d PCRs in %d laps", count, laps)
	}
}

// picturesAndData returns the PTS of the output pictures in presentation order,
// and the data PES in the order they are sent.
func picturesAndData(out []byte) ([]int64, []*pesRecord) {
	var pictures []int64
	var data []*pesRecord
	for _, r := range collectPES(out) {
		switch r.pid {
		case tVideoPID:
			pictures = append(pictures, r.pts)
		case tDataPID:
			data = append(data, r)
		}
	}
	slices.Sort(pictures)
	return pictures, data
}

// dataSourcePTS reads the PTS a test data PES had in the source, from its payload.
func dataSourcePTS(r *pesRecord) int64 { return int64(binary.BigEndian.Uint32(r.payload)) }

func countPayloadPackets(stream []byte, pid, begin, end int) int {
	n := 0
	for i := begin; i < end; i++ {
		if p := packetAt(stream, i); tsPID(p) == pid && tsHasPayload(p) {
			n++
		}
	}
	return n
}

// checkDataOfPictures checks that the data PES are those of the pictures in
// withData, one each in presentation order, each with its source PTS advanced
// by the laps before its own.
func checkDataOfPictures(t *testing.T, data []*pesRecord, withData []int64, firstPTS, duration int64) {
	t.Helper()
	if len(data) != len(withData) {
		t.Fatalf("%d data PES for %d pictures", len(data), len(withData))
	}
	for k, r := range data {
		lap := (r.pts - firstPTS) / duration
		if r.pts != withData[k] || dataSourcePTS(r)+lap*duration != r.pts {
			t.Fatalf("data PES %d at %d from %d, for the picture at %d", k, r.pts, dataSourcePTS(r), withData[k])
		}
	}
}

// TestLoopDataByPTS checks that data PES multiplexed three frames before their
// picture, as some encoders send ANC, or three after, play with the picture of
// their PTS across every seam.
func TestLoopDataByPTS(t *testing.T) {
	for _, ahead := range []int{3, -3} {
		t.Run(fmt.Sprintf("%d frames ahead", ahead), func(t *testing.T) {
			source := buildStream(t, streamOptions{seconds: 6, data: true, dataFramesAhead: ahead})
			const laps = 4
			l, out, stats := loopData(t, source, LoopOptions{MaxDurationMS: 2000}, laps)
			plan := l.chosen
			if anyHas(l.Report.Warnings, "PID 259") {
				t.Errorf("warnings %v", l.Report.Warnings)
			}
			if d := l.Report.Loop.Data; len(d) != 1 || !d[0].PlacedByLap || d[0].PES != plan.frames {
				t.Errorf("data fits %+v", d)
			}
			if stats.MovedDataPackets == 0 {
				t.Errorf("no data moved across seams: %+v", stats)
			}
			if n := continuityErrors(out); n != 0 {
				t.Errorf("%d continuity errors", n)
			}
			// Every picture, the first one too, has its own data PES and no other,
			// and no data PES comes for a picture after the last.
			pictures, data := picturesAndData(out)
			if len(pictures) != laps*plan.frames {
				t.Fatalf("%d pictures", len(pictures))
			}
			checkDataOfPictures(t, data, pictures, pictures[0], plan.duration)
		})
	}
}

// TestLoopDataMissingAtLoopEnds checks that the data PES a file lacks at one
// end of the loop are missing from every lap, with a warning: a file trimmed to
// start at the loop lacks the data sent before its first picture, one trimmed
// to end at it the data sent after its last. No other picture's PES stands in.
func TestLoopDataMissingAtLoopEnds(t *testing.T) {
	for _, ahead := range []int{3, -3} {
		t.Run(fmt.Sprintf("%d frames ahead", ahead), func(t *testing.T) {
			source := buildStream(t, streamOptions{seconds: 6, data: true, dataFramesAhead: ahead})
			idx, err := scanData(t, source)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := chooseLoop(idx, loopOptions{maxDuration: 2 * ticksPerSec})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < len(source)/PacketSize; i++ {
				outside := i >= plan.endPacket
				if ahead > 0 {
					outside = i < plan.startPacket
				}
				if p := packetAt(source, i); tsPID(p) == tDataPID && outside {
					writeNullPacket(p)
				}
			}

			const laps = 4
			l, out, _ := loopData(t, source, LoopOptions{MaxDurationMS: 2000}, laps)
			if l.chosen.startPacket != plan.startPacket || l.chosen.endPacket != plan.endPacket {
				t.Fatalf("loop %+v, want packets %d..%d", l.Report.Loop, plan.startPacket, plan.endPacket)
			}
			const missing = 3
			d := l.Report.Loop.Data
			if len(d) != 1 || (ahead > 0 && d[0].MissingAtStart != missing) || (ahead < 0 && d[0].MissingAtEnd != missing) {
				t.Errorf("data fits %+v", d)
			}
			if !anyHas(l.Report.Warnings, "data PID 259") {
				t.Errorf("warnings %v", l.Report.Warnings)
			}
			if n := continuityErrors(out); n != 0 {
				t.Errorf("%d continuity errors", n)
			}
			pictures, data := picturesAndData(out)
			if len(pictures) != laps*plan.frames {
				t.Fatalf("%d pictures", len(pictures))
			}
			var withData []int64
			for i, pts := range pictures {
				inLap := i % plan.frames
				if (ahead > 0 && inLap >= missing) || (ahead < 0 && inLap < plan.frames-missing) {
					withData = append(withData, pts)
				}
			}
			checkDataOfPictures(t, data, withData, pictures[0], plan.duration)
		})
	}
}

// TestLoopDataWithoutPTS checks that a data stream whose PES carry no PTS stays
// where the source sends it, with a warning.
func TestLoopDataWithoutPTS(t *testing.T) {
	source := buildStream(t, streamOptions{seconds: 6, data: true, dataFramesAhead: 3, dataWithoutPTS: true})
	const laps = 2
	l, out, _ := loopData(t, source, LoopOptions{MaxDurationMS: 2000}, laps)
	if d := l.Report.Data; len(d) != 1 || d[0].Timing != "a PES without a PTS" || l.Report.Loop.Data[0].PlacedByLap {
		t.Errorf("data %+v, fits %+v", d, l.Report.Loop.Data)
	}
	if !anyHas(l.Report.Warnings, "data PID 259") {
		t.Errorf("warnings %v", l.Report.Warnings)
	}
	if n := continuityErrors(out); n != 0 {
		t.Errorf("%d continuity errors", n)
	}
	window := countPayloadPackets(source, tDataPID, l.chosen.startPacket, l.chosen.endPacket)
	if n := countPayloadPackets(out, tDataPID, 0, len(out)/PacketSize); window == 0 || n != laps*window {
		t.Errorf("%d data packets in %d laps of %d", n, laps, window)
	}
}

// TestLoopAudioHeadGap checks that audio a file lacks at the loop's start leaves
// that gap in every lap, with a warning: audio sent 150 ms before its video, in
// a file that holds none before the loop. No audio from after the loop's end
// stands in for it. With an IDR every 50 frames in 4 s, this is the only loop,
// so it is chosen despite its audio.
func TestLoopAudioHeadGap(t *testing.T) {
	source := buildStream(t, streamOptions{seconds: 4, audioMuxDelay: -13_500, gop: 50})
	idx, err := scanData(t, source)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := chooseLoop(idx, loopOptions{maxDuration: 2 * ticksPerSec})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < plan.startPacket; i++ {
		if p := packetAt(source, i); tsPID(p) == tAudioPID || tsPID(p) == tAudio2PID {
			writeNullPacket(p)
		}
	}

	const laps = 4
	l, out, stats := loopData(t, source, LoopOptions{MaxDurationMS: 2000}, laps)
	if l.chosen.startPacket != plan.startPacket || l.chosen.endPacket != plan.endPacket {
		t.Fatalf("loop %+v, want packets %d..%d", l.Report.Loop, plan.startPacket, plan.endPacket)
	}
	if !anyHas(l.Report.Warnings, "start the file earlier") {
		t.Errorf("warnings %v", l.Report.Warnings)
	}
	if stats.AudioGaps != laps*len(l.seg.audio) {
		t.Errorf("stats %+v", stats)
	}
	if n := continuityErrors(out); n != 0 {
		t.Errorf("%d continuity errors", n)
	}

	loopStart, duration := idx.pictures[plan.startPicture].pts, plan.duration
	audio := make(map[int][]*pesRecord)
	for _, r := range collectPES(out) {
		if r.pid == tAudioPID || r.pid == tAudio2PID {
			audio[r.pid] = append(audio[r.pid], r)
		}
	}
	if len(audio) != 2 {
		t.Fatalf("%d audio streams", len(audio))
	}
	for pid, recs := range audio {
		// Every frame plays its own time of the loop, up to a frame late, never
		// audio from after the loop's end; so no lap has audio for the time the
		// file lacks at the loop's start.
		type frame struct{ out, src int64 }
		var frames []frame
		firstSource := int64(math.MaxInt64)
		for _, r := range recs {
			for k := 0; k+tAudioFrameSize <= len(r.payload); k += tAudioFrameSize {
				f := frame{r.pts + int64(k/tAudioFrameSize)*tAudioFrameTicks, sourcePTS(r.payload[k:])}
				frames = append(frames, f)
				firstSource = min(firstSource, f.src)
			}
		}
		missing := firstSource - loopStart
		if missing <= tAudioFrameTicks/2 {
			t.Errorf("audio PID %d: the file lacks only %d ticks", pid, missing)
		}
		for _, f := range frames {
			shift := f.out - f.src
			if f.src >= loopStart+duration || shift < 0 || shift%duration >= tAudioFrameTicks ||
				(f.out-loopStart)%duration < missing {
				t.Fatalf("audio PID %d: frame from %d plays at %d", pid, f.src, f.out)
			}
		}
	}
}

// TestLoopLeadingPictures checks that every stream loops the PTS interval of the
// loop's pictures, which starts before its IDR when pictures decoded after it
// are shown first.
func TestLoopLeadingPictures(t *testing.T) {
	source := buildStream(t, streamOptions{seconds: 6, data: true, dataFramesAhead: 3, leadingPictures: true})
	idx, err := scanData(t, source)
	if err != nil {
		t.Fatal(err)
	}
	const laps = 4
	l, out, _ := loopData(t, source, LoopOptions{MaxDurationMS: 2000}, laps)
	plan := l.chosen
	if plan.accessPoint != apClosedGOP {
		t.Fatalf("loop %+v", l.Report.Loop)
	}
	firstPTS := int64(math.MaxInt64)
	for _, p := range idx.pictures[plan.startPicture:plan.endPicture] {
		firstPTS = min(firstPTS, p.pts)
	}
	if firstPTS != idx.pictures[plan.startPicture].pts-2*tFrameTicks || plan.firstPTS != firstPTS ||
		l.Report.Loop.FirstPTS != firstPTS {
		t.Fatalf("loop %+v, its pictures start at %d", l.Report.Loop, firstPTS)
	}
	if anyHas(l.Report.Warnings, "audio PID") || anyHas(l.Report.Warnings, "data PID") {
		t.Errorf("warnings %v", l.Report.Warnings)
	}
	if n := continuityErrors(out); n != 0 {
		t.Errorf("%d continuity errors", n)
	}

	// Every picture, the ones shown before the IDR too, has the data of its own
	// source picture.
	pictures, data := picturesAndData(out)
	if len(pictures) != laps*plan.frames {
		t.Fatalf("%d pictures", len(pictures))
	}
	checkDataOfPictures(t, data, pictures, pictures[0], plan.duration)

	// Every audio frame comes from the same interval, up to a frame late.
	for _, r := range collectPES(out) {
		if r.pid != tAudioPID && r.pid != tAudio2PID {
			continue
		}
		for k := 0; k+tAudioFrameSize <= len(r.payload); k += tAudioFrameSize {
			src := sourcePTS(r.payload[k:])
			shift := r.pts + int64(k/tAudioFrameSize)*tAudioFrameTicks - src
			if src < firstPTS-tAudioFrameTicks/2 || src >= firstPTS+plan.duration || shift < 0 ||
				shift%plan.duration >= tAudioFrameTicks {
				t.Fatalf("audio PID %d: frame from %d shifted %d", r.pid, src, shift)
			}
		}
	}
}

// audioContinuation returns the first packet from packet from on that continues
// a PES of the test stream's first audio.
func audioContinuation(t *testing.T, stream []byte, from int) int {
	t.Helper()
	for i := from; i < len(stream)/PacketSize; i++ {
		if p := packetAt(stream, i); tsPID(p) == tAudioPID && tsHasPayload(p) && !tsPUSI(p) {
			return i
		}
	}
	t.Fatal("no audio packet")
	return -1
}

// TestLostAudioPacket checks that an audio PES that lost a packet is one frame
// no lap ends inside, and that the rest of the audio is still cut at frames:
// outside the loop it changes nothing, inside it comes back every lap.
func TestLostAudioPacket(t *testing.T) {
	source := buildStream(t, streamOptions{seconds: 6})
	opts := LoopOptions{MaxDurationMS: 2000}
	clean, cleanOut, _ := loopData(t, source, opts, 3)
	lose := func(from int) []byte {
		damaged := append([]byte(nil), source...)
		writeNullPacket(packetAt(damaged, audioContinuation(t, damaged, from)))
		return damaged
	}

	outside := lose(clean.Report.Loop.EndPacket + 2000)
	idx, err := scanData(t, outside)
	if err != nil {
		t.Fatal(err)
	}
	a := idx.audio[0]
	var damaged []audioPES
	for _, p := range a.pes {
		if p.damaged {
			damaged = append(damaged, p)
		}
	}
	if !a.frameLevel || len(damaged) != 1 || !anyHas(idx.warnings, "the frames of 1 of") {
		t.Fatalf("frame level %v, %d damaged PES, warnings %v", a.frameLevel, len(damaged), idx.warnings)
	}
	// It lasts until the next PES, three frames on.
	if f := a.frames[damaged[0].firstFrame]; damaged[0].frameCount != 1 ||
		f.duration != tAudioFramesPerPES*tAudioFrameTicks*a.timeScale || f.start != damaged[0].pts*a.timeScale {
		t.Errorf("damaged PES %+v with frame %+v", damaged[0], f)
	}
	l, out, _ := loopData(t, outside, opts, 3)
	if l.Report.Audio[0].CutAt != "frame" || anyHas(l.Report.Warnings, "come back every lap") {
		t.Errorf("audio %+v, warnings %v", l.Report.Audio[0], l.Report.Warnings)
	}
	if !bytes.Equal(out, cleanOut) {
		t.Error("a packet lost outside the loop changes the laps")
	}

	inside := lose((clean.Report.Loop.StartPacket + clean.Report.Loop.EndPacket) / 2)
	l, out, _ = loopData(t, inside, opts, 3)
	if l.Report.Audio[0].CutAt != "frame" || !anyHas(l.Report.Warnings, "audio PID 257: 1 PES whose frames cannot be read") ||
		l.Report.Loop.Audio[0].SeamOffsetMS != clean.Report.Loop.Audio[0].SeamOffsetMS {
		t.Errorf("audio %+v, in the loop %+v, warnings %v", l.Report.Audio[0], l.Report.Loop.Audio[0], l.Report.Warnings)
	}
	if n := continuityErrors(out); n != 0 {
		t.Errorf("%d continuity errors", n)
	}
}

// TestUnalignedAudio checks that audio whose frames mostly cannot be read, as
// when they straddle PES, is cut at PES boundaries.
func TestUnalignedAudio(t *testing.T) {
	source := buildStream(t, streamOptions{seconds: 6})
	// Each PES of the first audio loses its first continuation packet.
	for i := 0; i < len(source)/PacketSize; i++ {
		if p := packetAt(source, i); tsPID(p) == tAudioPID && tsPUSI(p) {
			writeNullPacket(packetAt(source, audioContinuation(t, source, i)))
		}
	}
	l, _, _ := loopData(t, source, LoopOptions{MaxDurationMS: 2000}, 1)
	if l.Report.Audio[0].CutAt != "pes" || l.Report.Audio[1].CutAt != "frame" ||
		!anyHas(l.Report.Warnings, "laps end on PES boundaries") {
		t.Errorf("audio %+v, warnings %v", l.Report.Audio, l.Report.Warnings)
	}
}

// TestDuplicateAudioPacket checks that an audio packet sent twice, which is
// allowed, is not two packets of payload in the laps.
func TestDuplicateAudioPacket(t *testing.T) {
	source := buildStream(t, streamOptions{seconds: 6})
	opts := LoopOptions{MaxDurationMS: 2000}
	clean, _, _ := loopData(t, source, opts, 1)
	// The copy takes a null packet before the PID's next packet.
	repeated := -1
	for i := (clean.Report.Loop.StartPacket + clean.Report.Loop.EndPacket) / 2; repeated < 0; i++ {
		i = audioContinuation(t, source, i)
		for j := i + 1; tsPID(packetAt(source, j)) != tAudioPID; j++ {
			if tsPID(packetAt(source, j)) == nullPID {
				copy(packetAt(source, j), packetAt(source, i))
				repeated = j
				break
			}
		}
	}
	if repeated >= clean.Report.Loop.EndPacket {
		t.Fatalf("the repeated packet %d is outside the loop", repeated)
	}
	l, out, _ := loopData(t, source, opts, 3)
	if len(l.Report.ContinuityErrors) != 0 || l.Report.Audio[0].CutAt != "frame" || len(l.Report.Warnings) != 0 {
		t.Errorf("continuity errors %v, audio %+v, warnings %v", l.Report.ContinuityErrors, l.Report.Audio[0], l.Report.Warnings)
	}
	if n := continuityErrors(out); n != 0 {
		t.Errorf("%d continuity errors", n)
	}
	for _, r := range collectPES(out) {
		if r.pid != tAudioPID {
			continue
		}
		if len(r.payload) == 0 || len(r.payload)%tAudioFrameSize != 0 {
			t.Fatalf("audio PES at packet %d has %d bytes, not whole frames of %d", r.packet, len(r.payload), tAudioFrameSize)
		}
		for off := 0; off < len(r.payload); off += tAudioFrameSize {
			if !bytes.Equal(r.payload[off:off+2], []byte{0xff, 0xf1}) {
				t.Fatalf("audio PES at packet %d has no frame at byte %d", r.packet, off)
			}
		}
	}
}

// lapEndOutput ends the run once it has taken a number of packets.
type lapEndOutput struct {
	out    bytes.Buffer
	cancel context.CancelFunc
	after  int
}

func (o *lapEndOutput) Write(_ context.Context, packets []byte, _ func(int) int64) error {
	o.out.Write(packets)
	if o.out.Len() >= o.after*PacketSize {
		o.cancel()
	}
	return nil
}

func (o *lapEndOutput) Flush() error { return nil }

type failingOutput struct{ err error }

func (o failingOutput) Write(context.Context, []byte, func(int) int64) error { return o.err }
func (o failingOutput) Flush() error                                         { return nil }

// TestWriteLapsInChunks loops a lap of several chunks: an endless run that is
// stopped ends an unpaced output at the end of a lap, with the packets of a
// bounded run, and an output that fails ends the run with its error.
func TestWriteLapsInChunks(t *testing.T) {
	source := buildStream(t, streamOptions{seconds: 12})
	l, bounded, _ := loopData(t, source, LoopOptions{}, 4)
	lapPackets := l.seg.lapPackets
	if lapPackets < 2*lapChunkPackets || l.seg.packetCount() != lapPackets {
		t.Fatalf("a lap of %d packets (%d at the source rate), chunks of %d", l.seg.packetCount(), lapPackets, lapChunkPackets)
	}
	if n := continuityErrors(bounded); n != 0 {
		t.Errorf("%d continuity errors", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := &lapEndOutput{cancel: cancel, after: lapPackets + lapPackets/2}
	if _, err := l.WriteLaps(ctx, []LapOutput{o}, 0); err != nil {
		t.Fatal(err)
	}
	out := o.out.Bytes()
	preamble := 0
	for tsPID(packetAt(out, preamble)) != tVideoPID {
		preamble++
	}
	if got := len(out)/PacketSize - preamble; got != 2*lapPackets {
		t.Errorf("stopped in the second lap: %d packets after the preamble, want two laps of %d", got, lapPackets)
	}
	if !bytes.Equal(out, bounded[:len(out)]) {
		t.Error("the laps of the stopped run differ from those of a bounded run")
	}

	failure := errors.New("no room")
	if _, err := l.WriteLaps(context.Background(), []LapOutput{failingOutput{failure}}, 0); !errors.Is(err, failure) {
		t.Errorf("error %v, want the output's", err)
	}
}
