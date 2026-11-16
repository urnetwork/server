// Invocation-owned heartbeat diagnostics distinguish store replies from later
// owner refusal. They never classify an error as retryable or grant authority.
package controller

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Closed source return boundaries, not categories inferred from error text.
type competitionHeartbeatOwnerBoundary uint8

const (
	competitionHeartbeatOwnerUnknown competitionHeartbeatOwnerBoundary = iota
	competitionHeartbeatOwnerParentBeforeDispatch
	competitionHeartbeatOwnerAuthorityBeforeDispatch
	competitionHeartbeatOwnerTotalBeforeDispatch
	competitionHeartbeatOwnerLockBeforeDispatch
	competitionHeartbeatOwnerAttemptBeforeDispatch
	competitionHeartbeatOwnerStoreError
	competitionHeartbeatOwnerParentAfterStore
	competitionHeartbeatOwnerAuthorityAfterStore
	competitionHeartbeatOwnerAttemptAfterSuccess
	competitionHeartbeatOwnerTotalAfterStore
	competitionHeartbeatOwnerLockAfterStore
	competitionHeartbeatOwnerWaitError
	competitionHeartbeatOwnerParentDuringWait
	competitionHeartbeatOwnerSuccess
)

// A nil store return is an observed reply, never by itself a COMMIT proof.
type competitionHeartbeatStoreOutcome uint8

const (
	competitionHeartbeatStoreNotCalled competitionHeartbeatStoreOutcome = iota
	competitionHeartbeatStorePending
	competitionHeartbeatStoreSucceeded
	competitionHeartbeatStoreFailed
)

// One bounded last reply is retained; earlier attempts remain visible by count.
// AttemptAtEntry/Return describe the last dispatched store, not a later refused
// dispatch. Multiple database captures retain only the last, not a call graph.
// Absolute confirmed bounds are data only, never a new lease or deadline.
type competitionHeartbeatOwnerSnapshot struct {
	Schema                    int                                   `json:"schema"`
	Boundary                  competitionHeartbeatOwnerBoundary     `json:"boundary"`
	StoreCalls                uint64                                `json:"store_calls"`
	LastStoreOutcome          competitionHeartbeatStoreOutcome      `json:"last_store_outcome"`
	ParentAtEntry             server.DbContextBudget                `json:"parent_at_entry"`
	ParentAtReturn            server.DbContextBudget                `json:"parent_at_return"`
	TotalAtReturn             server.DbContextBudget                `json:"total_at_return"`
	AttemptAtEntry            server.DbContextBudget                `json:"attempt_at_entry"`
	AttemptAtReturn           server.DbContextBudget                `json:"attempt_at_return"`
	HasConfirmedJobExpiry     bool                                  `json:"has_confirmed_job_expiry"`
	ConfirmedJobExpiryUnixNs  int64                                 `json:"confirmed_job_expiry_unix_ns"`
	HasConfirmedHostExpiry    bool                                  `json:"has_confirmed_host_expiry"`
	ConfirmedHostExpiryUnixNs int64                                 `json:"confirmed_host_expiry_unix_ns"`
	CompletedDatabaseCaptures uint64                                `json:"completed_database_captures"`
	CompletedDatabaseOmitted  bool                                  `json:"completed_database_omitted,omitempty"`
	CompletedStoreDatabase    *competitionHeartbeatDatabaseSnapshot `json:"completed_store_database,omitempty"`
}

// The adapter and engine share one sequential invocation owner, never a global.
type competitionHeartbeatOwnerObservation struct {
	snapshot competitionHeartbeatOwnerSnapshot
}

// This captures existing input bounds without consulting the policy clock.
func newCompetitionHeartbeatOwnerObservation(ctx context.Context, jobExpiry, hostExpiry time.Time) *competitionHeartbeatOwnerObservation {
	self := &competitionHeartbeatOwnerObservation{snapshot: competitionHeartbeatOwnerSnapshot{Schema: 1, ParentAtEntry: server.ObserveDbContextBudget(ctx)}}
	if !jobExpiry.IsZero() {
		self.snapshot.HasConfirmedJobExpiry, self.snapshot.ConfirmedJobExpiryUnixNs = true, jobExpiry.UnixNano()
	}
	if !hostExpiry.IsZero() {
		self.snapshot.HasConfirmedHostExpiry, self.snapshot.ConfirmedHostExpiryUnixNs = true, hostExpiry.UnixNano()
	}
	return self
}

// Mark only the branch actually selected, before its deferred cancellation.
func (self *competitionHeartbeatOwnerObservation) returning(boundary competitionHeartbeatOwnerBoundary, parent, total context.Context) {
	if self == nil {
		return
	}
	self.snapshot.Boundary, self.snapshot.ParentAtReturn = boundary, server.ObserveDbContextBudget(parent)
	if total != nil {
		self.snapshot.TotalAtReturn = server.ObserveDbContextBudget(total)
	}
}

// Reset the last database reply on every store call; success cannot borrow one.
func (self *competitionHeartbeatOwnerObservation) beginStore(ctx context.Context) {
	if self != nil {
		self.snapshot.StoreCalls++
		self.snapshot.LastStoreOutcome = competitionHeartbeatStorePending
		self.snapshot.AttemptAtEntry = server.ObserveDbContextBudget(ctx)
		self.snapshot.CompletedDatabaseCaptures, self.snapshot.CompletedDatabaseOmitted, self.snapshot.CompletedStoreDatabase = 0, false, nil
	}
}

