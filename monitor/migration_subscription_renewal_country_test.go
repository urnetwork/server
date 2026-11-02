// The subscription store country contract through the public migration signal:
// a database behind the code reads the missing column as migration lag, never
// drift, and from the column's version a missing column is drift it owns.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func TestMigrationsSubscriptionRenewalCountryPublicationBoundary(t *testing.T) {
	version := server.SubscriptionRenewalCountrySchemaMigrationVersion()
	head := server.MigrationCount()
	if version < 1 || head < version {
		t.Fatalf("subscription store country migration is not published: version=%d head=%d", version, head)
	}
	artifact := migrationCurrentSuffixTestContract(t, version).artifact
	if artifact.rowColumn != version-589 || artifact.removedVersion != 0 {
		t.Fatalf("store country contract is %+v", artifact)
	}
	for _, test := range []struct {
		version int
		present bool
		class   string
	}{
		{version: version - 1, class: "migration-behind"},
		{version: head, present: true},
		{version: head, class: "migration-schema-drift"},
	} {
		row := syntheticMigrationArtifactRow(test.version)
		row[artifact.rowColumn] = "f"
		if test.present {
			row[artifact.rowColumn] = "t"
		}
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(test.version), nil
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if test.class == "" {
			if len(alerts) != 0 {
				t.Fatalf("a present store country column produced %d alerts", len(alerts))
			}
			continue
		}
		if len(alerts) != 1 {
			t.Fatalf("version %d produced %d alerts, want exactly one %s", test.version, len(alerts), test.class)
		}
		alert := requireAlertClass(t, alerts, test.class)
		if test.class == "migration-schema-drift" && !strings.Contains(alert.Markdown(), fmt.Sprintf("%s@v%d", artifact.name, artifact.requiredVersion)) {
			t.Fatal("published store country drift lost its owning artifact")
		}
	}
}

// The real schema before and at the column's version, then rolled-back shape
// faults of the published column, each drift owned by the store country.
func TestMigrationsSubscriptionRenewalCountryPublishedSchemaAndFaults(t *testing.T) {
	version := server.SubscriptionRenewalCountrySchemaMigrationVersion()
	artifact := migrationCurrentSuffixTestContract(t, version).artifact
	owner := fmt.Sprintf("%s@v%d", artifact.name, version)
	faults := []struct{ name, sql string }{
		{"store country absent", `ALTER TABLE subscription_renewal DROP COLUMN country`},
		{"store country widened", `ALTER TABLE subscription_renewal ALTER COLUMN country TYPE varchar(3)`},
		{"store country as text", `ALTER TABLE subscription_renewal ALTER COLUMN country TYPE text`},
		{"store country required", `ALTER TABLE subscription_renewal ALTER COLUMN country SET NOT NULL`},
		{"invented store country default", `ALTER TABLE subscription_renewal ALTER COLUMN country SET DEFAULT ''`},
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
			t.Fatalf("rolled-back store country faults left drift: %s", drift)
		}
	})
	pending, published := prefixes[version-1], prefixes[version]
	if pending.row == nil || published.row == nil {
		t.Fatal("a published prefix was not observed")
	}
	if pending.row[artifact.rowColumn] != "f" || published.row[artifact.rowColumn] != "t" {
		t.Fatalf("the store country reads %q one version behind and %q at its version", pending.row[artifact.rowColumn], published.row[artifact.rowColumn])
	}
	// behind or at its version the column is never drift; only the lag
	// warning while the database is behind the source
	for _, head := range []int{version - 1, version} {
		wantLag := 0
		if head < server.MigrationCount() {
			wantLag = 1
		}
		alerts := prefixes[head].alerts
		if len(alerts) != wantLag || (wantLag == 1 && alerts[0].Class != "migration-behind") {
			t.Fatalf("at head %d: %d alerts, want %d migration-behind", head, len(alerts), wantLag)
		}
	}
	if len(observations) != len(faults) {
		t.Fatalf("observed %d store country faults, want %d", len(observations), len(faults))
	}
	for index, alerts := range observations {
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), owner) {
			t.Fatalf("%s lost its owning migration: %s", faults[index].name, alert.Markdown())
		}
	}
}
