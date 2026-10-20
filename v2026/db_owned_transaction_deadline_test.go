// A deadline reported after an admitted statement drains is still an error,
// but a successful rollback leaves its physical session reusable. These wire
// controls hold the exact validation or cleanup reply, expire the caller at
// that barrier, and distinguish acknowledged cleanup from transport loss.
package server

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The business statement models journal cleanup without production identities.
const ownedDeadlineCleanupStatement = "DELETE FROM synthetic_applied_journal"

// One completed attempt and its exact physical owner. The result channel joins
// all writes made by the attempt before the test inspects them.
type ownedTxDeadlineAttempt struct {
	fixture        *pgPoolWireFixture
	pool           *safePgPool
	physical       *pgconn.PgConn
	forceIdle      atomic.Bool
	rawDials       atomic.Int32
	acquires       int
	callbacks      int
	afterStatement int
	posts          int
	events         []PgOwnershipEventKind
	statementErr   error
	failure        error
}

// Holds exactly one real pgx reply. Native cancellation propagation supplies
// the expiration barrier; the existing deadline context preserves its class.
func runOwnedTxDeadline(t *testing.T, heldStatement string, acknowledgeStatement, acknowledgeRollback bool, statementFailure *pgproto3.ErrorResponse) *ownedTxDeadlineAttempt {
	t.Helper()
	attempt := &ownedTxDeadlineAttempt{}
	arrived := make(chan struct{})
	reply := make(chan bool, 1)
	var held atomic.Bool
	attempt.fixture, attempt.pool = newPgPoolWireFixture(t, func(_ int, query string) bool {
		if query == heldStatement && held.CompareAndSwap(false, true) {
			close(arrived)
			return <-reply
		}
		return query != "rollback" || acknowledgeRollback
	}, func(context.Context, pgxpool.ShouldPingParams) bool {
		return attempt.forceIdle.Swap(false)
	}, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		fixture.queryRows = ownedTransactionWireRows
		fixture.queryError = func(_ int, query string) *pgproto3.ErrorResponse {
			if query == heldStatement {
				return statementFailure
			}
			return nil
		}
		dial := config.ConnConfig.DialFunc
		config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
			attempt.rawDials.Add(1)
			return dial(ctx, network, address)
		}
	})
	// Release a held protocol worker before the fixture joins it on failure.
	t.Cleanup(func() {
		select {
		case reply <- false:
		default:
		}
	})
	warm := RaisePgResult(attempt.pool.open().Acquire(t.Context()))
	attempt.physical = warm.Conn().PgConn()
	warm.Release()
	base, expire := context.WithCancel(t.Context())
	defer expire()
	ctx := Testing_WithPgOwnershipObservation(prewriteDeadlineContext{Context: base}, func(event PgOwnershipEvent) {
		attempt.events = append(attempt.events, event.Kind)
	})
	done := make(chan any, 1)
	go func() {
		done <- captureDbErrorPanic(func() {
			ownedTxWithResourcePolicy(ctx, []PgOwnershipKey{NewPgOwnershipKey("owned-deadline", NewId())},
				ownedTransactionWireResource(), func(ctx context.Context) (PgConn, error) {
					attempt.acquires++
					return attempt.pool.open().Acquire(ctx)
				}, false, func(tx PgTx) {
					attempt.callbacks++
					AddTxPostCommit(tx, "must-not-publish-stopped-cleanup", func() any {
						attempt.posts++
						return nil
					})
					_, attempt.statementErr = tx.Exec(ctx, ownedDeadlineCleanupStatement)
					Raise(attempt.statementErr)
					attempt.afterStatement++
				}, TxReadCommitted, OptRetryDefault(), txStatementStopTimeout(statementStopTestTimeout))
		})
	}()
	<-arrived
	expire()
	reply <- acknowledgeStatement
	recovered := <-done
	attempt.failure, _ = recovered.(error)
	if attempt.failure == nil || !errors.Is(attempt.failure, context.DeadlineExceeded) {
		t.Fatal("owned deadline lost its original failure class", recovered)
	}
	if statementFailure == nil && !errors.Is(attempt.failure, DbContextDoneError) {
		t.Fatal("owned deadline lost its existing done classification", attempt.failure)
	}
	if attempt.statementErr != nil && !errors.Is(attempt.failure, attempt.statementErr) {
		t.Fatal("owned cleanup replaced its original statement failure", attempt.failure)
	}
	if attempt.acquires != 1 || attempt.afterStatement != 0 || attempt.posts != 0 {
		t.Fatal("stopped owned work replayed or published", attempt.acquires, attempt.afterStatement, attempt.posts)
	}
	return attempt
}

