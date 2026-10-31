// The verify cohort census contract through the public migration signal: a
// code head ahead of the database is migration lag, never schema drift, and
// from the census's version every published shape fault is drift it owns.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// A database one version behind the code reads the census as absent and only
// warns that it is behind; that warning is never promoted to a page. Once the
// database reaches the census's version, an absent census pages. A later
// migration in the source leaves the database behind again, so the lag
// warning is the only other alert either side may carry.
func TestMigrationsVerifyCohortCensusPendingWarnsWithoutPaging(t *testing.T) {
	version := server.VerifyCohortCensusSchemaMigrationVersion()
	artifact := migrationCurrentSuffixTestContract(t, version).artifact
	if artifact.rowColumn != version-589 || artifact.removedVersion != 0 {
		t.Fatalf("census contract is %+v", artifact)
	}
	owner := fmt.Sprintf("%s@v%d", artifact.name, version)
	for _, test := range []struct {
		name           string
		head           int
		present, drift bool
	}{
		{name: "pending census", head: version - 1},
		{name: "missing published census", head: version, drift: true},
		{name: "published census", head: version, present: true},
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

// At a database head, the drift page appears exactly when expected and names
// the census, and the only other alert is the lag warning while the database
// is behind the source, never promoted to a page.
func verifyCohortCensusRequireAlerts(t *testing.T, name string, head int, alerts []Alert, drift bool, owner string) {
	t.Helper()
	lag, pages := 0, 0
	for _, alert := range alerts {
		switch alert.Class {
		case "migration-behind":
			if alert.Severity != SeverityWarn || alert.PageSustain != 0 {
				t.Fatalf("%s: the lag alert is severity %v with page sustain %d, want a warning that is never promoted", name, alert.Severity, alert.PageSustain)
			}
			lag++
		case "migration-schema-drift":
			if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, owner) {
				t.Fatalf("%s: drift does not page with its owning artifact: %s", name, alert.Observed)
			}
			pages++
		default:
			t.Fatalf("%s: unexpected %s alert", name, alert.Class)
		}
	}
	wantLag := 0
	if head < server.MigrationCount() {
		wantLag = 1
	}
	if lag != wantLag || (pages == 1) != drift || 1 < pages {
		t.Fatalf("%s at head %d: %d lag and %d drift alerts, want %d and drift=%t", name, head, lag, pages, wantLag, drift)
	}
}

// The real schema before and at the census's version, then rolled-back shape
// faults of the published table, each drift owned by the census.
func TestMigrationsVerifyCohortCensusPublishedSchemaAndFaults(t *testing.T) {
	version := server.VerifyCohortCensusSchemaMigrationVersion()
	owner := fmt.Sprintf("%s@v%d", migrationCurrentSuffixTestContract(t, version).artifact.name, version)
	column := migrationCurrentSuffixTestContract(t, version).artifact.rowColumn
	faults := []struct{ name, sql string }{
		{"census table absent", `DROP TABLE verify_cohort_census`},
		{"census not durable", `ALTER TABLE verify_cohort_census SET UNLOGGED`},
		{"deployment key widened", `ALTER TABLE verify_cohort_census ALTER COLUMN deployment_key TYPE varchar(97)`},
		{"client id not a uuid", `ALTER TABLE verify_cohort_census ALTER COLUMN client_id TYPE text`},
		{"admission time zoned", `ALTER TABLE verify_cohort_census ALTER COLUMN admitted_at TYPE timestamp with time zone`},
		{"admission time nullable", `ALTER TABLE verify_cohort_census ALTER COLUMN admitted_at DROP NOT NULL`},
		{"invented admission default", `ALTER TABLE verify_cohort_census ALTER COLUMN admitted_at SET DEFAULT now()`},
		{"census without its key", `ALTER TABLE verify_cohort_census DROP CONSTRAINT verify_cohort_census_pkey`},
		{"census keyed by client alone", `ALTER TABLE verify_cohort_census DROP CONSTRAINT verify_cohort_census_pkey; ALTER TABLE verify_cohort_census ADD PRIMARY KEY (client_id)`},
		{"census key deferrable", `ALTER TABLE verify_cohort_census DROP CONSTRAINT verify_cohort_census_pkey; ALTER TABLE verify_cohort_census ADD PRIMARY KEY (deployment_key, client_id) DEFERRABLE`},
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
			t.Fatalf("rolled-back census faults left drift: %s", drift)
		}
	})
	pending, published := prefixes[version-1], prefixes[version]
	if pending.row == nil || published.row == nil {
		t.Fatal("a published prefix was not observed")
	}
	if pending.row[column] != "f" || published.row[column] != "t" {
		t.Fatalf("the census reads %q one version behind and %q at its version", pending.row[column], published.row[column])
	}
	verifyCohortCensusRequireAlerts(t, "pending census", version-1, pending.alerts, false, owner)
	verifyCohortCensusRequireAlerts(t, "published census", version, published.alerts, false, owner)
	if len(observations) != len(faults) {
		t.Fatalf("observed %d census faults, want %d", len(observations), len(faults))
	}
	for index, alerts := range observations {
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), owner) {
			t.Fatalf("%s lost its owning migration: %s", faults[index].name, alert.Markdown())
		}
	}
}
