package server

import "time"

// DbTiming is an optional, request-owned Tx/Db option. It contains only finite
// numeric phases, never query text, arguments, identifiers, or error strings.
// Repeated attempts accumulate rather than replacing a rolled-back attempt.
// One sequential owner must finish using it before another goroutine reads it.
type DbTiming struct {
	Phases [DbTimingPhaseCount]DbTimingSample
}

type DbTimingPhase uint8

const (
	DbTimingAcquire DbTimingPhase = iota
	DbTimingBegin
	DbTimingCommit
	DbTimingRollback
	DbTimingRetryWait
	DbTimingPhaseCount
)

// DbTimingSample counts completed observations, including failed operations.
// Duration is client wall time, not PostgreSQL statement execution time.
type DbTimingSample struct {
	Count    uint64
	Duration time.Duration
}

func (self *DbTiming) start() time.Time {
	if self == nil {
		return time.Time{}
	}
	return time.Now()
}

func (self *DbTiming) finish(phase DbTimingPhase, started time.Time) {
	if self != nil {
		self.Phases[phase].Count++
		self.Phases[phase].Duration += time.Since(started)
	}
}