// Counts exact wire operations without disclosing any production statement.
func (self *ownedTxDeadlineAttempt) queryCount(statement string) int {
	self.fixture.stateLock.Lock()
	defer self.fixture.stateLock.Unlock()
	count := 0
	for _, query := range self.fixture.queries {
		if query == statement || statement == "begin" && strings.HasPrefix(query, "begin") {
			count++
		}
	}
	return count
}

// A rollback and unlock must be acknowledged before the slot is reusable. The
// next forced idle check must still run on that same physical connection.
func (self *ownedTxDeadlineAttempt) requireReusable(t *testing.T) {
	t.Helper()
	if self.physical.IsClosed() || self.physical.IsBusy() || self.physical.TxStatus() != 'I' || self.rawDials.Load() != 1 {
		t.Fatal("acknowledged owned rollback discarded its healthy session", self.physical.IsClosed(), self.physical.IsBusy(), self.physical.TxStatus(), self.rawDials.Load())
	}
	if self.queryCount("begin") != 1 || self.queryCount("rollback") != 1 || self.queryCount("commit") != 0 || self.queryCount("SELECT pg_advisory_unlock_all()") != 1 {
		t.Fatal("stopped owned transaction did not roll back and unlock exactly once")
	}
	if len(self.events) != 2 || self.events[0] != PgOwnershipAdmitted || self.events[1] != PgOwnershipReleased {
		t.Fatal("acknowledged rollback lost its ownership release", self.events)
	}
	if self.pool.open().Stat().AcquiredConns() != 0 {
		t.Fatal("stopped owned transaction retained the checkout")
	}
	self.forceIdle.Store(true)
	borrowed := RaisePgResult(self.pool.open().Acquire(t.Context()))
	defer borrowed.Release()
	if borrowed.Conn().PgConn() != self.physical {
		t.Fatal("next borrower replaced the acknowledged idle session")
	}
	RaisePgResult(borrowed.Exec(t.Context(), "SELECT 1"))
	self.fixture.stateLock.Lock()
	dials, pings := self.fixture.dialCount, self.fixture.pingCount
	self.fixture.stateLock.Unlock()
	if dials != 1 || pings != 2 || self.rawDials.Load() != 1 {
		t.Fatal("session reuse skipped borrower health validation or opened a cancel transport", dials, pings, self.rawDials.Load())
	}
}

// Unknown physical state must still discard, and its replacement must pass
// startup validation before a fresh borrower receives it.
func (self *ownedTxDeadlineAttempt) requireDiscarded(t *testing.T) {
	t.Helper()
	if !self.physical.IsClosed() || self.queryCount("commit") != 0 || self.queryCount("SELECT pg_advisory_unlock_all()") != 0 {
		t.Fatal("uncertain owned cleanup reused or acknowledged its session")
	}
	if len(self.events) != 2 || self.events[0] != PgOwnershipAdmitted || self.events[1] != PgOwnershipUncertain {
		t.Fatal("uncertain owned cleanup invented an acknowledged release", self.events)
	}
	borrowed := RaisePgResult(self.pool.open().Acquire(t.Context()))
	defer borrowed.Release()
	if borrowed.Conn().PgConn() == self.physical {
		t.Fatal("next borrower received the uncertain physical connection")
	}
	RaisePgResult(borrowed.Exec(t.Context(), "SELECT 1"))
	self.fixture.stateLock.Lock()
	dials, pings := self.fixture.dialCount, self.fixture.pingCount
	self.fixture.stateLock.Unlock()
	if dials != 2 || pings != 2 {
		t.Fatal("uncertain session replacement bypassed startup validation", dials, pings)
	}
}

