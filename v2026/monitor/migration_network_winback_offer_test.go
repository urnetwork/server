// The win-back offer contract through the public migration signal: a code
// head ahead of the database is migration lag, never schema drift, and from
// the offers' version every published shape fault is drift it owns.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// A database one version behind the code reads the offers as absent and only
// warns that it is behind; once it reaches their version, absent offers page.
func TestMigrationsNetworkWinbackOfferPendingWarnsWithoutPaging(t *testing.T) {
	version := server.NetworkWinbackOfferSchemaMigrationVersion()
	if version == 0 {
		t.Fatal("the win-back offer migration is not in the list")
	}
	artifact := migrationCurrentSuffixTestContract(t, version).artifact
	if artifact.rowColumn != version-589 || artifact.removedVersion != 0 {
		t.Fatalf("win-back offer contract is %+v", artifact)
	}
	owner := fmt.Sprintf("%s@v%d", artifact.name, version)
	for _, test := range []struct {
		name           string
		head           int
		present, drift bool
	}{
		{name: "pending offers", head: version - 1},
		{name: "missing published offers", head: version, drift: true},
		{name: "published offers", head: version, present: true},
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

// The real schema before and at the offers' version, then rolled-back shape
// faults of the published table, each drift owned by the offers.
func TestMigrationsNetworkWinbackOfferPublishedSchemaAndFaults(t *testing.T) {
	version := server.NetworkWinbackOfferSchemaMigrationVersion()
	owner := fmt.Sprintf("%s@v%d", migrationCurrentSuffixTestContract(t, version).artifact.name, version)
	column := migrationCurrentSuffixTestContract(t, version).artifact.rowColumn
	faults := []struct{ name, sql string }{
		{"offers table absent", `DROP TABLE network_winback_offer`},
		{"offers not durable", `ALTER TABLE network_winback_offer SET UNLOGGED`},
		{"offer id not a uuid", `ALTER TABLE network_winback_offer ALTER COLUMN offer_id TYPE text`},
		{"store widened", `ALTER TABLE network_winback_offer ALTER COLUMN store TYPE varchar(17)`},
		{"expiry nullable", `ALTER TABLE network_winback_offer ALTER COLUMN expires_at DROP NOT NULL`},
		{"expiry zoned", `ALTER TABLE network_winback_offer ALTER COLUMN expires_at TYPE timestamp with time zone`},
		{"invented issue default", `ALTER TABLE network_winback_offer ALTER COLUMN issued_at SET DEFAULT now()`},
		{"redemption required", `ALTER TABLE network_winback_offer ALTER COLUMN redeemed_at SET NOT NULL`},
		{"promotion code narrowed", `ALTER TABLE network_winback_offer ALTER COLUMN stripe_promotion_code_id TYPE varchar(64)`},
		{"apple offer id dropped", `ALTER TABLE network_winback_offer DROP COLUMN apple_offer_id`},
		{"offers without their key", `ALTER TABLE network_winback_offer DROP CONSTRAINT network_winback_offer_pkey`},
		{"offers keyed by network", `ALTER TABLE network_winback_offer DROP CONSTRAINT network_winback_offer_pkey; ALTER TABLE network_winback_offer ADD PRIMARY KEY (network_id, offer_id)`},
		{"percent unbounded", `ALTER TABLE network_winback_offer DROP CONSTRAINT network_winback_offer_percent_off_check`},
		{"percent bound loosened", `ALTER TABLE network_winback_offer DROP CONSTRAINT network_winback_offer_percent_off_check; ALTER TABLE network_winback_offer ADD CHECK (0 <= percent_off AND percent_off <= 100)`},
		{"network index absent", `DROP INDEX network_winback_offer_network_issued`},
		{"network index without time", `DROP INDEX network_winback_offer_network_issued; CREATE INDEX network_winback_offer_network_issued ON network_winback_offer (network_id)`},
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
			t.Fatalf("rolled-back win-back offer faults left drift: %s", drift)
		}
	})
	pending, published := prefixes[version-1], prefixes[version]
	if pending.row == nil || published.row == nil {
		t.Fatal("a published prefix was not observed")
	}
	if pending.row[column] != "f" || published.row[column] != "t" {
		t.Fatalf("the offers read %q one version behind and %q at their version", pending.row[column], published.row[column])
	}
	verifyCohortCensusRequireAlerts(t, "pending offers", version-1, pending.alerts, false, owner)
	verifyCohortCensusRequireAlerts(t, "published offers", version, published.alerts, false, owner)
	if len(observations) != len(faults) {
		t.Fatalf("observed %d win-back offer faults, want %d", len(observations), len(faults))
	}
	for index, alerts := range observations {
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), owner) {
			t.Fatalf("%s lost its owning migration: %s", faults[index].name, alert.Markdown())
		}
	}
}
