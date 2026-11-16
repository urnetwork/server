// Invocation census tests use synthetic pgx replies and rollback fakes only.
// No database service or application configuration is loaded by these tests.
package server

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Any inspection would prove a nil observer added context sampling work.
type dbTxObservationTripwireContext struct{}

// No nil-observer path may consult a deadline.
func (self dbTxObservationTripwireContext) Deadline() (time.Time, bool) {
	panic("unexpected deadline sample")
}

// No nil-observer path may subscribe to cancellation.
func (self dbTxObservationTripwireContext) Done() <-chan struct{} {
	panic("unexpected cancellation sample")
}

// No nil-observer path may inspect cancellation.
func (self dbTxObservationTripwireContext) Err() error { panic("unexpected error sample") }

// No nil-observer path may inspect caller metadata.
func (self dbTxObservationTripwireContext) Value(any) any { panic("unexpected value sample") }

// Nil is a true no-observation mode; a returned snapshot is an immutable copy.
func TestDbTxObservationNilAndSnapshotOwnership(t *testing.T) {
	var absent *DbTxObservation
	ctx := dbTxObservationTripwireContext{}
	absent.acquireEntered()
	absent.acquireReturned(true)
	absent.beginEntered(ctx)
	absent.beginReturned(ctx, true)
	absent.callbackEntered()
	absent.commitEntered(ctx)
	absent.commitReturned(ctx, true)
	absent.rollbackEntered(ctx)
	absent.rollbackReturned(ctx, true)
	absent.releaseEntered()
	absent.releaseReturned(DbTxReleaseIdle, nil)
	if absent.Snapshot() != (DbTxObservationSnapshot{}) {
		t.Fatal("nil transaction observation invented evidence")
	}
	observation := NewDbTxObservation()
	observation.acquireEntered()
	observation.acquireReturned(true)
	before := observation.Snapshot()
	observation.acquireEntered()
	observation.callbackEntered()
	if before.AcquireAttempts != 1 || before.AcquireSucceeded != 1 || before.CallbackEntered != 0 || observation.Snapshot().AcquireAttempts != 2 {
		t.Fatal("snapshot retained mutable state or counters reset across attempts")
	}
}

// Configurable return/panic evidence leaves the real cleanup owner unchanged.
type dbTxObservationRollbackTx struct {
	PgTx
	result     error
	panicValue any
	calls      int
	budget     DbContextBudget
}

// The context observed here is the actual context passed to Rollback.
func (self *dbTxObservationRollbackTx) Rollback(ctx context.Context) error {
	self.calls++
	self.budget = ObserveDbContextBudget(ctx)
	if self.panicValue != nil {
		panic(self.panicValue)
	}
	return self.result
}

// Only nil acknowledges rollback. ErrTxClosed and another failure remain
// secondary cleanup evidence; neither replaces the owning callback outcome.
func TestDbTxObservationRollbackRequiresActualAcknowledgment(t *testing.T) {
	for _, result := range []error{nil, pgx.ErrTxClosed, errors.New("synthetic rollback refusal")} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		tx := &dbTxObservationRollbackTx{result: result}
		observation := NewDbTxObservation()
		if recovered := captureDbErrorPanic(func() { rollbackTxWithObservation(ctx, tx, observation) }); recovered != nil {
			t.Fatal("observation changed best-effort rollback outcome", recovered)
		}
		snapshot := observation.Snapshot()
		acknowledged := uint64(0)
		if result == nil {
			acknowledged = 1
		}
		if tx.calls != 1 || snapshot.RollbackAttempts != 1 || snapshot.RollbackAcknowledged != acknowledged || !snapshot.LastRollback.Returned ||
			tx.budget.State != "live" || !tx.budget.HasDeadline || tx.budget.RemainingNs <= 0 || tx.budget.RemainingNs > PgRollbackTimeout.Nanoseconds() ||
			snapshot.LastRollback.Before.State != "live" || snapshot.LastRollback.AtReturn.State != "live" {
			t.Fatal("rollback census changed its detached owner or acknowledgment", snapshot, tx.budget)
		}
	}
}

// A cleanup panic remains exact and cannot fabricate a completed boundary.
func TestDbTxObservationRollbackPanicRetainsUnreturnedBoundary(t *testing.T) {
	original := errors.New("synthetic rollback panic")
	observation := NewDbTxObservation()
	tx := &dbTxObservationRollbackTx{panicValue: original}
	if recovered := captureDbErrorPanic(func() { rollbackTxWithObservation(t.Context(), tx, observation) }); recovered != original {
		t.Fatal("observation replaced the original cleanup panic", recovered)
	}
	if snapshot := observation.Snapshot(); snapshot.RollbackAttempts != 1 || snapshot.RollbackAcknowledged != 0 || snapshot.LastRollback.Returned {
		t.Fatal("panicking rollback was reported as acknowledged", snapshot)
	}
}

