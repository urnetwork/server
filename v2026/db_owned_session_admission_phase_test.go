package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// An unreturned probe is not an acknowledged busy key. It poisons the borrowed
// session while retaining the checkout, and the next attempt refuses before
// issuing any SQL. A lost physical connection proves no advisory lock custody.
func TestOwnedSessionProbeLossDistinguishesAdmissionFromStickyPreparation(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
		return !strings.Contains(query, "pg_try_advisory_lock(owner.first,owner.second)")
	}, func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) { fixture.queryRows = ownedSessionWireRows })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var events []PgOwnershipEventKind
	ctx = Testing_WithPgOwnershipObservation(ctx, func(event PgOwnershipEvent) {
		events = append(events, event.Kind)
	})
	conn := RaisePgResult(pool.open().Acquire(ctx))
	defer conn.Release()
	session := NewPgOwnedSession(conn)
	phase := &DbPhaseObservation{}
	calls := 0
	failure := captureDbErrorPanic(func() {
		session.txWithResource(ctx, []PgOwnershipKey{{first: 17, second: 29}}, ownedTransactionWireResource(),
			func(PgTx) { calls++ }, phase, OptNoRetry())
	})
	if failure == nil || failure != session.Err() || calls != 0 || ctx.Err() != nil ||
		phase.Phase() != DbOperationAdmission || phase.AdmissionStage() != DbAdmissionProbe || pool.open().Stat().AcquiredConns() != 1 ||
		len(events) != 1 || events[0] != PgOwnershipUncertain {
		t.Fatal("probe loss became a known busy key, entered business, or lost its source phase", failure, phase.Phase(), events)
	}
	fixture.stateLock.Lock()
	queries := append([]string(nil), fixture.queries...)
	fixture.stateLock.Unlock()
	probes := 0
	for _, query := range queries {
		if strings.Contains(query, "pg_try_advisory_lock(owner.first,owner.second)") {
			probes++
		}
		if strings.HasPrefix(query, "begin") || strings.Contains(query, "pg_advisory_unlock") {
			t.Fatal("unreturned probe triggered a transaction or guessed reference cleanup")
		}
	}
	if probes != 1 {
		t.Fatal("unreturned probe was not the single failed admission attempt", probes)
	}

	retryPhase := &DbPhaseObservation{}
	refused := captureDbErrorPanic(func() {
		session.txWithResource(ctx, []PgOwnershipKey{{first: 17, second: 29}}, ownedTransactionWireResource(),
			func(PgTx) { calls++ }, retryPhase, OptNoRetry())
	})
	fixture.stateLock.Lock()
	queryCount := len(fixture.queries)
	fixture.stateLock.Unlock()
	if refused != failure || session.Err() != failure || calls != 0 ||
		retryPhase.Phase() != DbOperationOwnershipConfiguration || queryCount != len(queries) ||
		len(events) != 1 || pool.open().Stat().AcquiredConns() != 1 {
		t.Fatal("sticky preparation refusal retried SQL, changed the original failure, or surrendered the caller's checkout", refused, retryPhase.Phase())
	}
}
