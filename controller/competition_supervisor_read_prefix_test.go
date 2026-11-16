// The actual authority helper must retain its exact read/local-guard prefix.
// Changed SQL and every unreviewed transaction method invalidate continuation.
package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/model"
)

// Force each actual authority-helper statement to fail at its real entry.
// This covers incident-shaped step three without copying any live data.
func TestCompetitionSupervisorReadPrefixActualAuthorityHelperRetainsClosedShape(t *testing.T) {
	for failedStatement := 3; failedStatement <= 5; failedStatement++ {
		observation := newCompetitionSupervisorRenewObservation()
		calls := 0
		original := competitionHeartbeatDiagnosticTx{
			exec: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
				calls++
				if calls == failedStatement {
					return pgconn.CommandTag{}, context.DeadlineExceeded
				}
				return pgconn.NewCommandTag("SELECT 1"), nil
			},
			queryRow: func(context.Context, string, ...any) pgx.Row {
				calls++
				return competitionHeartbeatDiagnosticRow(func(dest ...any) error {
					if calls == failedStatement {
						return context.DeadlineExceeded
					}
					if len(dest) != 0 {
						if value, ok := dest[0].(*bool); ok {
							*value = true
						}
					}
					return nil
				})
			},
		}
		wrapped := observation.observeReadPrefix(original, false)
		for _, statement := range competitionSupervisorReadPrefixStatements(false)[:2] {
			observation.step = statement.step
			if statement.kind == "exec" {
				_, _ = wrapped.Exec(t.Context(), statement.sql)
			} else {
				_ = wrapped.QueryRow(t.Context(), statement.sql).Scan()
			}
		}
		observation.step = competitionSupervisorRenewalAuthority
		settings := &Settings{CompetitionId: "synthetic-competition", AutomationPolicy: model.CompetitionAutomationPolicy{Enabled: true, Environment: "synthetic", SeasonId: "synthetic-season"}}
		lease := model.CompetitionSupervisorLease{Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId, OwnerId: "synthetic-owner", FencingToken: 1}
		err := (PostgresStore{}).requireAutomationLeaseInTx(t.Context(), wrapped, settings, lease)
		if !errors.Is(err, context.DeadlineExceeded) || calls != failedStatement || !observation.readPrefix.valid {
			t.Fatal("actual authority helper escaped its reviewed read/local-guard shape", failedStatement, calls, observation.readPrefix, err)
		}
		proof := competitionSupervisorReadLockFailure()
		proof.step, proof.readPrefix = observation.step, observation.readPrefix
		if !competitionSupervisorReadLockDeadline(proof) || proof.transaction.LastRelease.CleanupComplete {
			t.Fatal("closed authority prefix required physical reuse or refused its acknowledged rollback", failedStatement)
		}
	}
}

// The wrapper delegates changed SQL exactly once, but never authenticates it
// as a read-only prefix, even when the semantic step is unchanged.
func TestCompetitionSupervisorReadPrefixRejectsChangedOrAdditionalSql(t *testing.T) {
	for _, sql := range []string{"UPDATE synthetic_other_table SET value=1", "COMMIT", "SELECT pg_advisory_xact_lock(hashtextextended($1,0)); COMMIT"} {
		original := &competitionHeartbeatSurfaceTx{}
		observation := newCompetitionSupervisorRenewObservation()
		wrapped := observation.observeReadPrefix(original, false)
		observation.step = competitionSupervisorRenewalScopeLock
		_, _ = wrapped.Exec(t.Context(), sql)
		if observation.readPrefix.valid || original.calls != 1 {
			t.Fatal("changed SQL gained closed-prefix authority or altered delegation")
		}
	}
}

// All other methods use the existing explicit closed pgx surface. These
// calls must invalidate proof before raw/nested transaction access escapes.
func TestCompetitionSupervisorReadPrefixRejectsEveryOtherPgxMethod(t *testing.T) {
	for _, invoke := range []func(pgx.Tx){
		func(tx pgx.Tx) { _, _ = tx.Begin(t.Context()) },
		func(tx pgx.Tx) { _ = tx.Commit(t.Context()) },
		func(tx pgx.Tx) { _ = tx.Rollback(t.Context()) },
		func(tx pgx.Tx) { _, _ = tx.Query(t.Context(), "synthetic query") },
		func(tx pgx.Tx) { _ = tx.SendBatch(t.Context(), &pgx.Batch{}) },
		func(tx pgx.Tx) { _, _ = tx.CopyFrom(t.Context(), nil, nil, nil) },
		func(tx pgx.Tx) { _, _ = tx.Prepare(t.Context(), "synthetic", "synthetic query") },
		func(tx pgx.Tx) { _ = tx.Conn() },
		func(tx pgx.Tx) { _ = tx.LargeObjects() },
	} {
		original := &competitionHeartbeatSurfaceTx{}
		observation := newCompetitionSupervisorRenewObservation()
		wrapped := observation.observeReadPrefix(original, false)
		invoke(wrapped)
		if observation.readPrefixBypass.unknown != 1 || original.calls != 1 {
			t.Fatal("another pgx method escaped the closed supervisor prefix")
		}
	}
}

// The renewal writer's prefix and the worker's read-only operation are
// independently selected classes; neither semantic label can borrow the other.
func TestCompetitionSupervisorReadPrefixCannotCrossWorkerAndWriter(t *testing.T) {
	proof := competitionSupervisorReadLockFailure()
	if !competitionSupervisorReadLockDeadline(proof) || competitionSupervisorWorkerFenceDeadline(proof) {
		t.Fatal("writer prefix was confused with the worker's read operation")
	}
	proof.step = competitionSupervisorWorkerFenceRead
	proof.readPrefix = competitionSupervisorReadPrefixSnapshot{workerFence: true, valid: true, count: 1, returned: 1}
	if !competitionSupervisorWorkerFenceDeadline(proof) || competitionSupervisorReadLockDeadline(proof) {
		t.Fatal("worker read prefix was confused with a renewal writer")
	}
}
