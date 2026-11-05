// Optional transaction diagnostics retain cumulative boundary counts without
// changing admission, replay, context ownership or the original outcome.
package server

import "context"

// One sequential Db/Tx invocation owns this observation across all inner
// retries. Read Snapshot only after that invocation has completely unwound.
// It contains no callback, query, error, identifier or retained context.
type DbTxObservation struct {
	snapshot DbTxObservationSnapshot
}

// Counts describe actual boundary entry/return, not a transaction decision.
// In particular, ErrTxClosed is not a rollback acknowledgment, and a commit
// error or panic remains unacknowledged even if the database may have committed.
// These are Db/Tx-owned boundaries, not manual calls made by a business callback.
type DbTxObservationSnapshot struct {
	AcquireAttempts      uint64                  `json:"acquire_attempts"`
	AcquireSucceeded     uint64                  `json:"acquire_succeeded"`
	BeginAttempts        uint64                  `json:"begin_attempts"`
	BeginSucceeded       uint64                  `json:"begin_succeeded"`
	CallbackEntered      uint64                  `json:"callback_entered"`
	CommitAttempts       uint64                  `json:"commit_attempts"`
	CommitAcknowledged   uint64                  `json:"commit_acknowledged"`
	RollbackAttempts     uint64                  `json:"rollback_attempts"`
	RollbackAcknowledged uint64                  `json:"rollback_acknowledged"`
	ReleaseAttempts      uint64                  `json:"release_attempts"`
	ReleaseReturned      uint64                  `json:"release_returned"`
	LastBegin            DbTxBoundaryObservation `json:"last_begin"`
	LastCommit           DbTxBoundaryObservation `json:"last_commit"`
	LastRollback         DbTxBoundaryObservation `json:"last_rollback"`
	LastRelease          DbTxReleaseObservation  `json:"last_release"`
}

// These dispositions describe the existing pool owner, not a cleanup join.
// Unknown includes hooks whose release/destructor lifetime is not established.
type DbTxReleaseDisposition uint8

const (
	DbTxReleaseUnknown DbTxReleaseDisposition = iota
	DbTxReleaseIdle
	DbTxReleaseDestroyTransferred
)

// A disposal transfer may still have physical resources pending. Only the
// actual held cleanup channel supplies CleanupComplete; it grants no replay.
type DbTxReleaseObservation struct {
	Disposition     DbTxReleaseDisposition `json:"disposition"`
	CleanupComplete bool                   `json:"cleanup_complete"`
}

// Only the last entered boundary retains finite context budgets. BEGIN samples
// its invocation caller; commit/rollback sample their existing detached bounded
// operation contexts. Returned excludes a boundary that panicked before return.
type DbTxBoundaryObservation struct {
	Before   DbContextBudget `json:"before"`
	AtReturn DbContextBudget `json:"at_return"`
	Returned bool            `json:"returned"`
}

// A zero value is also ready; no state is shared with another invocation.
func NewDbTxObservation() *DbTxObservation { return &DbTxObservation{} }

// A value copy cannot change when the original owner performs later work.
// Nil observations retain the same empty shape without sampling any context.
func (self *DbTxObservation) Snapshot() DbTxObservationSnapshot {
	if self == nil {
		return DbTxObservationSnapshot{}
	}
	return self.snapshot
}

// Count entry only after pool opening, immediately before the actual Acquire.
func (self *DbTxObservation) acquireEntered() {
	if self != nil {
		self.snapshot.AcquireAttempts++
	}
}

// Failed acquisition cannot fabricate an admitted transaction.
func (self *DbTxObservation) acquireReturned(succeeded bool) {
	if self != nil && succeeded {
		self.snapshot.AcquireSucceeded++
	}
}

// Reset only the last budget, never any cumulative invocation count.
func (self *DbTxObservation) beginEntered(ctx context.Context) {
	if self != nil {
		self.snapshot.BeginAttempts++
		self.snapshot.LastBegin = DbTxBoundaryObservation{Before: ObserveDbContextBudget(ctx)}
	}
}

// A successful BEGIN can still be followed by cancellation before callback.
func (self *DbTxObservation) beginReturned(ctx context.Context, succeeded bool) {
	if self != nil {
		self.snapshot.LastBegin.AtReturn, self.snapshot.LastBegin.Returned = ObserveDbContextBudget(ctx), true
		if succeeded {
			self.snapshot.BeginSucceeded++
		}
	}
}

// This is the actual business Tx callback, never dbWithPool's outer closure.
func (self *DbTxObservation) callbackEntered() {
	if self != nil {
		self.snapshot.CallbackEntered++
	}
}

// Sample the existing detached commit context, without creating another bound.
func (self *DbTxObservation) commitEntered(ctx context.Context) {
	if self != nil {
		self.snapshot.CommitAttempts++
		self.snapshot.LastCommit = DbTxBoundaryObservation{Before: ObserveDbContextBudget(ctx)}
	}
}

// Only a nil commit return acknowledges authority before later bookkeeping.
func (self *DbTxObservation) commitReturned(ctx context.Context, acknowledged bool) {
	if self != nil {
		self.snapshot.LastCommit.AtReturn, self.snapshot.LastCommit.Returned = ObserveDbContextBudget(ctx), true
		if acknowledged {
			self.snapshot.CommitAcknowledged++
		}
	}
}

// Cleanup keeps the previous phase/error and uses its existing detached owner.
func (self *DbTxObservation) rollbackEntered(ctx context.Context) {
	if self != nil {
		self.snapshot.RollbackAttempts++
		self.snapshot.LastRollback = DbTxBoundaryObservation{Before: ObserveDbContextBudget(ctx)}
	}
}

// A closed transaction is not a successful rollback acknowledgment.
func (self *DbTxObservation) rollbackReturned(ctx context.Context, acknowledged bool) {
	if self != nil {
		self.snapshot.LastRollback.AtReturn, self.snapshot.LastRollback.Returned = ObserveDbContextBudget(ctx), true
		if acknowledged {
			self.snapshot.RollbackAcknowledged++
		}
	}
}

// Entry precedes the existing non-Hijack release, without changing its owner.
func (self *DbTxObservation) releaseEntered() {
	if self != nil {
		self.snapshot.ReleaseAttempts++
		self.snapshot.LastRelease = DbTxReleaseObservation{}
	}
}

// Only a returned release can transfer custody. Sampling a channel does not
// wait for it, retain it, or start another cleanup worker.
func (self *DbTxObservation) releaseReturned(disposition DbTxReleaseDisposition, cleanup <-chan struct{}) {
	if self != nil {
		self.snapshot.ReleaseReturned++
		self.snapshot.LastRelease.Disposition = disposition
		select {
		case <-cleanup:
			self.snapshot.LastRelease.CleanupComplete = true
		default:
		}
	}
}
