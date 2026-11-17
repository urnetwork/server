// The deleted-account record's three contracts through the public migration
// signal: a code head ahead of the database is migration lag, never schema
// drift, and from each migration's version every published shape fault is
// drift it owns.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func deletedNetworkTestVersions(t *testing.T) (deleted, move, backfill int) {
	t.Helper()
	deleted, move, backfill = server.DeletedNetworkSchemaMigrationVersion(), server.AppleSubscriptionMoveSchemaMigrationVersion(), server.DeletedNetworkBackfillMigrationVersion()
	if deleted == 0 || move != deleted+1 || backfill != move+1 {
		t.Fatalf("the deleted-account migrations are at %d, %d and %d, want three consecutive versions", deleted, move, backfill)
	}
	return
}

func deletedNetworkTestOwner(t *testing.T, version int) string {
	t.Helper()
	return fmt.Sprintf("%s@v%d", migrationCurrentSuffixTestContract(t, version).artifact.name, version)
}

// Each contract pins every column and key it publishes, so a rolled-back shape
// of any of them is drift its migration owns.
func TestMigrationsDeletedNetworkContractsNameTheirArtifacts(t *testing.T) {
	for _, test := range []struct {
		contract snMainnetMigrationContract
		want     []string
	}{
		{deletedNetworkMigrationContract, []string{"deleted_network", "'network_id'", "'delete_time'", "'source'", "'character varying(32)'", "PRIMARY KEY (network_id)"}},
		{appleSubscriptionMoveMigrationContract, []string{"apple_subscription_move", "'original_transaction_id'", "'from_network_id'", "'to_network_id'", "'transaction_id'", "'move_time'", "PRIMARY KEY (original_transaction_id)"}},
		{deletedNetworkBackfillMigrationContract, []string{"deleted_network", "'source'", "PRIMARY KEY (network_id)"}},
	} {
		for _, want := range test.want {
			if !strings.Contains(test.contract.query, want) {
				t.Fatalf("the %s contract does not pin %s", test.contract.artifact.name, want)
			}
		}
	}
}

