package server

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Only a fully returned refusal and successful partial-reference cleanup can
// become a busy wait. The same pre-BEGIN phase also contains distinct faults.
func TestOwnedSessionAdmissionStagesKeepAcknowledgedBusySeparate(t *testing.T) {
	for _, stage := range []DbAdmissionStage{DbAdmissionPrecheck, DbAdmissionProbe, DbAdmissionCleanup, DbAdmissionAcknowledgedBusyWait} {
		func() {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
				return !(stage == DbAdmissionProbe && strings.Contains(query, "pg_try_advisory_lock(owner.first,owner.second)")) &&
					!(stage == DbAdmissionCleanup && strings.Contains(query, "pg_advisory_unlock(owner.first,owner.second)"))
			}, func(context.Context, pgxpool.ShouldPingParams) bool { return false },
				func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
					fixture.queryRows = func(backend int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
						fields, rows := ownedSessionWireRows(backend, query)
						if strings.Contains(query, "pg_try_advisory_lock(owner.first,owner.second)") {
							// First key is acquired; the second is an acknowledged
							// refusal. Cleanup must decrement only the first key.
							rows = append(rows, [][]byte{rows[0][0], []byte("31"), []byte("43"), []byte("f")})
						}
						return fields, rows
					}
				})
			conn := RaisePgResult(pool.open().Acquire(ctx))
			defer conn.Release()
			waiting := 0
			ctx = Testing_WithPgOwnershipObservation(ctx, func(event PgOwnershipEvent) {
				if event.Kind == PgOwnershipWaiting {
					waiting++
					cancel()
				}
			})
			if stage == DbAdmissionPrecheck {
				cancel()
			}
			session := NewPgOwnedSession(conn)
			phase := &DbPhaseObservation{}
			calls := 0
			failure := captureDbErrorPanic(func() {
				session.txWithResource(ctx, []PgOwnershipKey{{first: 17, second: 29}, {first: 31, second: 43}},
					ownedTransactionWireResource(), func(PgTx) { calls++ }, phase, OptNoRetry())
			})
			if failure == nil || calls != 0 || phase.Phase() != DbOperationAdmission || phase.AdmissionStage() != stage ||
				pool.open().Stat().AcquiredConns() != 1 {
				t.Fatal("admission observation changed the failure boundary or caller checkout", stage, failure, phase.AdmissionStage())
			}
			wantWaiting := 0
			if stage == DbAdmissionAcknowledgedBusyWait {
				wantWaiting = 1
			}
			if waiting != wantWaiting || (session.Err() != nil) != (stage == DbAdmissionProbe || stage == DbAdmissionCleanup) {
				t.Fatal("unacknowledged transport became a healthy busy wait", stage, waiting, session.Err())
			}
			fixture.stateLock.Lock()
			queries := append([]string(nil), fixture.queries...)
			fixture.stateLock.Unlock()
			probes, unlocks := 0, 0
			for _, query := range queries {
				if strings.Contains(query, "pg_try_advisory_lock(owner.first,owner.second)") {
					probes++
				}
				if strings.Contains(query, "pg_advisory_unlock(owner.first,owner.second)") {
					unlocks++
				}
				if strings.HasPrefix(query, "begin") || strings.Contains(query, "pg_advisory_unlock_all") {
					t.Fatal("admission-only observation entered business or released inherited ownership")
				}
			}
			wantProbes, wantUnlocks := 1, 0
			if stage == DbAdmissionPrecheck {
				wantProbes = 0
			}
			if stage == DbAdmissionCleanup || stage == DbAdmissionAcknowledgedBusyWait {
				wantUnlocks = 1
			}
			if probes != wantProbes || unlocks != wantUnlocks {
				t.Fatal("admission subtype changed query or cleanup work", stage, probes, unlocks)
			}
		}()
	}
}

