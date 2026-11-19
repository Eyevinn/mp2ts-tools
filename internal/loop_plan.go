package internal

import (
	"cmp"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	// maxPCRInterval: consecutive PCRs further apart are a discontinuity,
	// signalled or not.
	maxPCRInterval = pcrPerSec
	// constantRateTolerance: a PCR this close to a constant rate is constant
	// rate: well above PCR jitter, far below the hundreds of ms a variable-rate
	// encoder drifts.
	constantRateTolerance = pcrPerSec / 1000
	// maxPictureRate: the fastest nominal rate, 120 fps, is 240 pictures a
	// second as fields.
	maxPictureRate = 300
	// maxBufferDelay and clockDriftDivisor bound how far the PCR and the video
	// PTS may part over a span: the decoder buffer delay, at most 1 s in the
	// T-STD of ISO/IEC 13818-1, plus a tenth of the span for a PCR clock that
	// drifts from the encoder's. A PCR that follows a DTS counting pictures runs
	// 1500 times too slow.
	maxBufferDelay    = pcrPerSec
	clockDriftDivisor = 10
	// maxSeamDTSJump: a loop whose video DTS would jump further at every seam is
	// no loop: its DTS parts from the PTS by more than the decoder buffer holds.
	maxSeamDTSJump = ticksPerSec
	// maxEvaluatedPairs of access points are looked at before the search settles
	// for the best so far, a few seconds: an hour of intra-only video has billions.
	maxEvaluatedPairs = 16_000_000
)

func allowedClockSkew(span int64) int64 { return maxBufferDelay + span/clockDriftDivisor }

func pcrSeconds(pcrTicks int64) float64 { return float64(pcrTicks) / pcrPerSec }

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// FrameRate is a rational frame rate Num/Den frames per second; zero is unknown.
type FrameRate struct {
	Num int64
	Den int64
}

func (r FrameRate) known() bool { return r.Num != 0 && r.Den != 0 }

func (r FrameRate) String() string {
	if !r.known() {
		return "unknown"
	}
	if r.Den == 1 {
		return strconv.FormatInt(r.Num, 10)
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

// ParseFrameRate parses a frame rate such as "25", "29.97", "59.94", "23.976"
// or "30000/1001".
func ParseFrameRate(s string) (FrameRate, error) {
	s = strings.TrimSpace(s)
	number := func(d string) (int64, bool) {
		v, err := strconv.ParseUint(d, 10, 32)
		return int64(v), err == nil && v != 0
	}
	if n, d, ok := strings.Cut(s, "/"); ok {
		num, ok1 := number(n)
		den, ok2 := number(d)
		if !ok1 || !ok2 {
			return FrameRate{}, fmt.Errorf("invalid frame rate %q", s)
		}
		return FrameRate{num, den}, nil
	}
	switch s {
	case "23.976", "23.98":
		return FrameRate{24000, 1001}, nil
	case "29.97":
		return FrameRate{30000, 1001}, nil
	case "59.94":
		return FrameRate{60000, 1001}, nil
	case "119.88":
		return FrameRate{120000, 1001}, nil
	}
	if n, ok := number(s); ok {
		return FrameRate{n, 1}, nil
	}
	return FrameRate{}, fmt.Errorf("invalid frame rate %q", s)
}

var nominalFrameRates = []FrameRate{
	{24000, 1001}, {24, 1}, {25, 1}, {30000, 1001}, {30, 1}, {48, 1},
	{50, 1}, {60000, 1001}, {60, 1}, {100, 1}, {120000, 1001}, {120, 1},
}

// medianDTSStep is the median step from one picture's DTS to the next.
func medianDTSStep(pics []videoPicture) (int64, bool) {
	if len(pics) < 2 {
		return 0, false
	}
	steps := make([]int64, 0, len(pics)-1)
	for i := 1; i < len(pics); i++ {
		steps = append(steps, pics[i].dts-pics[i-1].dts)
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i] < steps[j] })
	return steps[len(steps)/2], true
}

