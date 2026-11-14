// The onboarding results' purchase.started count through the public migration
// signal: a code head ahead of the database is migration lag, never schema
// drift, and from the column's version every published shape fault is drift
// it owns.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// A database one version behind the code reads the column as absent and only
// warns that it is behind. Once the database reaches the column's version, an
// absent column pages. The alert check is the census's, which judges any
// artifact at a head.
func TestMigrationsOnboardingResultsPurchaseStartedPendingWarnsWithoutPaging(t *testing.T) {
	version := server.OnboardingResultsPurchaseStartedMigrationVersion()
	artifact := migrationCurrentSuffixTestContract(t, version).artifact
	if artifact.rowColumn != version-589 || artifact.removedVersion != 0 {
		t.Fatalf("purchase started contract is %+v", artifact)
	}
	owner := fmt.Sprintf("%s@v%d", artifact.name, version)
	for _, test := range []struct {
		name           string
		head           int
		present, drift bool
	}{
		{name: "pending column", head: version - 1},
		{name: "missing published column", head: version, drift: true},
		{name: "published column", head: version, present: true},
	} {
		row := syntheticMigrationArtifactRow(test.head)
		row[artifact.rowColumn] = "f"
		if test.present {
			row[artifact.rowColumn] = "t"
		}
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(test.head), nil
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		verifyCohortCensusRequireAlerts(t, test.name, test.head, alerts, test.drift, owner)
	}
}

// The real schema before and at the column's version, then rolled-back shape
// faults of the published column, each drift owned by the column's contract.
func TestMigrationsOnboardingResultsPurchaseStartedPublishedSchemaAndFaults(t *testing.T) {
	version := server.OnboardingResultsPurchaseStartedMigrationVersion()
	artifact := migrationCurrentSuffixTestContract(t, version).artifact
	owner := fmt.Sprintf("%s@v%d", artifact.name, version)
	faults := []struct{ name, sql string }{
		{"purchase started count absent", `ALTER TABLE onboarding_results_daily DROP COLUMN purchase_started_7d`},
		{"purchase started count nullable", `ALTER TABLE onboarding_results_daily ALTER COLUMN purchase_started_7d DROP NOT NULL`},
		{"purchase started count without its default", `ALTER TABLE onboarding_results_daily ALTER COLUMN purchase_started_7d DROP DEFAULT`},
		{"invented purchase started default", `ALTER TABLE onboarding_results_daily ALTER COLUMN purchase_started_7d SET DEFAULT 1`},
		{"purchase started count widened", `ALTER TABLE onboarding_results_daily ALTER COLUMN purchase_started_7d TYPE bigint`},
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
			t.Fatalf("rolled-back purchase started faults left drift: %s", drift)
		}
	})
	pending, published := prefixes[version-1], prefixes[version]
	if pending.row == nil || published.row == nil {
		t.Fatal("a published prefix was not observed")
	}
	if pending.row[artifact.rowColumn] != "f" || published.row[artifact.rowColumn] != "t" {
		t.Fatalf("the column reads %q one version behind and %q at its version", pending.row[artifact.rowColumn], published.row[artifact.rowColumn])
	}
	verifyCohortCensusRequireAlerts(t, "pending column", version-1, pending.alerts, false, owner)
	verifyCohortCensusRequireAlerts(t, "published column", version, published.alerts, false, owner)
	if len(observations) != len(faults) {
		t.Fatalf("observed %d purchase started faults, want %d", len(observations), len(faults))
	}
	for index, alerts := range observations {
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), owner) {
			t.Fatalf("%s lost its owning migration: %s", faults[index].name, alert.Markdown())
		}
	}
}