// A database one version behind a contract reads it as absent and only warns
// that it is behind; at its version an absent artifact pages, and a present
// one raises nothing but lag.
func TestMigrationsDeletedNetworkPendingWarnsWithoutPaging(t *testing.T) {
	deleted, move, backfill := deletedNetworkTestVersions(t)
	for _, version := range []int{deleted, move, backfill} {
		artifact := migrationCurrentSuffixTestContract(t, version).artifact
		if artifact.rowColumn != version-589 || artifact.removedVersion != 0 {
			t.Fatalf("contract at %d is %+v", version, artifact)
		}
		owner := deletedNetworkTestOwner(t, version)
		for _, test := range []struct {
			name           string
			head           int
			present, drift bool
		}{
			{name: fmt.Sprintf("pending %s", artifact.name), head: version - 1},
			{name: fmt.Sprintf("missing published %s", artifact.name), head: version, drift: true},
			{name: fmt.Sprintf("published %s", artifact.name), head: version, present: true},
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
}

// The real schema before and at each version, then rolled-back shape faults of
// both tables, each drift owned by the migration that published the shape.
func TestMigrationsDeletedNetworkPublishedSchemaAndFaults(t *testing.T) {
	deleted, move, backfill := deletedNetworkTestVersions(t)
	deletedOwner, moveOwner, backfillOwner := deletedNetworkTestOwner(t, deleted), deletedNetworkTestOwner(t, move), deletedNetworkTestOwner(t, backfill)
	faults := []struct {
		name   string
		sql    string
		owners []string
	}{
		{"deleted accounts absent", `DROP TABLE deleted_network`, []string{deletedOwner, backfillOwner}},
		{"deleted accounts not durable", `ALTER TABLE deleted_network SET UNLOGGED`, []string{deletedOwner, backfillOwner}},
		{"deleted account time nullable", `ALTER TABLE deleted_network ALTER COLUMN delete_time DROP NOT NULL`, []string{deletedOwner}},
		{"deleted account time zoned", `ALTER TABLE deleted_network ALTER COLUMN delete_time TYPE timestamp with time zone`, []string{deletedOwner}},
		{"deletion source widened", `ALTER TABLE deleted_network ALTER COLUMN source TYPE varchar(64)`, []string{deletedOwner, backfillOwner}},
		{"deletion source defaulted", `ALTER TABLE deleted_network ALTER COLUMN source SET DEFAULT 'backfill'`, []string{deletedOwner, backfillOwner}},
		{"deleted accounts without their key", `ALTER TABLE deleted_network DROP CONSTRAINT deleted_network_pkey`, []string{deletedOwner, backfillOwner}},
		{"moves absent", `DROP TABLE apple_subscription_move`, []string{moveOwner}},
		{"moves not durable", `ALTER TABLE apple_subscription_move SET UNLOGGED`, []string{moveOwner}},
		{"move target nullable", `ALTER TABLE apple_subscription_move ALTER COLUMN to_network_id DROP NOT NULL`, []string{moveOwner}},
		{"move transaction narrowed", `ALTER TABLE apple_subscription_move ALTER COLUMN transaction_id TYPE varchar(64)`, []string{moveOwner}},
		{"moves without their key", `ALTER TABLE apple_subscription_move DROP CONSTRAINT apple_subscription_move_pkey`, []string{moveOwner}},
	}
	rows := map[int]Row{}
	alertsAt := map[int][]Alert{}
	var observations [][]Alert
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{deleted - 1, deleted, move, backfill} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			source := &migrationPingDatabaseSource{}
			alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			rows[head], alertsAt[head] = source.artifactRow, alerts
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
			t.Fatalf("rolled-back deleted-account faults left drift: %s", drift)
		}
	})
	column := func(version int) int { return migrationCurrentSuffixTestContract(t, version).artifact.rowColumn }
	for _, head := range []int{deleted - 1, deleted, move, backfill} {
		if rows[head] == nil {
			t.Fatalf("the schema at %d was not observed", head)
		}
	}
	if rows[deleted-1][column(deleted)] != "f" || rows[deleted][column(deleted)] != "t" {
		t.Fatalf("the record of deleted accounts reads %q one version behind and %q at its version", rows[deleted-1][column(deleted)], rows[deleted][column(deleted)])
	}
	if rows[deleted][column(move)] != "f" || rows[move][column(move)] != "t" {
		t.Fatalf("the moves read %q one version behind and %q at their version", rows[deleted][column(move)], rows[move][column(move)])
	}
	// the backfill writes into the table 992 published, so it reads present
	// from 992 on; at its own version it must
	if rows[backfill][column(backfill)] != "t" {
		t.Fatalf("the backfilled record reads %q at its version", rows[backfill][column(backfill)])
	}
	verifyCohortCensusRequireAlerts(t, "pending record of deleted accounts", deleted-1, alertsAt[deleted-1], false, deletedOwner)
	verifyCohortCensusRequireAlerts(t, "published record of deleted accounts", deleted, alertsAt[deleted], false, deletedOwner)
	verifyCohortCensusRequireAlerts(t, "published moves", move, alertsAt[move], false, moveOwner)
	verifyCohortCensusRequireAlerts(t, "published backfill", backfill, alertsAt[backfill], false, backfillOwner)
	if len(observations) != len(faults) {
		t.Fatalf("observed %d deleted-account faults, want %d", len(observations), len(faults))
	}
	for index, alerts := range observations {
		alert := requireAlertClass(t, alerts, "migration-schema-drift")
		if alert.Severity != SeverityPage {
			t.Fatalf("%s does not page", faults[index].name)
		}
		for _, owner := range faults[index].owners {
			if !strings.Contains(alert.Markdown(), owner) {
				t.Fatalf("%s lost its owning migration %s: %s", faults[index].name, owner, alert.Markdown())
			}
		}
	}
}