// detectFrameRate returns the nominal frame rate closest to the median time a
// picture is shown per frame period it lasts. A picture is shown from its PTS
// to the next in display order, for as many field periods as it has, two a
// frame: a stream of field pictures gives its frame rate, not its field rate,
// and one whose frames repeat fields for pulldown the rate its frames are shown
// at. Failing that, the median DTS step gives the decoding rate.
func detectFrameRate(pics []videoPicture) (FrameRate, bool) {
	type shown struct{ pts, fields int64 }
	order := make([]shown, len(pics))
	for i := range pics {
		order[i] = shown{pics[i].pts, pics[i].info.fieldPeriods()}
	}
	slices.SortFunc(order, func(a, b shown) int { return cmp.Compare(a.pts, b.pts) })
	var periods []float64
	for k := 1; k < len(order); k++ {
		if step := order[k].pts - order[k-1].pts; step > 0 {
			periods = append(periods, float64(step)*2/float64(order[k-1].fields))
		}
	}
	if r, ok := nominalFrameRate(periods); ok {
		return r, true
	}
	// Timestamps that do not follow how long pictures are shown, such as
	// fields a frame period apart: the decoding rate.
	periods = periods[:0]
	for i := 1; i < len(pics); i++ {
		if step := pics[i].dts - pics[i-1].dts; step > 0 {
			periods = append(periods, float64(step))
		}
	}
	return nominalFrameRate(periods)
}

// nominalFrameRate returns the nominal frame rate whose frame period is closest
// to the median of periods (90 kHz), if within 0.5% of it: 30 and 29.97 fps are
// only 0.1% apart.
func nominalFrameRate(periods []float64) (FrameRate, bool) {
	if len(periods) == 0 {
		return FrameRate{}, false
	}
	slices.Sort(periods)
	median := periods[len(periods)/2]
	var closest FrameRate
	found := false
	closestErr := 0.005
	for _, r := range nominalFrameRates {
		period := ticksPerSec * float64(r.Den) / float64(r.Num)
		if e := math.Abs(period-median) / period; e <= closestErr {
			closest, closestErr, found = r, e, true
		}
	}
	return closest, found
}

// checkPictureRate refuses a DTS step far below any frame period: no decoding
// time. A muxer that takes a picture counter for the DTS writes one tick a picture.
func checkPictureRate(pics []videoPicture) string {
	step, ok := medianDTSStep(pics)
	if !ok || step*maxPictureRate >= ticksPerSec {
		return ""
	}
	least, most := int64(math.MaxInt64), int64(math.MinInt64)
	for _, p := range pics {
		least, most = min(least, p.pts-p.dts), max(most, p.pts-p.dts)
	}
	delay := fmt.Sprintf("; PTS - DTS goes from %.3f s to %.3f s", float64(least)/ticksPerSec, float64(most)/ticksPerSec)
	if step <= 0 {
		return fmt.Sprintf("the video DTS does not advance from picture to picture (median step %d ticks)%s", step, delay)
	}
	plural := "s"
	if step == 1 {
		plural = ""
	}
	return fmt.Sprintf("the video DTS advances %d tick%s a picture, %d pictures a second, so it is not a decoding time "+
		"(at most %d pictures a second are)%s", step, plural, ticksPerSec/step, maxPictureRate, delay)
}

// pcrStretch is a run of PCR samples without a discontinuity, with its rate and
// how far it is from a constant rate.
type pcrStretch struct {
	firstSample, endSample int
	ticksPerPacket         float64 // 27 MHz
	deviation              int64   // 27 MHz: largest distance from a constant rate
}

// pcrTimeline is the PCR of the program as stretches without discontinuities.
type pcrTimeline struct {
	samples         []pcrSample
	stretches       []pcrStretch
	stretchOfSample []int
}

func newPCRTimeline(idx *streamIndex) *pcrTimeline {
	t := &pcrTimeline{samples: idx.pcrSamples}
	disc := idx.discontinuities
	for k := range t.samples {
		newStretch := len(t.stretches) == 0
		if !newStretch {
			prev, cur := t.samples[k-1], t.samples[k]
			step := cur.pcr - prev.pcr
			sig := sort.SearchInts(disc, prev.packet+1)
			newStretch = step < 0 || step > maxPCRInterval || (sig < len(disc) && disc[sig] <= cur.packet)
		}
		if newStretch {
			t.stretches = append(t.stretches, pcrStretch{firstSample: k})
		}
		t.stretches[len(t.stretches)-1].endSample = k + 1
		t.stretchOfSample = append(t.stretchOfSample, len(t.stretches)-1)
	}
	for i := range t.stretches {
		s := &t.stretches[i]
		first, last := t.samples[s.firstSample], t.samples[s.endSample-1]
		if last.packet == first.packet {
			continue
		}
		s.ticksPerPacket = float64(last.pcr-first.pcr) / float64(last.packet-first.packet)
		for k := s.firstSample; k < s.endSample; k++ {
			cur := t.samples[k]
			constant := float64(first.pcr) + s.ticksPerPacket*float64(cur.packet-first.packet)
			s.deviation = max(s.deviation, int64(math.Round(math.Abs(float64(cur.pcr)-constant))))
		}
	}
	return t
}

