// Actual disposable-PG calls authenticate the diagnostic decorator's wiring
// and rollback acknowledgments, not the cause of a production latency event.
package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func competitionHeartbeatStatementAt(snapshot *competitionHeartbeatDatabaseSnapshot, step competitionHeartbeatDatabaseStep, ordinal uint8) *competitionHeartbeatStatementObservation {
	if snapshot != nil {
		for _, statement := range snapshot.Statements {
			if statement.Step == step && statement.Ordinal == ordinal {
				return &statement
			}
		}
	}
	return nil
}

// A real lazy-row refusal returns from Scan, then acknowledges rollback. It
// never fabricates a job read, a write or a transaction commit acknowledgment.
func TestCompetitionHeartbeatDatabasePgMissingSlotBoundary(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture, job := newCompetitionLeasePgFixture(t, ctx, "diagnostic-missing-slot")
		before := readCompetitionLeasePgSnapshot(ctx, job.JobId)
		err := fixture.store.Heartbeat(ctx, fixture.settings, "synthetic-other-worker", job.JobId)
		snapshot := competitionHeartbeatFailureDetails(err).HeartbeatDatabase
		statement := competitionHeartbeatStatementAt(snapshot, competitionHeartbeatDatabaseSlotLock, 1)
		if !errors.Is(err, ErrLeaseLost) || snapshot == nil || snapshot.Step != competitionHeartbeatDatabaseSlotLock || snapshot.CallbackEntries != 1 ||
			statement == nil || !statement.Returned || statement.Succeeded || statement.Kind != "row_scan" ||
			snapshot.Transaction.AcquireAttempts != 1 || snapshot.Transaction.AcquireSucceeded != 1 || snapshot.Transaction.BeginSucceeded != 1 ||
			snapshot.Transaction.CallbackEntered != 1 || snapshot.Transaction.CommitAttempts != 0 || snapshot.Transaction.RollbackAttempts != 1 || snapshot.Transaction.RollbackAcknowledged != 1 ||
			competitionHeartbeatStatementAt(snapshot, competitionHeartbeatDatabaseJobLock, 1) != nil {
			t.Fatalf("actual missing-slot failure lost its source/terminal boundary: %+v", snapshot)
		}
		if after := readCompetitionLeasePgSnapshot(ctx, job.JobId); !reflect.DeepEqual(after, before) {
			t.Fatal("diagnostic heartbeat changed authority after row refusal")
		}
	})
}

// A returned UPDATE with zero affected rows differs from a failed statement.
// Its preceding slot update still rolls back under the exact existing policy.
func TestCompetitionHeartbeatDatabasePgSuppressedUpdateBoundary(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture, job := newCompetitionLeasePgFixture(t, ctx, "diagnostic-suppressed-update")
		before := readCompetitionLeasePgSnapshot(ctx, job.JobId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_diagnostic_suppress_update() RETURNS trigger AS $body$
			 BEGIN RETURN NULL; END $body$ LANGUAGE plpgsql;
			 CREATE TRIGGER synthetic_diagnostic_suppress_update BEFORE UPDATE OF lease_expires_at ON competition_job
			 FOR EACH ROW EXECUTE FUNCTION synthetic_diagnostic_suppress_update();`))
		})
		fixture.clock = fixture.clock.Add(time.Second)
		err := fixture.store.Heartbeat(ctx, fixture.settings, "synthetic-lease-worker", job.JobId)
		snapshot := competitionHeartbeatFailureDetails(err).HeartbeatDatabase
		jobUpdate := competitionHeartbeatStatementAt(snapshot, competitionHeartbeatDatabaseJobUpdate, 1)
		slotUpdate := competitionHeartbeatStatementAt(snapshot, competitionHeartbeatDatabaseSlotUpdate, 1)
		if !errors.Is(err, ErrLeaseLost) || snapshot == nil || snapshot.Step != competitionHeartbeatDatabaseJobUpdate ||
			jobUpdate == nil || !jobUpdate.Returned || !jobUpdate.Succeeded || jobUpdate.Kind != "exec" ||
			slotUpdate == nil || !slotUpdate.Returned || !slotUpdate.Succeeded ||
			snapshot.Transaction.CommitAttempts != 0 || snapshot.Transaction.RollbackAttempts != 1 || snapshot.Transaction.RollbackAcknowledged != 1 {
			t.Fatalf("actual zero-row UPDATE was mistaken for an unreturned statement: %+v", snapshot)
		}
		if after := readCompetitionLeasePgSnapshot(ctx, job.JobId); !reflect.DeepEqual(after, before) {
			t.Fatal("diagnostic heartbeat committed a partial lease extension")
		}
	})
}
