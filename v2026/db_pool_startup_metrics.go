package server

// Fixed, per-pool-generation observations of the existing startup hook. These
// counters do not own cancellation, connection disposal or pool admission.

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Only the initial validation and its failed-startup cleanup are observed.
type pgPoolStartupPhase uint8

const (
	pgPoolInitialPing pgPoolStartupPhase = iota
	pgPoolFailedStartupCleanup
	pgPoolStartupPhaseCount
)

// Outcomes describe the actual phase return, independent of Acquire's caller.
type pgPoolStartupOutcome uint8

const (
	pgPoolStartupOk pgPoolStartupOutcome = iota
	pgPoolStartupDeadline
	pgPoolStartupCanceled
	pgPoolStartupOther
	pgPoolStartupOutcomeCount
)

// Completed duration excludes still-active phases; no live clock is sampled.
type pgPoolStartupPhaseSnapshot struct {
	active          int64
	completed       [pgPoolStartupOutcomeCount]uint64
	durationSeconds float64
}

// One fixed-size observer is captured by each pool's AfterConnect hook.
// No pool or wrapper lock is acquired while updating its counters.
type pgPoolStartupMetrics struct {
	stateLock      sync.Mutex
	phaseSnapshots [pgPoolStartupPhaseCount]pgPoolStartupPhaseSnapshot
}

// Physical cleanup completion is independent of the Close call's result.
type pgStartupCleanupConnection interface {
	Close(context.Context) error
	CleanupDone() chan struct{}
}

// Observes the existing detached cleanup budget and returns its join result.
func (self *pgPoolStartupMetrics) cleanupFailedStartup(ctx context.Context, conn pgStartupCleanupConnection, timeout time.Duration) error {
	started := self.begin(pgPoolFailedStartupCleanup)
	err := cleanupFailedPgStartup(ctx, conn, timeout)
	self.finish(pgPoolFailedStartupCleanup, time.Since(started), err)
	return err
}

// Starts observation before entering the existing operation.
func (self *pgPoolStartupMetrics) begin(phase pgPoolStartupPhase) time.Time {
	self.stateLock.Lock()
	self.phaseSnapshots[phase].active++
	self.stateLock.Unlock()
	return time.Now()
}

// Records one completed operation without retaining its error or identity.
func (self *pgPoolStartupMetrics) finish(phase pgPoolStartupPhase, elapsed time.Duration, err error) {
	outcome := pgPoolStartupOther
	switch {
	case err == nil:
		outcome = pgPoolStartupOk
	case errors.Is(err, context.Canceled):
		outcome = pgPoolStartupCanceled
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded), pgconn.Timeout(err):
		outcome = pgPoolStartupDeadline
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.phaseSnapshots[phase].active--
	self.phaseSnapshots[phase].completed[outcome]++
	self.phaseSnapshots[phase].durationSeconds += elapsed.Seconds()
}

// Reads only local counters; an unobserved fixture pool has zero phase counts.
func (self *pgPoolStartupMetrics) snapshot() [pgPoolStartupPhaseCount]pgPoolStartupPhaseSnapshot {
	if self == nil {
		return [pgPoolStartupPhaseCount]pgPoolStartupPhaseSnapshot{}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.phaseSnapshots
}