// Real pgx wire sequencing authenticates cumulative counts over the unchanged
// retry paths, and independently checks that no SQL/replay decision changed.
func TestDbTxObservationRetainsAllInnerAttemptsAndWireOrder(t *testing.T) {
	for _, check := range []struct {
		name    string
		want    DbTxObservationSnapshot
		queries []string
	}{
		{name: "success", want: DbTxObservationSnapshot{AcquireAttempts: 1, AcquireSucceeded: 1, BeginAttempts: 1, BeginSucceeded: 1, CallbackEntered: 1, CommitAttempts: 1, CommitAcknowledged: 1}, queries: []string{"begin", "SELECT 1", "commit"}},
		{name: "body retry", want: DbTxObservationSnapshot{AcquireAttempts: 2, AcquireSucceeded: 2, BeginAttempts: 2, BeginSucceeded: 2, CallbackEntered: 2, CommitAttempts: 1, CommitAcknowledged: 1, RollbackAttempts: 1, RollbackAcknowledged: 1}, queries: []string{"begin", "SELECT 1", "rollback", "begin", "SELECT 1", "commit"}},
		{name: "commit retry", want: DbTxObservationSnapshot{AcquireAttempts: 2, AcquireSucceeded: 2, BeginAttempts: 2, BeginSucceeded: 2, CallbackEntered: 2, CommitAttempts: 2, CommitAcknowledged: 1}, queries: []string{"begin", "SELECT 1", "commit", "begin", "SELECT 1", "commit"}},
		{name: "body panic", want: DbTxObservationSnapshot{AcquireAttempts: 1, AcquireSucceeded: 1, BeginAttempts: 1, BeginSucceeded: 1, CallbackEntered: 1, RollbackAttempts: 1, RollbackAcknowledged: 1}, queries: []string{"begin", "SELECT 1", "rollback"}},
		{name: "lost commit", want: DbTxObservationSnapshot{AcquireAttempts: 1, AcquireSucceeded: 1, BeginAttempts: 1, BeginSucceeded: 1, CallbackEntered: 1, CommitAttempts: 1}, queries: []string{"begin", "SELECT 1", "commit"}},
	} {
		check.want.ReleaseAttempts, check.want.ReleaseReturned = check.want.AcquireSucceeded, check.want.AcquireSucceeded
		failed := false
		fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool { return check.name != "lost commit" || query != "commit" }, neverPingTestPool,
			func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
				fixture.queryError = func(_ int, query string) *pgproto3.ErrorResponse {
					if !failed && (check.name == "body retry" && query == "SELECT 1" || check.name == "commit retry" && query == "commit") {
						failed = true
						return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "40001", Message: "synthetic retry"}
					}
					return nil
				}
			})
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		observation := NewDbTxObservation()
		phase := &DbPhaseObservation{}
		original := errors.New("synthetic callback panic")
		retry := OptRetryDefault()
		retry.retryMinTimeout, retry.retryMaxTimeout = time.Millisecond, time.Millisecond
		recovered := captureDbErrorPanic(func() {
			txWithPool(ctx, pool, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, "SELECT 1"))
				if check.name == "body panic" {
					panic(original)
				}
			}, retry, phase, observation)
		})
		cancel()
		if check.name == "body panic" && recovered != original || check.name == "lost commit" && recovered == nil || check.name != "body panic" && check.name != "lost commit" && recovered != nil {
			t.Fatalf("%s changed the original transaction outcome: %v", check.name, recovered)
		}
		snapshot := observation.Snapshot()
		if snapshot.BeginAttempts > 0 && !snapshot.LastBegin.Returned || snapshot.CommitAttempts > 0 && !snapshot.LastCommit.Returned || snapshot.RollbackAttempts > 0 && !snapshot.LastRollback.Returned {
			t.Fatal("returned wire boundary was not recorded", check.name, snapshot)
		}
		if check.name == "body panic" && phase.Phase() != DbOperationCallback || check.name == "lost commit" && phase.Phase() != DbOperationCommit {
			t.Fatal("cleanup changed the last business phase", check.name, phase.Phase())
		}
		snapshot.LastBegin, snapshot.LastCommit, snapshot.LastRollback = DbTxBoundaryObservation{}, DbTxBoundaryObservation{}, DbTxBoundaryObservation{}
		if snapshot.LastRelease.Disposition != DbTxReleaseIdle && snapshot.LastRelease.Disposition != DbTxReleaseDestroyTransferred {
			t.Fatal("returned wire release lost its concrete owner", check.name, snapshot.LastRelease)
		}
		snapshot.LastRelease = DbTxReleaseObservation{}
		if snapshot != check.want {
			t.Fatalf("%s census got %+v, want %+v", check.name, snapshot, check.want)
		}
		queries := func() []string {
			fixture.stateLock.Lock()
			defer fixture.stateLock.Unlock()
			var values []string
			for _, query := range fixture.queries {
				if query == "-- ping" {
					continue
				}
				if strings.HasPrefix(query, "begin") {
					query = "begin"
				}
				values = append(values, query)
			}
			return values
		}()
		if !reflect.DeepEqual(queries, check.queries) {
			t.Fatal("observation changed wire order", check.name, queries, check.queries)
		}
	}
}

