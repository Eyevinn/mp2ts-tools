package internal

// stuffingPID is the null/stuffing PID, excluded from continuity-counter tracking.
const stuffingPID = 8191

// ContinuityCounters provides bookkeeping of continuity counters and packet
// losses so that, when a stream is re-emitted (e.g. looped), the per-PID CC
// sequence stays gapless across the seam while faithfully reproducing any
// packet losses that were present in the input.
type ContinuityCounters struct {
	ccs map[int]*ccPidData
}

// NewContinuityCounters returns an initialized ContinuityCounters.
func NewContinuityCounters() *ContinuityCounters {
	return &ContinuityCounters{ccs: make(map[int]*ccPidData)}
}

// RegisterPacketLoss records a packet loss at pktNr so the same gap can be
// reproduced later. The stuffing PID 8191 is ignored.
func (c *ContinuityCounters) RegisterPacketLoss(pid, pktNr, loss int) {
	if pid == stuffingPID {
		return
	}
	if _, ok := c.ccs[pid]; !ok {
		c.ccs[pid] = newCCPidData(-1)
	}
	c.ccs[pid].addPacketLoss(pktNr, loss)
}

// CalcStep computes the CC step for a packet during the analysis pass and
// registers any loss (step != 1). It returns the step.
func (c *ContinuityCounters) CalcStep(pid, pktNr, cc int) int {
	if _, ok := c.ccs[pid]; !ok {
		c.ccs[pid] = newCCPidData(cc)
		return 1
	}
	ctr := c.ccs[pid]
	step := (cc - ctr.lastCC + 16) % 16
	if step != 1 {
		c.RegisterPacketLoss(pid, pktNr, step-1)
	}
	ctr.lastCC = cc
	return step
}

// ResetCC resets the continuity counters to restart at 0. Packet-loss info is
// kept.
func (c *ContinuityCounters) ResetCC() {
	for _, ctr := range c.ccs {
		ctr.lastCC = -1
	}
}

// NewCC returns the next CC value to stamp for pid at output time, replaying any
// recorded packet loss at pktNr. Synthetic packets can pass pktNr = -1 to always
// advance by one.
func (c *ContinuityCounters) NewCC(pid, pktNr int) int {
	if _, ok := c.ccs[pid]; !ok {
		c.ccs[pid] = newCCPidData(-1)
	}
	cc := c.ccs[pid]
	step := cc.step(pktNr)
	cc.lastCC = (cc.lastCC + 16 + step) % 16
	return cc.lastCC
}

type ccPidData struct {
	lastCC            int
	knownPacketLosses map[int]int
}

func newCCPidData(lastCC int) *ccPidData {
	return &ccPidData{
		lastCC:            lastCC,
		knownPacketLosses: make(map[int]int),
	}
}

func (c *ccPidData) addPacketLoss(pktNr, loss int) {
	c.knownPacketLosses[pktNr] = loss
}

func (c *ccPidData) step(pktNr int) int {
	if len(c.knownPacketLosses) == 0 {
		return 1
	}
	if loss, ok := c.knownPacketLosses[pktNr]; ok {
		return 1 + loss
	}
	return 1
}
