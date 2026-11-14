package monitor

// Per-execution availability for an intentionally deferred class. It is
// independent of alerts: first-observation grace remains silent, while an
// existing incident cannot recover from an observation that never ran.

import (
	"context"
	"sync"
)

// Only this exact identity's absence credit is withheld. Frame is excluded
// because notifier recovery matches on probe, class and target.
type SignalClassClearHold struct {
	SignalId string
	Class    string
	Target   string
}

// An internal sink scoped to one public Run call, never stored on a signal.
// The adapter publishes only after successful completion. Snapshot returns
// independent storage before the handler runs; concurrent runs own other sinks.
type signalClearHoldObservation struct {
	stateLock  sync.Mutex
	clearHolds []SignalClassClearHold
}

// Only the execution-loop owner installs this sink around a public Run.
type signalClearHoldContextKey struct{}

// Retain fixed class/target identities, never source payload or alert text.
func recordSignalClearHolds(ctx context.Context, clearHolds []SignalClassClearHold) {
	observation, _ := ctx.Value(signalClearHoldContextKey{}).(*signalClearHoldObservation)
	if observation == nil || len(clearHolds) == 0 {
		return
	}
	observation.stateLock.Lock()
	defer observation.stateLock.Unlock()
	observation.clearHolds = append(observation.clearHolds, clearHolds...)
}

// A failed execution retains its existing failure authority and no holds.
func (self *signalClearHoldObservation) snapshot(failed bool) []SignalClassClearHold {
	if failed {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]SignalClassClearHold(nil), self.clearHolds...)
}