// stretchOf returns the stretch that times packet: the one holding the last PCR
// at or before it, or the first one for packets before any PCR; nil if that
// stretch has fewer than two PCRs.
func (t *pcrTimeline) stretchOf(packet int) *pcrStretch {
	if len(t.samples) == 0 {
		return nil
	}
	after := sort.Search(len(t.samples), func(k int) bool { return t.samples[k].packet > packet })
	k := max(after-1, 0)
	s := &t.stretches[t.stretchOfSample[k]]
	if s.endSample-s.firstSample < 2 {
		return nil
	}
	return s
}

// at returns the PCR packet would carry, interpolated or extrapolated within s.
func (t *pcrTimeline) at(s *pcrStretch, packet int) int64 {
	samples := t.samples[s.firstSample:s.endSample]
	after := sort.Search(len(samples), func(k int) bool { return packet < samples[k].packet })
	after = min(max(after, 1), len(samples)-1)
	left, right := samples[after-1], samples[after]
	slope := float64(right.pcr-left.pcr) / float64(right.packet-left.packet)
	return left.pcr + int64(math.Round(slope*(float64(packet)-float64(left.packet))))
}

// checkPCRAgainstVideo checks that over each PCR stretch the PCR advances as the
// PTS of the video it carries: a lap is sized by the PCR and lasts by the PTS.
func checkPCRAgainstVideo(idx *streamIndex, t *pcrTimeline) string {
	pics := idx.pictures
	first := 0
	for p := 1; p <= len(pics); p++ {
		s := t.stretchOf(pics[first].firstPacket)
		if p < len(pics) && t.stretchOf(pics[p].firstPacket) == s {
			continue
		}
		last := p - 1
		if s != nil && last > first {
			ptsSpan := (pics[last].pts - pics[first].pts) * pcrPerTick
			pcrSpan := t.at(s, pics[last].firstPacket) - t.at(s, pics[first].firstPacket)
			if absInt64(pcrSpan-ptsSpan) > allowedClockSkew(absInt64(ptsSpan)) {
				count := s.endSample - s.firstSample
				packets := idx.pcrSamples[s.endSample-1].packet - idx.pcrSamples[s.firstSample].packet
				return fmt.Sprintf("the PCR does not keep time with the video: from picture %d to %d the video PTS "+
					"advances %.3f s and the PCR %.3f s (%d PCRs, one every %d packets)",
					first, last, pcrSeconds(ptsSpan), pcrSeconds(pcrSpan), count, packets/(count-1))
			}
		}
		first = p
	}
	return ""
}

// loopOptions are the choices that bound the loop.
type loopOptions struct {
	frameRate   FrameRate // zero: detected from the DTS
	maxDuration int64     // 90 kHz; 0: no limit
}

// audioFit is the audio of one stream that a lap plays: the frames from the
// one nearest the loop's first picture on, wherever the source sends them, so
// the audio seam is where the video's is.
type audioFit struct {
	stream     int // into streamIndex.audio
	firstPES   int
	pesCount   int
	firstFrame int
	// frameCount is enough to end a lap anywhere within half a frame of the
	// loop's end, unless the file has less.
	frameCount int
	// excess is their duration minus the loop's, in the stream's time unit.
	// When negative, every seam has that gap.
	excess int64
	// seamOffset is the first frame's start minus the first picture's PTS, 90 kHz.
	seamOffset int64
	// seamOK: the first frame is within its own duration of the first picture.
	seamOK bool
}