// A held backend validation reply must not enter the cleanup callback after
// expiration; its drained query and acknowledged rollback keep the session.
func TestTryOwnedTxDeadlineDuringBackendValidationReusesSession(t *testing.T) {
	attempt := runOwnedTxDeadline(t, "SELECT pg_backend_pid()", true, true, nil)
	if attempt.callbacks != 0 || attempt.statementErr != nil || attempt.queryCount(ownedDeadlineCleanupStatement) != 0 {
		t.Fatal("expired backend validation entered cleanup SQL", attempt.callbacks, attempt.statementErr)
	}
	attempt.requireReusable(t)
}

// A cleanup reply arriving after expiration still reports the deadline and
// rolls back. Neither the error nor an acknowledged rollback condemns a socket.
func TestTryOwnedTxDeadlineDuringCleanupStatementReusesSession(t *testing.T) {
	attempt := runOwnedTxDeadline(t, ownedDeadlineCleanupStatement, true, true, nil)
	if attempt.callbacks != 1 || !errors.Is(attempt.statementErr, context.DeadlineExceeded) || attempt.queryCount(ownedDeadlineCleanupStatement) != 1 {
		t.Fatal("cleanup did not stop once at the held statement", attempt.callbacks, attempt.statementErr)
	}
	attempt.requireReusable(t)
}

// A successful statement reply cannot authorize reuse when rollback loses its
// own reply. The statement's deadline remains the primary outcome.
func TestTryOwnedTxDeadlineFailedRollbackDiscardsSession(t *testing.T) {
	attempt := runOwnedTxDeadline(t, ownedDeadlineCleanupStatement, true, false, nil)
	if attempt.callbacks != 1 || !errors.Is(attempt.statementErr, context.DeadlineExceeded) || attempt.queryCount("rollback") != 1 {
		t.Fatal("fixture did not lose the cleanup rollback after the stopped statement")
	}
	attempt.requireDiscarded(t)
}

// Losing the held statement's reply remains uncertain even though caller
// expiration is also reported. No automatic rerun may hide that transport loss.
func TestTryOwnedTxDeadlineLostStatementReplyDiscardsSession(t *testing.T) {
	attempt := runOwnedTxDeadline(t, ownedDeadlineCleanupStatement, false, true, nil)
	if attempt.callbacks != 1 || !errors.Is(attempt.statementErr, context.DeadlineExceeded) || attempt.queryCount(ownedDeadlineCleanupStatement) != 1 {
		t.Fatal("fixture did not retain the lost statement reply and deadline")
	}
	attempt.requireDiscarded(t)
}

// A PostgreSQL statement timeout acknowledged after the caller expires keeps
// both causes. Successful rollback and unlock permit normal validated reuse.
func TestTryOwnedTxAcknowledgedStatementTimeoutReusesSession(t *testing.T) {
	attempt := runOwnedTxDeadline(t, ownedDeadlineCleanupStatement, true, true,
		&pgproto3.ErrorResponse{Severity: "ERROR", Code: "57014", Message: "synthetic statement timeout"})
	var pgErr *pgconn.PgError
	if attempt.callbacks != 1 || !errors.As(attempt.failure, &pgErr) || pgErr.Code != "57014" {
		t.Fatal("acknowledged statement timeout lost its database cause", attempt.failure)
	}
	attempt.requireReusable(t)
}

// A server-declared connection failure still condemns an open idle connection
// even after rollback; physical idleness alone cannot override that authority.
func TestTryOwnedTxServerConnectionFailureDiscardsSession(t *testing.T) {
	attempt := runOwnedTxDeadline(t, ownedDeadlineCleanupStatement, true, true,
		&pgproto3.ErrorResponse{Severity: "ERROR", Code: "08006", Message: "synthetic connection failure"})
	var pgErr *pgconn.PgError
	if attempt.callbacks != 1 || !errors.As(attempt.failure, &pgErr) || pgErr.Code != "08006" || attempt.queryCount("rollback") != 1 {
		t.Fatal("server connection failure lost its authority after rollback", attempt.failure)
	}
	attempt.requireDiscarded(t)
}
