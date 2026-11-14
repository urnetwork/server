// Disposable PostgreSQL fixtures observe the actual renewal store and machine
// report. They do not qualify a live pool, network route or broader retry rule.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Read the actual TestEnv-owned database, then release that connection before
// fixture construction or constrained-pool ownership begins.
func requireCompetitionSupervisorDiagnosticsSchema(t testing.TB, ctx context.Context) {
	t.Helper()
	var head int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(max(end_version_number),0) FROM migration_audit WHERE status='success'`).Scan(&head))
	})
	if head != server.MigrationCount() {
		t.Fatalf("supervisor diagnostic fixture schema=%d want=%d", head, server.MigrationCount())
	}
	t.Logf("actual_supervisor_diagnostic_fixture_schema_head=%d", head)
}

// The source-only RED restores just the old renewal store method: a real
// refused acquire remains safe but the composed report loses its diagnostic.
func TestCompetitionSupervisorRenewalPgDiagnosticsObserveUnadmittedPool(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		requireCompetitionSupervisorDiagnosticsSchema(t, ctx)
		fixture := newCompetitionAutomationFixture(t)
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-diagnostic-owner", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		restore := competitionSupervisorAcquirePgPool(t)
		defer restore()
		release := holdCompetitionSupervisorAcquirePgPool(t, ctx)
		defer release()
		child, stop := context.WithTimeout(ctx, time.Second)
		result, err := fixture.store.RenewAutomationLease(child, fixture.settings, *lease, time.Minute)
		stop()
		if result != nil || !errors.Is(err, context.DeadlineExceeded) || !competitionSupervisorUnadmittedDeadline(err) {
			t.Fatal("diagnostics changed actual unadmitted renewal behavior", result, err)
		}
		details := CompetitionSupervisorRenewalFailureDetails(err)
		if details == nil {
			t.Fatal("actual renewal machine report omitted its database diagnostic")
		}
		transaction := details.Transaction
		if details.DatabasePhase != server.DbOperationAcquire || details.Step != competitionSupervisorRenewalUnknown || details.CallbackEntries != 0 ||
			!details.ConfirmedLeaseExpiresAt.Equal(lease.LeaseExpiresAt) || details.AtEntry.State != "live" || details.AtReturn.State != "deadline" ||
			details.Acquire.PoolEntry.Maximum != 1 || details.Acquire.PoolEntry.Acquired != 1 || details.Acquire.PoolReturn == nil || details.Acquire.Admitted ||
			transaction.AcquireAttempts != 1 || transaction.AcquireSucceeded != 0 || transaction.BeginAttempts != 0 || transaction.CallbackEntered != 0 ||
			transaction.CommitAttempts != 0 || transaction.RollbackAttempts != 0 || transaction.ReleaseAttempts != 0 {
			t.Fatal("real unadmitted pool diagnostic lost exact boundary counts", details)
		}
		content, marshalErr := MarshalCompetitionSupervisorReport("lease_lost", err)
		var report struct {
			RenewalDatabase *CompetitionSupervisorRenewalDatabaseFailure `json:"renewal_database"`
		}
		if marshalErr != nil || json.Unmarshal(content, &report) != nil || report.RenewalDatabase == nil || report.RenewalDatabase.Transaction != transaction {
			t.Fatal("actual failed acquisition did not reach the machine-report serializer", marshalErr)
		}
		release()
		fixture.now = fixture.now.Add(time.Second)
		next, err := fixture.store.RenewAutomationLease(ctx, fixture.settings, *lease, time.Minute)
		if err != nil || next == nil || next.FencingToken != lease.FencingToken || !next.LeaseExpiresAt.After(lease.LeaseExpiresAt) {
			t.Fatal("observing refusal changed later healthy renewal", next, err)
		}
	})
}

// Cancellation is forced after the real scope-lock statement and before the
// real lease-row read. A returned rollback cannot be mislabeled unadmitted.
func TestCompetitionSupervisorRenewalPgDiagnosticsObserveCallbackRollback(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		requireCompetitionSupervisorDiagnosticsSchema(t, ctx)
		fixture := newCompetitionAutomationFixture(t)
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-diagnostic-callback-owner", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		child, stop := context.WithCancel(ctx)
		defer stop()
		calls := 0
		fixture.store.beforeLeaseRowLockForTest = func(kind string) {
			calls++
			if kind != "supervisor" {
				panic(errors.New("synthetic wrong source boundary"))
			}
			stop()
		}
		result, err := fixture.store.RenewAutomationLease(child, fixture.settings, *lease, time.Minute)
		details := CompetitionSupervisorRenewalFailureDetails(err)
		if result != nil || !errors.Is(err, context.Canceled) || calls != 1 || details == nil || competitionSupervisorUnadmittedDeadline(err) {
			t.Fatal("real callback cancellation changed cause or gained retry authority", result, calls, details, err)
		}
		transaction := details.Transaction
		if details.DatabasePhase != server.DbOperationCallback || details.Step != competitionSupervisorRenewalLeaseRowLock || details.CallbackEntries != 1 ||
			details.AtEntry.State != "live" || details.AtReturn.State != "canceled" || !details.ConfirmedLeaseExpiresAt.Equal(lease.LeaseExpiresAt) ||
			transaction.AcquireAttempts != 1 || transaction.AcquireSucceeded != 1 || transaction.BeginAttempts != 1 || transaction.BeginSucceeded != 1 ||
			transaction.CallbackEntered != 1 || transaction.CommitAttempts != 0 || transaction.CommitAcknowledged != 0 ||
			transaction.RollbackAttempts != 1 || !transaction.LastRollback.Returned || transaction.ReleaseAttempts != 1 || transaction.ReleaseReturned != 1 {
			t.Fatal("actual callback failure lost BEGIN/rollback/release evidence", details)
		}
		var expiry time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT lease_expires_at FROM competition_supervisor_lease
			 WHERE environment=$1 AND competition_id=$2 AND season_id=$3`, lease.Environment, lease.CompetitionId, lease.SeasonId).Scan(&expiry))
		})
		if !expiry.Equal(lease.LeaseExpiresAt) {
			t.Fatal("diagnostic callback changed confirmed durable authority")
		}
	})
}