// loopPlan is a loop: the packets [startPacket, endPacket) from one video
// access point to a later one, played over and over.
type loopPlan struct {
	accessPoint  accessPoint
	startPicture int
	endPicture   int
	startPacket  int
	endPacket    int
	duration     int64 // 90 kHz, from the two access points' PTS
	frames       int   // coded pictures in a lap
	// fieldPeriods is how long a lap's pictures are shown, in field periods: two
	// a frame.
	fieldPeriods int64
	gops         int
	frameRate    FrameRate
	exact        bool // the duration is a whole number of frame periods
	constantRate bool
	bitrate      float64
	deviation    int64 // 27 MHz
	// windowTime is how long the window's packets take at their source timing, 27 MHz.
	windowTime int64
	// lapPackets is, for constant-rate input, the loop duration at the source
	// rate in packets.
	lapPackets int
	// outputPackets per lap: lapPackets, reached by adding or removing null
	// packets at the end of the window, unless the window has too few.
	outputPackets int
	audio         []audioFit
	scte35        []*LoopSCTE35
	warnings      []string
}

type loopCandidate struct {
	start, end int // into the access point list
	// numberingOK: a recovery-point loop starts each lap with the picture
	// numbers the stream continues with; always true for IDR and IRAP loops.
	numberingOK bool
	// dtsOK: the DTS steps across the seam within half a picture of its usual step.
	dtsOK bool
	// fieldsOK: the loop's first picture shows its fields as the picture after
	// its end does, so the fields alternate across the seam and a pulldown
	// cadence goes on.
	fieldsOK     bool
	exact        bool
	audioOK      bool
	seamOK       bool
	duration     int64
	dtsStepError int64 // 90 kHz
}

func (c *loopCandidate) fits() bool {
	return c.numberingOK && c.dtsOK && c.fieldsOK && c.exact && c.audioOK && c.seamOK
}

// better ranks c above o. Ties go to the earliest start, which has the lower index.
func (c *loopCandidate) better(o *loopCandidate) bool {
	for _, f := range [][2]bool{{c.numberingOK, o.numberingOK}, {c.dtsOK, o.dtsOK}, {c.fieldsOK, o.fieldsOK},
		{c.exact, o.exact}, {c.audioOK, o.audioOK}, {c.seamOK, o.seamOK}} {
		if f[0] != f[1] {
			return f[0]
		}
	}
	if c.duration != o.duration {
		return c.duration > o.duration
	}
	return c.start < o.start
}

// rangeMax gives the largest value of any range of a list in constant time,
// from a sparse table.
type rangeMax struct{ levels [][]int64 }

func newRangeMax(values []int64) *rangeMax {
	r := &rangeMax{levels: [][]int64{values}}
	for width := 2; width <= len(values); width *= 2 {
		below := r.levels[len(r.levels)-1]
		level := make([]int64, len(below)-width/2)
		for i := range level {
			level[i] = max(below[i], below[i+width/2])
		}
		r.levels = append(r.levels, level)
	}
	return r
}

// max returns the largest of the values [first, end), which must not be empty.
func (r *rangeMax) max(first, end int) int64 {
	level := bits.Len(uint(end-first)) - 1
	values := r.levels[level]
	return max(values[first], values[end-(1<<level)])
}

// fitAudio finds the audio of stream slot that a lap starting at startPTS plays.
func fitAudio(idx *streamIndex, slot int, startPTS int64, startPacket, endPacket int, duration int64) audioFit {
	st := &idx.audio[slot]
	scale := st.timeScale
	fit := audioFit{stream: slot, excess: -duration * scale}
	frames := st.frames
	if len(frames) == 0 || len(st.pes) == 0 {
		return fit
	}
	// Audio and video timestamps are unwrapped separately, so they may be whole
	// wraps apart.
	wraps := int64(math.Round(float64(st.pes[0].pts-startPTS) / float64(PtsWrap)))
	loopStart := (startPTS + wraps*PtsWrap) * scale
	first := sort.Search(len(frames), func(k int) bool { return frames[k].start >= loopStart })
	if first == len(frames) || (first > 0 && loopStart-frames[first-1].start <= frames[first].start-loopStart) {
		first--
	}

	// A lap's audio is sent in the lap before, during or after it, so its PES
	// must start within one window of the window.
	window := endPacket - startPacket
	pesOf := func(frame int) int {
		return sort.Search(len(st.pes), func(k int) bool { return st.pes[k].firstFrame > frame }) - 1
	}
	firstPES := pesOf(first)
	if st.pes[firstPES].firstPacket+window < startPacket {
		return fit
	}
	unreachable := sort.Search(len(st.pes), func(k int) bool { return st.pes[k].firstPacket >= endPacket+window })
	reachable := len(frames)
	if unreachable < len(st.pes) {
		reachable = st.pes[unreachable].firstFrame
	}

	// Enough frames for a lap to end within half a frame of the loop's end.
	wantedEnd := frames[first].start + duration*scale + frames[first].duration
	end := first + sort.Search(len(frames)-first, func(k int) bool { return frames[first+k].start >= wantedEnd })
	end = min(end, reachable)
	if end <= first {
		return fit
	}
	last := frames[end-1]
	fit.firstFrame = first
	fit.frameCount = end - first
	fit.firstPES = firstPES
	fit.pesCount = pesOf(fit.firstFrame+fit.frameCount-1) - firstPES + 1
	fit.excess = last.start + last.duration - frames[first].start - duration*scale
	fit.seamOffset = (frames[first].start - loopStart) / scale
	fit.seamOK = absInt64(frames[first].start-loopStart) <= frames[first].duration
	return fit
}

