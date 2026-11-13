package server

import (
	"sync/atomic"
	"time"
)

// DbReadObservation is optional, request-owned, fixed-size read telemetry.
// Writers publish immutable snapshots; scrapes never read mutable operation
// state. It holds no callbacks, identifiers, SQL, arguments, or error strings.
// Its methods are safe for concurrent observation and never wait for a consumer.
type DbReadObservation struct {
	snapshot atomic.Pointer[DbReadObservationSnapshot]
}

type DbReadPhase uint8

const (
	DbReadStarting DbReadPhase = iota
	DbReadAcquireBegin
	DbReadAcquireDone
	DbReadQueryBegin
	DbReadRows
	DbReadQueryDone
	DbReadError
	DbReadComplete
	DbReadPhaseCount
)

type DbReadObservationSnapshot struct {
	StartedAt        time.Time
	UpdatedAt        time.Time
	Phase            DbReadPhase
	PhaseCounts      [DbReadPhaseCount]uint64
	PhaseAt          [DbReadPhaseCount]time.Time
	AcquireSucceeded uint64
	QuerySucceeded   uint64
	AcquireDuration  time.Duration
	QueryDuration    time.Duration
	Rows             uint64
	Finished         bool
}

func NewDbReadObservation() *DbReadObservation {
	observation := &DbReadObservation{}
	observation.record(DbReadStarting, false)
	return observation
}

func (self *DbReadObservation) Snapshot() DbReadObservationSnapshot {
	if self != nil {
		if snapshot := self.snapshot.Load(); snapshot != nil {
			return *snapshot
		}
	}
	return DbReadObservationSnapshot{}
}

func (self *DbReadObservation) BeginAcquire()              { self.record(DbReadAcquireBegin, false) }
func (self *DbReadObservation) FinishAcquire(success bool) { self.record(DbReadAcquireDone, success) }
func (self *DbReadObservation) BeginQuery()                { self.record(DbReadQueryBegin, false) }
func (self *DbReadObservation) FinishQuery(success bool)   { self.record(DbReadQueryDone, success) }
func (self *DbReadObservation) Row()                       { self.record(DbReadRows, false) }

// Finish is terminal and idempotent. A late operation cannot revise a joined
// owner's final phase, even if a test or future caller retains the pointer.
func (self *DbReadObservation) Finish(success bool) {
	phase := DbReadError
	if success {
		phase = DbReadComplete
	}
	self.record(phase, false)
}

func (self *DbReadObservation) record(phase DbReadPhase, success bool) {
	if self == nil {
		return
	}
	for {
		prior := self.snapshot.Load()
		next := DbReadObservationSnapshot{}
		if prior != nil {
			if prior.Finished {
				return
			}
			next = *prior
		}
		at := time.Now()
		if next.StartedAt.IsZero() {
			next.StartedAt = at
		}
		next.UpdatedAt = at
		next.Phase = phase
		next.PhaseCounts[phase]++
		next.PhaseAt[phase] = at
		switch phase {
		case DbReadAcquireDone:
			if begin := next.PhaseAt[DbReadAcquireBegin]; !begin.IsZero() {
				next.AcquireDuration += at.Sub(begin)
			}
			if success {
				next.AcquireSucceeded++
			}
		case DbReadQueryDone:
			if begin := next.PhaseAt[DbReadQueryBegin]; !begin.IsZero() {
				next.QueryDuration += at.Sub(begin)
			}
			if success {
				next.QuerySucceeded++
			}
		case DbReadRows:
			next.Rows++
		case DbReadError, DbReadComplete:
			next.Finished = true
		}
		if self.snapshot.CompareAndSwap(prior, &next) {
			return
		}
	}
}
