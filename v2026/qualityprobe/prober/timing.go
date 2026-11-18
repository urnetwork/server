package prober

import "time"

// ProbeTiming partitions one returned ProbeOne call. It carries no identity,
// result, or error text. Open may return before asynchronous route admission;
// CheckAndBuffer includes that later setup, checks, readiness, and in-memory
// reporter work. AttemptReport runs after joined cleanup. A caller's eventual
// durable publication may happen outside ProbeOne and is not measured here.
type ProbeTiming struct {
	Open, CheckAndBuffer, CloseJoin, AttemptReport, Other, Total time.Duration
}

type probeTimingStage uint8

const (
	probeTimingOther probeTimingStage = iota
	probeTimingOpen
	probeTimingCheck
	probeTimingClose
	probeTimingAttempt
)

type probeTimingRecorder struct {
	observe     func(ProbeTiming)
	start, last time.Time
	stage       probeTimingStage
	value       ProbeTiming
}

func newProbeTimingRecorder(observe func(ProbeTiming)) *probeTimingRecorder {
	if observe == nil {
		return nil
	}
	now := time.Now()
	return &probeTimingRecorder{observe: observe, start: now, last: now}
}

func (self *probeTimingRecorder) advance(now time.Time) {
	duration := now.Sub(self.last)
	switch self.stage {
	case probeTimingOpen:
		self.value.Open += duration
	case probeTimingCheck:
		self.value.CheckAndBuffer += duration
	case probeTimingClose:
		self.value.CloseJoin += duration
	case probeTimingAttempt:
		self.value.AttemptReport += duration
	default:
		self.value.Other += duration
	}
	self.last = now
}

func (self *probeTimingRecorder) enter(stage probeTimingStage) {
	if self != nil {
		self.advance(time.Now())
		self.stage = stage
	}
}

// Called on ordinary return only, after terminal Close and attempt reporting.
// A panic or a call that never returns must not become a completed timing.
func (self *probeTimingRecorder) finish() {
	if self != nil {
		now := time.Now()
		self.advance(now)
		self.value.Total = now.Sub(self.start)
		self.observe(self.value)
	}
}