// numberingFields names the fields of n that differ from o's.
func numberingFields(n, o pictureNumbering) string {
	var fields []string
	if n.frameNum != o.frameNum {
		fields = append(fields, fmt.Sprintf("frame_num %d", n.frameNum))
	}
	if n.orderLSB != o.orderLSB {
		fields = append(fields, fmt.Sprintf("picture order count LSB %d", n.orderLSB))
	}
	if n.orderDelta != o.orderDelta {
		fields = append(fields, fmt.Sprintf("delta_pic_order_cnt[0] %d", n.orderDelta))
	}
	if n.bottomField != o.bottomField {
		if n.bottomField {
			fields = append(fields, "a bottom field")
		} else {
			fields = append(fields, "a top field")
		}
	}
	text := ""
	for i, f := range fields {
		switch {
		case i == 0:
		case i+1 == len(fields):
			text += " and "
		default:
			text += ", "
		}
		text += f
	}
	return text
}

// chooseLoop picks the loop. Among all pairs of access points of the cleanest
// class with two, never a second field, it prefers, in order: a recovery-point
// loop that keeps the picture numbering, a DTS that steps across the seam as
// elsewhere, fields that alternate across the seam in the stream's cadence, a
// whole number of frame periods, audio from the loop start, a seam that needs
// no change of output rate, and then the longest loop.
func chooseLoop(idx *streamIndex, opts loopOptions) (*loopPlan, error) {
	timeline := newPCRTimeline(idx)
	if len(idx.pcrSamples) < 2 {
		return nil, fmt.Errorf("the program carries fewer than two PCRs")
	}
	var problems []string
	for _, p := range []string{checkPictureRate(idx.pictures), checkPCRAgainstVideo(idx, timeline)} {
		if p != "" {
			problems = append(problems, p)
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(problems, "; "))
	}

	// A loop needs two access points of the class it starts on; prefer the
	// cleanest. A second field is no loop point: the lap would start or end
	// inside a frame.
	pics := idx.pictures
	var points []int
	ap := apClosedGOP
	for _, level := range []accessPoint{apClosedGOP, apOpenGOP, apRecoveryPoint} {
		points = points[:0]
		for i := range pics {
			if pics[i].info.accessPoint >= level && !pics[i].info.secondField {
				points = append(points, i)
			}
		}
		ap = level
		if len(points) >= 2 {
			break
		}
	}
	if len(points) < 2 {
		return nil, fmt.Errorf("fewer than two video access points (IDR, CRA, MPEG-2 I picture with sequence " +
			"header or recovery point)")
	}

	frameRate := opts.frameRate
	if !frameRate.known() {
		frameRate, _ = detectFrameRate(pics)
	}
	disc := idx.discontinuities
	picturePeriod, _ := medianDTSStep(pics)
	// fieldPeriodsBefore[i] is how long pictures [0, i) are shown, in field periods.
	fieldPeriodsBefore := make([]int64, len(pics)+1)
	for i := range pics {
		fieldPeriodsBefore[i+1] = fieldPeriodsBefore[i] + pics[i].info.fieldPeriods()
	}

	// Numbering decides between recovery-point loops only when two recovery
	// points share one; otherwise no loop keeps it, and it must not hold the
	// search open.
	numberingMatters := false
	if ap == apRecoveryPoint {
		seen := make(map[pictureNumbering]bool)
		for _, p := range points {
			if n := pics[p].info.numbering; n != nil {
				if seen[*n] {
					numberingMatters = true
					break
				}
				seen[*n] = true
			}
		}
	}

	// The latest PTS of each GOP, from one access point to the next: a window's
	// pictures must all be shown before its end, or one would show in the next lap.
	latest := make([]int64, len(points)-1)
	for g := range latest {
		latest[g] = math.MinInt64
		for p := points[g]; p < points[g+1]; p++ {
			latest[g] = max(latest[g], pics[p].pts)
		}
	}
	latestOfGOPs := newRangeMax(latest)

	// The longest window rejected because the PCR sends it much faster or slower
	// than its video lasts (27 MHz), and the longest rejected because its DTS
	// would jump at every seam.
	var offClock *struct{ lapTime, windowTime int64 }
	var offDTS *struct{ duration, startDelay, endDelay int64 }
	shownAfterEnd := false

	evaluate := func(first, second int) *loopCandidate {
		start, end := &pics[points[first]], &pics[points[second]]
		c := &loopCandidate{start: first, end: second, duration: end.pts - start.pts}
		startPacket, endPacket := start.firstPacket, end.firstPacket
		stretch := timeline.stretchOf(startPacket)
		if stretch == nil || timeline.stretchOf(endPacket-1) != stretch {
			return nil
		}
		if k := sort.SearchInts(disc, startPacket+1); k < len(disc) && disc[k] < endPacket {
			return nil
		}
		// A lap is sized by the PCR: one whose window is sent far off the time it
		// lasts would be any size. A short window may not even part by half its
		// duration, which a stretch too short to check could hide.
		lapTime := c.duration * pcrPerTick
		windowTime := timeline.at(stretch, endPacket) - timeline.at(stretch, startPacket)
		if absInt64(windowTime-lapTime) > min(allowedClockSkew(lapTime), lapTime/2) {
			if offClock == nil || lapTime > offClock.lapTime {
				offClock = &struct{ lapTime, windowTime int64 }{lapTime, windowTime}
			}
			return nil
		}
		if !(stretch.ticksPerPacket > 0) {
			return nil
		}
		if latestOfGOPs.max(first, second) >= end.pts {
			shownAfterEnd = true
			return nil
		}
		// The next lap's first DTS follows the window's last as the end picture's
		// would, unless PTS - DTS differs between the two.
		c.dtsStepError = (end.pts - end.dts) - (start.pts - start.dts)
		if absInt64(c.dtsStepError) > maxSeamDTSJump {
			if offDTS == nil || c.duration > offDTS.duration {
				offDTS = &struct{ duration, startDelay, endDelay int64 }{c.duration, start.pts - start.dts, end.pts - end.dts}
			}
			return nil
		}
		c.dtsOK = absInt64(c.dtsStepError)*2 <= picturePeriod
		c.numberingOK = !numberingMatters ||
			(start.info.numbering != nil && end.info.numbering != nil && *start.info.numbering == *end.info.numbering)
		c.fieldsOK = start.info.cadence() == end.info.cadence()
		// Whole frame periods by how long the pictures are shown, or by the coded
		// pictures for a rate given as the field rate of field pictures or the
		// coded rate of pulldown.
		fields := fieldPeriodsBefore[points[second]] - fieldPeriodsBefore[points[first]]
		frames := int64(points[second] - points[first])
		c.exact = !frameRate.known() || c.duration*frameRate.Num*2 == fields*ticksPerSec*frameRate.Den ||
			c.duration*frameRate.Num == frames*ticksPerSec*frameRate.Den
		c.audioOK = true
		for s := range idx.audio {
			fit := fitAudio(idx, s, start.pts, startPacket, endPacket, c.duration)
			if fit.excess < 0 || !fit.seamOK {
				c.audioOK = false
				break
			}
		}
		windowPackets := int64(endPacket - startPacket)
		if stretch.deviation <= constantRateTolerance {
			lapPackets := int64(math.Round(float64(c.duration*pcrPerTick) / stretch.ticksPerPacket))
			removable := int64(idx.nullsBefore[endPacket] - idx.nullsBefore[startPacket])
			c.seamOK = lapPackets >= windowPackets-removable
		} else {
			c.seamOK = windowTime <= lapTime
		}
		return c
	}

	// Longest first: once a loop fits in every way, no shorter one can beat it,
	// for this start or a later one.
	var best *loopCandidate
	evaluated := 0
	for first := 0; first < len(points) && evaluated < maxEvaluatedPairs; first++ {
		// Past the longest loop asked for, nothing needs looking at.
		ends := len(points)
		if opts.maxDuration > 0 {
			latestEnd := pics[points[first]].pts + opts.maxDuration
			ends = first + 1 + sort.Search(len(points)-first-1, func(k int) bool {
				return pics[points[first+1+k]].pts > latestEnd
			})
		}
		for second := ends - 1; second > first && evaluated < maxEvaluatedPairs; second-- {
			duration := pics[points[second]].pts - pics[points[first]].pts
			if best != nil && best.fits() && duration <= best.duration {
				break
			}
			if (opts.maxDuration > 0 && duration > opts.maxDuration) || duration <= 0 {
				continue
			}
			evaluated++
			c := evaluate(first, second)
			if c == nil {
				continue // crosses a discontinuity or is sent off its time, which a shorter loop may not
			}
			if best == nil || c.better(best) {
				best = c
			}
		}
	}
	if best == nil && (offClock != nil || offDTS != nil || shownAfterEnd) {
		var problems []string
		if offClock != nil {
			problems = append(problems, fmt.Sprintf("the PCR does not keep time with the video: the longest loop lasts "+
				"%.3f s by the video PTS but takes %.3f s by the PCR", pcrSeconds(offClock.lapTime), pcrSeconds(offClock.windowTime)))
		}
		if offDTS != nil {
			problems = append(problems, fmt.Sprintf("the video DTS would jump %+.3f s at every seam: PTS - DTS is %.3f s at "+
				"the start of the longest loop and %.3f s at its end", float64(offDTS.endDelay-offDTS.startDelay)/ticksPerSec,
				float64(offDTS.startDelay)/ticksPerSec, float64(offDTS.endDelay)/ticksPerSec))
		}
		if shownAfterEnd {
			problems = append(problems, "a picture decoded before the end of the loop is shown after it")
		}
		return nil, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	if best == nil {
		if opts.maxDuration > 0 {
			return nil, fmt.Errorf("no loop fits the duration limit without crossing a PCR discontinuity")
		}
		return nil, fmt.Errorf("every pair of access points has a PCR discontinuity between them")
	}

	plan := &loopPlan{
		accessPoint:  ap,
		startPicture: points[best.start],
		endPicture:   points[best.end],
		startPacket:  pics[points[best.start]].firstPacket,
		endPacket:    pics[points[best.end]].firstPacket,
		duration:     best.duration,
		frames:       points[best.end] - points[best.start],
		fieldPeriods: fieldPeriodsBefore[points[best.end]] - fieldPeriodsBefore[points[best.start]],
		gops:         best.end - best.start,
		frameRate:    frameRate,
		exact:        best.exact,
	}
	if evaluated >= maxEvaluatedPairs {
		plan.warnings = append(plan.warnings, fmt.Sprintf(
			"the search for the best loop stopped after %d pairs of access points; -d narrows it", evaluated))
	}
	if !best.dtsOK {
		start := pics[plan.startPicture]
		plan.warnings = append(plan.warnings, fmt.Sprintf("the video DTS steps %+.1f ms off at every seam: PTS - DTS is "+
			"%.1f ms at the loop start and %.1f ms at its end", float64(best.dtsStepError)/90,
			float64(start.pts-start.dts)/90, float64(start.pts-start.dts+best.dtsStepError)/90))
	}
	if !best.fieldsOK {
		plan.warnings = append(plan.warnings, fmt.Sprintf("the loop starts on a picture shown %s where the stream "+
			"continues with one shown %s: the field order or pulldown cadence breaks at every seam",
			pics[plan.startPicture].info.cadence(), pics[plan.endPicture].info.cadence()))
	}
	stretch := timeline.stretchOf(plan.startPacket)
	plan.constantRate = stretch.deviation <= constantRateTolerance
	plan.bitrate = float64(PacketSize*8) * pcrPerSec / stretch.ticksPerPacket
	plan.deviation = stretch.deviation
	plan.windowTime = timeline.at(stretch, plan.endPacket) - timeline.at(stretch, plan.startPacket)
	windowPackets := plan.endPacket - plan.startPacket
	plan.outputPackets = windowPackets
	if plan.constantRate {
		lapPackets := int(math.Round(float64(plan.duration*pcrPerTick) / stretch.ticksPerPacket))
		removable := int(idx.nullsBefore[plan.endPacket] - idx.nullsBefore[plan.startPacket])
		plan.lapPackets = lapPackets
		plan.outputPackets = max(lapPackets, windowPackets-removable)
		if plan.outputPackets > lapPackets {
			plan.warnings = append(plan.warnings, fmt.Sprintf("the window has %d packets more than a lap holds at "+
				"the source rate and too few null packets to drop; the output rate is %.3f%% higher",
				plan.outputPackets-lapPackets, 100*float64(plan.outputPackets-lapPackets)/float64(lapPackets)))
		}
	} else {
		plan.warnings = append(plan.warnings, fmt.Sprintf("variable-rate input (the PCR strays up to %.0f ms from a "+
			"constant rate): packets keep their source timing, so a player must pace the output by its PCR",
			float64(stretch.deviation)/27_000))
		if !best.seamOK {
			plan.warnings = append(plan.warnings, fmt.Sprintf("the window takes %.1f ms longer to send than the loop "+
				"lasts; its timing is compressed to fit", float64(plan.windowTime-plan.duration*pcrPerTick)/27_000))
		}
	}

	for s := range idx.audio {
		fit := fitAudio(idx, s, pics[plan.startPicture].pts, plan.startPacket, plan.endPacket, plan.duration)
		plan.audio = append(plan.audio, fit)
		pid := idx.audio[s].pid
		if fit.frameCount == 0 {
			plan.warnings = append(plan.warnings, fmt.Sprintf("audio PID %d: no audio near the loop; it is left out", pid))
			continue
		}
		if fit.excess < 0 {
			plan.warnings = append(plan.warnings, fmt.Sprintf("audio PID %d: the file holds %.1f ms less audio from "+
				"the loop start than the loop lasts; every seam has that gap",
				pid, float64(-fit.excess)/float64(idx.audio[s].timeScale)/90))
		}
		if !fit.seamOK {
			plan.warnings = append(plan.warnings, fmt.Sprintf("audio PID %d: the audio nearest the loop start is "+
				"%+.1f ms from it, so the audio seam is that far from the video's", pid, float64(fit.seamOffset)/90))
		}
	}

	// Each lap keeps the counter pattern of every PID but audio, whose packets
	// it counts afresh, so the source's errors there come back every lap.
	audioPID := make(map[int]bool)
	for _, a := range idx.audio {
		audioPID[a.pid] = true
	}
	repeated := 0
	for _, e := range idx.continuityErrors {
		if e.packet >= plan.startPacket && e.packet < plan.endPacket && !audioPID[e.pid] {
			repeated++
		}
	}
	if repeated > 0 {
		plan.warnings = append(plan.warnings, fmt.Sprintf(
			"%d of the source's continuity errors are inside the loop and come back every lap", repeated))
	}

	switch {
	case !frameRate.known():
		plan.warnings = append(plan.warnings,
			"the frame rate is not a known rate; give it with -fps to get a whole number of frames per lap")
	case !plan.exact:
		plan.warnings = append(plan.warnings, fmt.Sprintf("no loop is a whole number of %s fps frame periods; "+
			"the video timestamps step unevenly at every seam", frameRate))
	}
	if !pics[plan.startPicture].info.parameterSets {
		plan.warnings = append(plan.warnings, "the first picture of the loop carries no sequence parameters")
	}
	switch ap {
	case apOpenGOP:
		if idx.videoCodec == CODEC_HEVC {
			plan.warnings = append(plan.warnings,
				"open-GOP loop point: the CRA becomes BLA, so decoders drop its leading pictures at every seam")
		} else {
			plan.warnings = append(plan.warnings, "open-GOP loop point: the GOP is marked broken_link; its leading "+
				"B pictures still predict from the previous lap")
		}
	case apRecoveryPoint:
		first, next := pics[plan.startPicture].info.numbering, pics[plan.endPicture].info.numbering
		switch {
		case first == nil || next == nil:
			plan.warnings = append(plan.warnings, "the stream has no IDR, and the picture numbering at its recovery "+
				"points cannot be read; decoders may drop pictures at every seam")
		case *first != *next:
			plan.warnings = append(plan.warnings, fmt.Sprintf("the stream has no IDR, and no loop between recovery "+
				"points keeps the picture numbering: each lap starts with %s where the stream continues with %s, so "+
				"decoders drop or reorder pictures at every seam", numberingFields(*first, *next), numberingFields(*next, *first)))
		default:
			plan.warnings = append(plan.warnings, "the stream has no IDR; laps start at a recovery point, so until the "+
				"stream recovers, the first pictures of each lap predict from the end of the lap before")
		}
	}
	return plan, nil
}
