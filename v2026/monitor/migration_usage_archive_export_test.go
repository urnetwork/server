// The usage archive export ledger's contract through the public migration
// signal, against the real schema: absent one version behind it, present at
// its version, and each published shape fault drift the ledger owns.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestMigrationsUsageArchiveExportLedgerPublishedSchemaAndFaults(t *testing.T) {
	version := server.ProviderUsageArchiveExportLedgerMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 {
		t.Fatalf("the export ledger's contract is %+v", contract.artifact)
	}
	owner := fmt.Sprintf("%s@v%d", contract.artifact.name, version)
	column := contract.artifact.rowColumn
	faults := []struct{ name, sql string }{
		{"ledger absent", `DROP TABLE st_provider_usage_archive_export`},
		{"ledger not durable", `ALTER TABLE st_provider_usage_archive_export SET UNLOGGED`},
		{"row count nullable", `ALTER TABLE st_provider_usage_archive_export ALTER COLUMN row_count DROP NOT NULL`},
		{"digest narrowed", `ALTER TABLE st_provider_usage_archive_export ALTER COLUMN row_digest TYPE bigint`},
		{"invented create default", `ALTER TABLE st_provider_usage_archive_export ALTER COLUMN create_time SET DEFAULT now()`},
		{"drop time dropped", `ALTER TABLE st_provider_usage_archive_export DROP COLUMN dropped_time`},
		{"file hash not unique", `ALTER TABLE st_provider_usage_archive_export DROP CONSTRAINT st_provider_usage_archive_export_file_sha256_key`},
		{"leaf index absent", `DROP INDEX st_provider_usage_archive_export_leaf`},
	}
	type prefix struct {
		row    Row
		alerts []Alert
	}
	prefixes := map[int]prefix{}
	var observations [][]Alert
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{version - 1, version} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			source := &migrationPingDatabaseSource{}
			alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			prefixes[head] = prefix{row: source.artifactRow, alerts: alerts}
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				observations = append(observations, alerts)
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back export ledger faults left drift: %s", drift)
		}
	})
	pending, published := prefixes[version-1], prefixes[version]
	if pending.row == nil || published.row == nil {
		t.Fatal("a published prefix was not observed")
	}
	if pending.row[column] != "f" || published.row[column] != "t" {
		t.Fatalf("the ledger reads %q one version behind and %q at its version", pending.row[column], published.row[column])
	}
	verifyCohortCensusRequireAlerts(t, "pending ledger", version-1, pending.alerts, false, owner)
	verifyCohortCensusRequireAlerts(t, "published ledger", version, published.alerts, false, owner)
	if len(observations) != len(faults) {
		t.Fatalf("observed %d export ledger faults, want %d", len(observations), len(faults))
	}
	for index, alerts := range observations {
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), owner) {
			t.Fatalf("%s lost its owning migration: %s", faults[index].name, alert.Markdown())
		}
	}
}