// Observe only after the complete public store and all its cleanup returned.
func (self *competitionHeartbeatOwnerObservation) finishStore(ctx context.Context, succeeded bool, database *competitionHeartbeatCompletedDatabase) {
	if self == nil {
		return
	}
	self.snapshot.AttemptAtReturn = server.ObserveDbContextBudget(ctx)
	self.snapshot.LastStoreOutcome = competitionHeartbeatStoreFailed
	if succeeded {
		self.snapshot.LastStoreOutcome = competitionHeartbeatStoreSucceeded
	}
	self.snapshot.CompletedDatabaseCaptures, self.snapshot.CompletedStoreDatabase, self.snapshot.CompletedDatabaseOmitted = database.closeAndSnapshot()
}

// Returned facts and their nested pointers cannot mutate the retained owner.
func (self *competitionHeartbeatOwnerObservation) snapshotCopy() competitionHeartbeatOwnerSnapshot {
	if self == nil {
		return competitionHeartbeatOwnerSnapshot{}
	}
	value := self.snapshot
	value.CompletedStoreDatabase, _ = cloneCompetitionHeartbeatOwnerDatabase(value.CompletedStoreDatabase)
	return value
}

// A private context key cannot be selected by SQL, identifiers or persisted data.
type competitionHeartbeatCompletedDatabaseKey struct{}

// A per-store sink closes at return. Its tiny lock excludes late/asynchronous
// publication without retaining a context, error, SQL, argument or callback.
type competitionHeartbeatCompletedDatabase struct {
	stateLock sync.Mutex
	closed    bool
	count     uint64
	omitted   bool
	snapshot  *competitionHeartbeatDatabaseSnapshot
}

// The key is optional, so ordinary store callers retain their old capture path.
func competitionHeartbeatCompletedDatabaseFromContext(ctx context.Context) *competitionHeartbeatCompletedDatabase {
	value, _ := ctx.Value(competitionHeartbeatCompletedDatabaseKey{}).(*competitionHeartbeatCompletedDatabase)
	return value
}

// Completed facts are copied immediately; unknown strings/oversize are omitted.
func (self *competitionHeartbeatCompletedDatabase) capture(snapshot competitionHeartbeatDatabaseSnapshot) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.closed {
		self.count++
		self.snapshot, self.omitted = cloneCompetitionHeartbeatOwnerDatabase(&snapshot)
	}
}

// Closing returns detached facts and forbids any subsequent publication.
func (self *competitionHeartbeatCompletedDatabase) closeAndSnapshot() (uint64, *competitionHeartbeatDatabaseSnapshot, bool) {
	if self == nil {
		return 0, nil, false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
	snapshot, _ := cloneCompetitionHeartbeatOwnerDatabase(self.snapshot)
	return self.count, snapshot, self.omitted
}

// The bound follows the source's fixed step/ordinal array, not a new tunable.
func cloneCompetitionHeartbeatOwnerDatabase(original *competitionHeartbeatDatabaseSnapshot) (*competitionHeartbeatDatabaseSnapshot, bool) {
	if original == nil {
		return nil, false
	}
	if len(original.Statements) > int(competitionHeartbeatDatabaseStepCount)*3 ||
		original.SqlShape != "" && original.SqlShape != competitionHeartbeatLegacySlotShape && original.SqlShape != competitionHeartbeatScopeLockShape && original.SqlShape != competitionHeartbeatDistributedSlotShape {
		return nil, true
	}
	validBudget := func(value server.DbContextBudget) bool {
		return value.State == "" || value.State == "live" || value.State == "deadline" || value.State == "canceled" || value.State == "other"
	}
	for _, budget := range []server.DbContextBudget{original.AtEntry, original.AtReturn, original.Acquire.BeforeOpen, original.Acquire.BeforeAcquire, original.Acquire.AtReturn,
		original.Transaction.LastBegin.Before, original.Transaction.LastBegin.AtReturn, original.Transaction.LastCommit.Before, original.Transaction.LastCommit.AtReturn,
		original.Transaction.LastRollback.Before, original.Transaction.LastRollback.AtReturn} {
		if !validBudget(budget) {
			return nil, true
		}
	}
	for _, statement := range original.Statements {
		if statement.Kind != "exec" && statement.Kind != "row_scan" || !validBudget(statement.Entered) || statement.AtReturn != nil && !validBudget(*statement.AtReturn) {
			return nil, true
		}
	}
	value := *original
	if original.Acquire.PoolReturn != nil {
		pool := *original.Acquire.PoolReturn
		value.Acquire.PoolReturn = &pool
	}
	value.Statements = append([]competitionHeartbeatStatementObservation(nil), original.Statements...)
	for index := range value.Statements {
		if original.Statements[index].AtReturn != nil {
			budget := *original.Statements[index].AtReturn
			value.Statements[index].AtReturn = &budget
		}
	}
	return &value, false
}

// Only the final worker handback adds this wrapper, after retry decisions.
type competitionHeartbeatOwnerError struct {
	cause    error
	snapshot competitionHeartbeatOwnerSnapshot
}

// Error bytes and their existing digest remain exactly those of the cause.
func (self competitionHeartbeatOwnerError) Error() string { return self.cause.Error() }

// Cancellation, PostgreSQL and fencing identity retain their original leaves.
func (self competitionHeartbeatOwnerError) Unwrap() error { return self.cause }

// No nil error becomes a failure, and no observer can supply retry authority.
func withCompetitionHeartbeatOwner(err error, observation *competitionHeartbeatOwnerObservation) error {
	if err == nil || observation == nil {
		return err
	}
	return competitionHeartbeatOwnerError{cause: err, snapshot: observation.snapshotCopy()}
}