// A canceled acquire records refusal but no BEGIN or business callback.
func TestDbTxObservationCanceledAcquireDoesNotInventAdmission(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	observation := NewDbTxObservation()
	recovered := captureDbErrorPanic(func() {
		txWithPool(ctx, pool, func(PgTx) { t.Error("canceled acquire ran business callback") }, OptNoRetry(), observation)
	})
	if recovered == nil || observation.Snapshot() != (DbTxObservationSnapshot{AcquireAttempts: 1}) {
		t.Fatal("refused acquire fabricated an admitted transaction", recovered, observation.Snapshot())
	}
}

// An actual refused BEGIN is distinct from acquisition and callback admission.
func TestDbTxObservationRefusedBeginDoesNotInventCallback(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
		fixture.queryError = func(_ int, query string) *pgproto3.ErrorResponse {
			if strings.HasPrefix(query, "begin") {
				return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "42501", Message: "synthetic BEGIN refusal"}
			}
			return nil
		}
	})
	observation := NewDbTxObservation()
	phase := &DbPhaseObservation{}
	recovered := captureDbErrorPanic(func() {
		txWithPool(t.Context(), pool, func(PgTx) { t.Error("refused BEGIN entered business callback") }, OptNoRetry(), phase, observation)
	})
	snapshot := observation.Snapshot()
	if recovered == nil || !snapshot.LastBegin.Returned || phase.Phase() != DbOperationBegin {
		t.Fatal("refused BEGIN lost its returned boundary", recovered, snapshot)
	}
	snapshot.LastBegin = DbTxBoundaryObservation{}
	if snapshot.LastRelease.Disposition != DbTxReleaseDestroyTransferred {
		t.Fatal("failed BEGIN did not transfer the closed connection", snapshot)
	}
	snapshot.LastRelease = DbTxReleaseObservation{}
	if snapshot != (DbTxObservationSnapshot{AcquireAttempts: 1, AcquireSucceeded: 1, BeginAttempts: 1, ReleaseAttempts: 1, ReleaseReturned: 1}) {
		t.Fatal("refused BEGIN fabricated an admitted business transaction", snapshot)
	}
}

// BEGIN may acknowledge after its caller stops; its wrapper closure does not
// count as a business callback, and actual detached rollback is still observed.
func TestDbTxObservationStoppedBeginKeepsCallbackCountZero(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	_, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
		if strings.HasPrefix(query, "begin") {
			close(entered)
			<-release
		}
		return true
	}, neverPingTestPool)
	watchdog, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	ctx, cancel := context.WithCancel(watchdog)
	defer cancel()
	observation := NewDbTxObservation()
	phase := &DbPhaseObservation{}
	joined := make(chan any, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		joined <- captureDbErrorPanic(func() {
			txWithPool(ctx, pool, func(PgTx) { panic("stopped BEGIN must not enter business callback") }, OptNoRetry(), phase, observation)
		})
	}()
	defer func() {
		cancel()
		once.Do(func() { close(release) })
		cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(watchdog), 5*time.Second)
		defer cancelCleanup()
		select {
		case <-finished:
		case <-cleanup.Done():
			t.Error("observed BEGIN owner did not join test cleanup")
		}
	}()
	select {
	case <-entered:
	case <-watchdog.Done():
		t.Fatal("BEGIN did not reach its acknowledged wire barrier")
	}
	cancel()
	once.Do(func() { close(release) })
	select {
	case recovered := <-joined:
		if recovered == nil {
			t.Fatal("canceled BEGIN unexpectedly completed its callback")
		}
	case <-watchdog.Done():
		t.Fatal("canceled BEGIN did not join detached rollback")
	}
	snapshot := observation.Snapshot()
	if snapshot.AcquireAttempts != 1 || snapshot.AcquireSucceeded != 1 || snapshot.BeginAttempts != 1 || snapshot.BeginSucceeded != 1 || snapshot.CallbackEntered != 0 ||
		snapshot.CommitAttempts != 0 || snapshot.RollbackAttempts != 1 || snapshot.RollbackAcknowledged != 1 || snapshot.LastBegin.AtReturn.State != "canceled" || phase.Phase() != DbOperationBegin {
		t.Fatal("BEGIN acknowledgment or cleanup fabricated business admission", snapshot, phase.Phase())
	}
}
