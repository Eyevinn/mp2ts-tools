package internal

type StreamStatistics struct {
	Type       string  `json:"streamType"`
	Pid        uint16  `json:"pid"`
	FrameRate  float64 `json:"frameRate"`
	TimeStamps []int64 `json:"-"`
	MaxStep    int64   `json:"maxStep,omitempty"`
	MinStep    int64   `json:"minStep,omitempty"`
	AvgStep    int64   `json:"avgStep,omitempty"`
	// RAI-markers
	RAIPTS         []int64 `json:"-"`
	IDRPTS         []int64 `json:"-"`
	RAIGOPDuration int64   `json:"RAIGoPDuration,omitempty"`
	IDRGOPDuration int64   `json:"IDRGoPDuration,omitempty"`
	// Errors
	Errors []string `json:"errors,omitempty"`
}

func (p *JsonPrinter) PrintStatistics(s StreamStatistics, show bool) {
	// fmt.Fprintf(p.w, "Print statistics for PID: %d\n", s.Pid)
	s.calculateFrameRate(TimeScale)
	s.calculateGoPDuration(TimeScale)
	// TODO: format statistics

	// print statistics
	p.Print(s, show)
}

func sliceMinMaxAverage(values []int64) (min, max, avg int64) {
	if len(values) == 0 {
		return 0, 0, 0
	}

	min = values[0]
	max = values[0]
	sum := int64(0)
	for _, number := range values {
		if number < min {
			min = number
		}
		if number > max {
			max = number
		}
		sum += number
	}
	avg = sum / int64(len(values))
	return min, max, avg
}

func CalculateSteps(timestamps []int64) []int64 {
	if len(timestamps) < 2 {
		return nil
	}

	// PTS/DTS are 33-bit values, so it wraps around after 26.5 hours
	steps := make([]int64, len(timestamps)-1)
	for i := 0; i < len(timestamps)-1; i++ {
		steps[i] = SignedPTSDiff(timestamps[i+1], timestamps[i])
	}
	return steps
}

// Calculate frame rate from DTS or PTS steps
func (s *StreamStatistics) calculateFrameRate(timescale int64) {
	if len(s.TimeStamps) < 2 {
		s.Errors = append(s.Errors, "too few timestamps to calculate frame rate")
		return
	}

	steps := CalculateSteps(s.TimeStamps)
	minStep, maxStep, avgStep := sliceMinMaxAverage(steps)
	if maxStep != minStep {
		s.Errors = append(s.Errors, "irregular PTS/DTS steps")
		s.MinStep, s.MaxStep, s.AvgStep = minStep, maxStep, avgStep
	}

	// fmt.Printf("Steps: %v\n", steps)
	// fmt.Printf("Average step: %f\n", avgStep)
	s.FrameRate = float64(timescale) / float64(avgStep)
}

func (s *StreamStatistics) calculateGoPDuration(timescale int64) {
	if len(s.RAIPTS) < 2 || len(s.IDRPTS) < 2 {
		s.Errors = append(s.Errors, "no GoP duration since less than 2 I-frames")
		return
	}

	// Calculate GOP duration
	RAIPTSSteps := CalculateSteps(s.RAIPTS)
	IDRPTSSteps := CalculateSteps(s.IDRPTS)
	_, _, RAIGOPStep := sliceMinMaxAverage(RAIPTSSteps)
	_, _, IDRGOPStep := sliceMinMaxAverage(IDRPTSSteps)
	// fmt.Printf("RAIPTSSteps: %v\n", RAIPTSSteps)
	// fmt.Printf("RAIGOPStep: %d\n", RAIGOPStep)
	s.RAIGOPDuration = RAIGOPStep / timescale
	s.IDRGOPDuration = IDRGOPStep / timescale
}
