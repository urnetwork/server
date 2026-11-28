// The verify cohort's rolling census contract through the public migration
// signal: a code head ahead of the database is migration lag, never schema
// drift, and from the migration's version every published shape fault is
// drift it owns.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// A database one version behind the code reads the rolling census as absent
// and only warns that it is behind; once the database reaches its version, an
// absent rolling census pages, and a present one raises nothing but lag.
func TestMigrationsVerifyCohortRollingPendingWarnsWithoutPaging(t *testing.T) {
	version := server.VerifyCohortRollingSchemaMigrationVersion()
	if version == 0 {
		t.Fatal("the rolling census migration is not in the migration list")
	}
	artifact := migrationCurrentSuffixTestContract(t, version).artifact
	if artifact.rowColumn != version-589 || artifact.removedVersion != 0 {
		t.Fatalf("rolling census contract is %+v", artifact)
	}
	owner := fmt.Sprintf("%s@v%d", artifact.name, version)
	for _, test := range []struct {
		name           string
		head           int
		present, drift bool
	}{
		{name: "pending rolling census", head: version - 1},
		{name: "missing published rolling census", head: version, drift: true},
		{name: "published rolling census", head: version, present: true},
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

// The real schema before and at the rolling census's version, then rolled-back
// shape faults of everything it publishes, each drift owned by the migration.
func TestMigrationsVerifyCohortRollingPublishedSchemaAndFaults(t *testing.T) {
	version := server.VerifyCohortRollingSchemaMigrationVersion()
	owner := fmt.Sprintf("%s@v%d", migrationCurrentSuffixTestContract(t, version).artifact.name, version)
	column := migrationCurrentSuffixTestContract(t, version).artifact.rowColumn
	faults := []struct{ name, sql string }{
		{"summary census absent", `ALTER TABLE sn_head_summary DROP COLUMN census_providers`},
		{"summary census narrowed", `ALTER TABLE sn_head_summary ALTER COLUMN census_providers TYPE integer`},
		{"summary bindings absent", `ALTER TABLE sn_head_summary DROP COLUMN bound_clients`},
		{"summary coverage required", `ALTER TABLE sn_head_summary ALTER COLUMN bound_clients_observed SET NOT NULL`},
		{"invented coverage default", `ALTER TABLE sn_head_summary ALTER COLUMN bound_clients_observed SET DEFAULT 0`},
		{"last epoch absent", `ALTER TABLE verify_cohort_census DROP COLUMN last_epoch`},
		{"last epoch narrowed", `ALTER TABLE verify_cohort_census ALTER COLUMN last_epoch TYPE integer`},
		{"state table absent", `DROP TABLE verify_cohort_state`},
		{"state not durable", `ALTER TABLE verify_cohort_state SET UNLOGGED`},
		{"state start nullable", `ALTER TABLE verify_cohort_state ALTER COLUMN rolling_from_epoch DROP NOT NULL`},
		{"state start zoned", `ALTER TABLE verify_cohort_state ALTER COLUMN rolling_since TYPE timestamp with time zone`},
		{"state without its key", `ALTER TABLE verify_cohort_state DROP CONSTRAINT verify_cohort_state_pkey`},
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
			t.Fatalf("rolled-back rolling census faults left drift: %s", drift)
		}
	})
	pending, published := prefixes[version-1], prefixes[version]
	if pending.row == nil || published.row == nil {
		t.Fatal("a published prefix was not observed")
	}
	if pending.row[column] != "f" || published.row[column] != "t" {
		t.Fatalf("the rolling census reads %q one version behind and %q at its version", pending.row[column], published.row[column])
	}
	verifyCohortCensusRequireAlerts(t, "pending rolling census", version-1, pending.alerts, false, owner)
	verifyCohortCensusRequireAlerts(t, "published rolling census", version, published.alerts, false, owner)
	if len(observations) != len(faults) {
		t.Fatalf("observed %d rolling census faults, want %d", len(observations), len(faults))
	}
	for index, alerts := range observations {
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), owner) {
			t.Fatalf("%s lost its owning migration: %s", faults[index].name, alert.Markdown())
		}
	}
}

// The contract pins every column and the state table's key it publishes, so
// a rolled-back shape of any of them is drift the rolling census owns.
func TestMigrationsVerifyCohortRollingContractNamesItsArtifacts(t *testing.T) {
	query := verifyCohortRollingMigrationContract.query
	for _, want := range []string{
		"sn_head_summary", "'census_providers'", "'bound_clients'", "'bound_clients_observed'",
		"verify_cohort_census", "'last_epoch'",
		"verify_cohort_state", "'deployment_key'", "'rolling_from_epoch'", "'rolling_since'", "PRIMARY KEY (deployment_key)",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("the rolling census contract does not pin %s", want)
		}
	}
}
