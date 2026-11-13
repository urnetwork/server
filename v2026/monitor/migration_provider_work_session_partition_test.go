// The session journal's custody on every layout the owner-run cutover gives
// it (server.MigrateProviderWorkSessionEventsToPartitions): plain, prepared
// and partitioned, and the faults of the partitioned layout that must reach
// the contract of the journal's partition preparation.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The contract of the journal's current custody, at its computed version:
// the horizon drop guard's, which republishes the partition preparation's.
func migrationProviderWorkSessionPartitionTestContract(t testing.TB) snMainnetMigrationContract {
	t.Helper()
	return migrationCurrentSuffixTestContract(t, server.ProviderWorkSessionEventHorizonDropGuardMigrationVersion())
}

// The partition preparation's contract ends at the horizon drop guard, whose
// contract republishes the same custody with only the drop guard's body
// replaced, by the 35-day body, and is not superseded.
func TestMigrationsProviderWorkSessionHorizonDropGuardContract(t *testing.T) {
	version := server.ProviderWorkSessionEventHorizonDropGuardMigrationVersion()
	preparation := server.ProviderWorkSessionPartitionMigrationVersion()
	if version == 0 || version <= preparation {
		t.Fatalf("horizon drop guard version %d, partition preparation version %d", version, preparation)
	}
	if contract := migrationCurrentSuffixTestContract(t, preparation); contract.artifact.removedVersion != version {
		t.Fatalf("the partition preparation's contract is removed at %d, want %d", contract.artifact.removedVersion, version)
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.removedVersion != 0 || contract.artifact.rowColumn != version-589 {
		t.Fatalf("horizon drop guard contract %+v", contract.artifact)
	}
	_, _, _, dropGuard := server.ProviderWorkSessionPartitionBodiesSql()
	horizonGuard := server.ProviderWorkSessionEventHorizonDropGuardBodySql()
	preparationQuery := migrationCurrentSuffixTestContract(t, preparation).query
	if !strings.Contains(contract.query, snMainnetMigrationLiteral(horizonGuard)) || strings.Contains(contract.query, snMainnetMigrationLiteral(dropGuard)) {
		t.Fatal("the horizon contract does not pin the horizon drop guard's body")
	}
	if strings.ReplaceAll(contract.query, snMainnetMigrationLiteral(horizonGuard), snMainnetMigrationLiteral(dropGuard)) != preparationQuery {
		t.Fatal("the horizon contract differs from the partition preparation's beyond the drop guard's body")
	}
	if strings.Replace(dropGuard, "interval '91 days'", "interval '35 days'", 1) != strings.Replace(horizonGuard, "kept at least 35 days", "kept 13 weeks", 1) {
		t.Fatal("the horizon drop guard is not the drop guard with only its age and message changed")
	}
}

// The custody holds and nothing drifts while the journal is plain, prepared
// and partitioned; on the partitioned layout a guard missing or disabled on
// one week's partition, a parent index whose partitions lack their copies and
// a dropped receipt reference each reach the contract.
func TestMigrationsProviderWorkSessionPartitionedCatalog(t *testing.T) {
	var observed []snMainnetMigrationAlertObservation
	faults := 0
	migrationContractTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrations(ctx)
		contract := migrationProviderWorkSessionPartitionTestContract(t)
		requireCoherent := func(layout string) {
			t.Helper()
			row, drift := migrationPingDatabaseCheck(t, ctx)
			if drift != "" {
				t.Fatalf("the %s journal reports drift: %s", layout, drift)
			}
			if !migrationBool(row[contract.artifact.rowColumn]) {
				t.Fatalf("%s@v%d reads absent on the %s journal", contract.artifact.name, contract.artifact.requiredVersion, layout)
			}
		}
		requireCoherent("plain")

		currentWeek := server.PartitionGranularityWeekly.RangeStart(server.NowUtc())
		cutoverWeek := server.PartitionGranularityWeekly.AddRanges(currentWeek, 2)
		logf := func(format string, args ...any) { t.Logf(format, args...) }
		if err := server.MigrateProviderWorkSessionEventsToPartitions(ctx, server.ProviderWorkSessionPartitionMigrateOptions{CutoverWeek: cutoverWeek, Logf: logf}); err != nil {
			t.Fatal(err)
		}
		requireCoherent("prepared")
		if err := server.MigrateProviderWorkSessionEventsToPartitions(ctx, server.ProviderWorkSessionPartitionMigrateOptions{Finalize: true, Logf: logf}); err != nil {
			t.Fatal(err)
		}
		requireCoherent("partitioned")

		week := fmt.Sprintf("provider_work_session_event_w%s", cutoverWeek.Format("20060102"))
		observed, faults = nil, 0
		for _, fault := range []struct {
			name string
			sql  string
		}{
			{"a week's partition without its TRUNCATE guard", fmt.Sprintf(`DROP TRIGGER original_truncate_guard ON %s`, week)},
			{"the row guard disabled on a week's partition", fmt.Sprintf(`ALTER TABLE %s DISABLE TRIGGER original_guard`, week)},
			{"the row guard disabled on the legacy partition", `ALTER TABLE provider_work_session_event_legacy DISABLE TRIGGER original_guard`},
			{"the parent's row guard dropped", `DROP TRIGGER original_guard ON provider_work_session_event`},
			{"a parent index whose partitions lack their copies", `DROP INDEX provider_work_session_event_connection;
				CREATE INDEX provider_work_session_event_connection ON ONLY provider_work_session_event (client_id, connection_id, sequence)`},
			{"the receipt reference dropped", `DROP TRIGGER provider_work_session_receipt_reference ON provider_work_session_receipt`},
		} {
			faults += 1
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatal(err)
				}
				observed = append(observed, snMainnetMigrationAlertObservation{
					alerts: alerts, want: fmt.Sprintf("%s@v%d", contract.artifact.name, contract.artifact.requiredVersion),
				})
			})
		}
		if err := server.MigrateProviderWorkSessionEventsToPartitions(ctx, server.ProviderWorkSessionPartitionMigrateOptions{Rollback: true, Logf: logf}); err != nil {
			t.Fatal(err)
		}
		requireCoherent("rolled back")
	})
	if faults == 0 || len(observed) != faults {
		t.Fatalf("observed %d of %d partitioned layout faults", len(observed), faults)
	}
	for index, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("fault %d lost its owning migration: %s", index, alert.Markdown())
		}
	}
}