// The pooled helper also checks cancellation before a probe. An acquirer that
// returns at that boundary must not report a Query which was never attempted.
func TestOwnedTransactionAdmissionPrecheckDoesNotInventProbe(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	phase := &DbPhaseObservation{}
	calls := 0
	failure := captureDbErrorPanic(func() {
		ownedTxWithResource(ctx, []PgOwnershipKey{{first: 17, second: 29}}, ownedTransactionWireResource(),
			func(ctx context.Context) (PgConn, error) {
				conn, err := pool.open().Acquire(ctx)
				cancel()
				return conn, err
			}, func(PgTx) { calls++ }, phase, OptNoRetry())
	})
	err, ok := failure.(error)
	if !ok || !errors.Is(err, context.Canceled) || calls != 0 || phase.Phase() != DbOperationAdmission || phase.AdmissionStage() != DbAdmissionPrecheck {
		t.Fatal("canceled pooled precheck became an ownership probe", failure, phase.AdmissionStage())
	}
	fixture.stateLock.Lock()
	queries := append([]string(nil), fixture.queries...)
	fixture.stateLock.Unlock()
	for _, query := range queries {
		if strings.Contains(query, "pg_try_advisory_lock") || strings.HasPrefix(query, "begin") {
			t.Fatal("canceled pooled precheck issued ownership or business SQL")
		}
	}
}

type admissionStageDeadlineConn struct {
	net.Conn
	timedOut chan struct{}
	once     *sync.Once
}

func (self *admissionStageDeadlineConn) Read(p []byte) (int, error) {
	n, err := self.Conn.Read(p)
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		self.once.Do(func() { close(self.timedOut) })
	}
	return n, err
}

// The real admission timer can expire during a pgx probe, retaining the typed
// deadline as the sticky cause. This is distinct from an acknowledged refusal.
func TestOwnedSessionProbeDeadlineRetainsCauseForPreparation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	probeStarted, readTimedOut, releaseProbe := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var released, timedOut sync.Once
	release := func() { released.Do(func() { close(releaseProbe) }) }
	defer release()
	fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
		if strings.Contains(query, "pg_try_advisory_lock(owner.first,owner.second)") {
			close(probeStarted)
			<-releaseProbe
			return false
		}
		return true
	}, func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
			fixture.queryRows = ownedSessionWireRows
			dial := config.ConnConfig.DialFunc
			config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := dial(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return &admissionStageDeadlineConn{Conn: conn, timedOut: readTimedOut, once: &timedOut}, nil
			}
		})
	conn := RaisePgResult(pool.open().Acquire(ctx))
	defer conn.Release()
	session := NewPgOwnedSession(conn)
	phase := &DbPhaseObservation{}
	calls := 0
	done := make(chan any, 1)
	joined := false
	go func() {
		done <- captureDbErrorPanic(func() {
			session.txWithResource(ctx, []PgOwnershipKey{{first: 17, second: 29}}, ownedTransactionWireResource(),
				func(PgTx) { calls++ }, phase, OptNoRetry())
		})
	}()
	defer func() {
		cancel()
		release()
		if !joined {
			<-done
		}
	}()
	select {
	case <-probeStarted:
	case <-ctx.Done():
		t.Fatal("ownership probe did not reach its actual wire boundary", ctx.Err())
	}
	select {
	case <-readTimedOut:
	case <-ctx.Done():
		t.Fatal("ownership probe did not reach the production admission deadline", ctx.Err())
	}
	release()
	var failure any
	select {
	case failure = <-done:
		joined = true
	case <-ctx.Done():
		t.Fatal("deadline probe did not join before caller cleanup", ctx.Err())
	}
	err, ok := failure.(error)
	if !ok || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || failure != session.Err() || calls != 0 ||
		phase.Phase() != DbOperationAdmission || phase.AdmissionStage() != DbAdmissionProbe || pool.open().Stat().AcquiredConns() != 1 {
		t.Fatal("probe deadline became acknowledged busy or lost its typed sticky cause", failure, phase.AdmissionStage())
	}
	fixture.stateLock.Lock()
	queryCount := len(fixture.queries)
	fixture.stateLock.Unlock()
	retryPhase := &DbPhaseObservation{}
	refused := captureDbErrorPanic(func() {
		session.txWithResource(ctx, []PgOwnershipKey{{first: 17, second: 29}}, ownedTransactionWireResource(),
			func(PgTx) { calls++ }, retryPhase, OptNoRetry())
	})
	fixture.stateLock.Lock()
	afterQueryCount := len(fixture.queries)
	fixture.stateLock.Unlock()
	if refused != failure || calls != 0 || retryPhase.Phase() != DbOperationOwnershipConfiguration || afterQueryCount != queryCount {
		t.Fatal("sticky deadline preparation retried SQL or changed its original cause", refused, retryPhase.Phase())
	}
}
