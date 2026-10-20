// Keeps the complete upstream migration prefix immutable when signed client-key
// history is appended, and refuses the conflicting pre-promotion draft catalog.
package server

import (
	"context"
	"strings"
	"testing"
)

const clientKeyHistoryUpstreamMigrationCount = 650

// The independent identity covers every upstream entry, including the
// onboarding schema already applied through production version 650; the older
// published-prefix guard stops at entry 592.
func TestStClientKeyHistoryMigrationKeepsUpstreamPrefix(t *testing.T) {
	if len(migrations) <= clientKeyHistoryUpstreamMigrationCount {
		t.Fatal("signed client-key history did not follow the upstream migration prefix")
	}
	identity, err := migrationPrefixIdentity(clientKeyHistoryUpstreamMigrationCount)
	if err != nil {
		t.Fatal(err)
	}
	const expectedIdentity = "2994366d85787a78439782833ffe2a82017b4436be3598a72138131c478960ce"
	if identity != expectedIdentity {
		t.Fatalf("upstream migration prefix identity = %s, want %s", identity, expectedIdentity)
	}
}

// These fixed indices prevent an independently developed migration from being
// inserted ahead of the onboarding schema already recorded by Main.
func TestStClientKeyHistoryMigrationFollowsPublishedOnboardingSchema(t *testing.T) {
	expected := []struct {
		marker string
		index  int
	}{
		{marker: "transfer_contract_unresolved_source_pair_create_time", index: 631},
		{marker: "transfer_contract_unresolved_destination_pair_create_time", index: 632},
		{marker: "transfer_contract_unresolved_payer_transfer_byte_count", index: 633},
		{marker: "ALTER COLUMN open SET STATISTICS 300", index: 634},
		{marker: "CREATE TABLE network_onboarding_offer (", index: 635},
		{marker: "CREATE INDEX network_onboarding_created_at", index: 649},
		{marker: "CREATE TABLE st_client_key_history (", index: clientKeyHistoryUpstreamMigrationCount},
		{marker: "DROP TRIGGER competition_staging_finalization_blocked", index: 651},
		{marker: "ADD COLUMN admission_closed_at timestamp NULL", index: 652},
		{marker: "ADD COLUMN epoch_metrics_available boolean", index: 653},
	}
	for _, entry := range expected {
		if index := migrationIndex(t, entry.marker); index != entry.index {
			t.Fatalf("migration %q index = %d, want %d", entry.marker, index, entry.index)
		}
	}
	migration, ok := migrations[clientKeyHistoryUpstreamMigrationCount].(*SqlMigration)
	if !ok || migration.sql != clientKeyHistorySchemaSQL {
		t.Fatal("appended client-key migration differs from its complete signed-history schema")
	}
	count := 0
	for _, candidate := range migrations {
		if sql, ok := candidate.(*SqlMigration); ok && strings.Contains(sql.sql, "CREATE TABLE st_client_key_history (") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("signed client-key schema appears %d times, want one append", count)
	}
}

// A retained draft database applied client-key history at index 631. It cannot
// be relabeled as the new upstream planner migration or silently skipped.
func TestStClientKeyHistoryMigrationRefusesOldDraftCatalog(t *testing.T) {
	if len(migrations) <= clientKeyHistoryUpstreamMigrationCount {
		t.Fatal("signed client-key migration is missing")
	}
	const oldDraftVersion = 632
	entries := testMigrationCatalogEntries(t, oldDraftVersion)
	identity, err := migrationIdentity(migrations[clientKeyHistoryUpstreamMigrationCount])
	if err != nil {
		t.Fatal(err)
	}
	entries[oldDraftVersion-1].Identity = identity
	err = validateMigrationCatalog(oldDraftVersion, entries)
	if err == nil || !strings.Contains(err.Error(), "migration 631 identity differs from durable catalog") {
		t.Fatalf("conflicting retained draft catalog was not refused at its original index: %v", err)
	}
}

// Main had durably reached version 650 with the onboarding schema before the
// independently developed client-key and staging branches were combined. That
// exact head must remain reproducible: client-key history is pending at 650 and
// the ordinary runner installs it, its triggers, and every later append.
func TestMainVersion650MigratesThroughClientKeyAppend(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		ApplyDbMigrationsUpTo(ctx, clientKeyHistoryUpstreamMigrationCount)

		relationExists := func(name string) bool {
			var exists bool
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+name).Scan(&exists))
			})
			return exists
		}
		columnExists := func(table string, column string) bool {
			var exists bool
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `
					SELECT EXISTS (
						SELECT 1 FROM information_schema.columns
						WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
					)
				`, table, column).Scan(&exists))
			})
			return exists
		}

		if version := DbVersion(ctx); version != clientKeyHistoryUpstreamMigrationCount {
			t.Fatalf("pre-append database version = %d, want %d", version, clientKeyHistoryUpstreamMigrationCount)
		}
		if !relationExists("network_onboarding_created_at") {
			t.Fatal("version 650 is missing its final published onboarding index")
		}
		if relationExists("st_client_key_history") || relationExists("st_client_key_head") {
			t.Fatal("client-key history was inserted ahead of published version 650")
		}
		if columnExists("competition_round", "admission_closed_at") || columnExists("network_points_leaderboard_snapshot", "epoch_metrics_available") {
			t.Fatal("a post-650 column was inserted into the published prefix")
		}

		ApplyDbMigrations(ctx)
		if version := DbVersion(ctx); version != MigrationCount() {
			t.Fatalf("post-append database version = %d, want %d", version, MigrationCount())
		}
		if !relationExists("st_client_key_history") || !relationExists("st_client_key_head") {
			t.Fatal("ordinary migration did not install both client-key history tables")
		}
		if !columnExists("competition_round", "admission_closed_at") || !columnExists("network_points_leaderboard_snapshot", "epoch_metrics_available") {
			t.Fatal("ordinary migration did not install the post-650 columns")
		}

		MaintenanceDb(ctx, func(conn PgConn) {
			var triggerCount int
			Raise(conn.QueryRow(ctx, `
				SELECT count(*)
				FROM pg_trigger AS trigger_record
				JOIN pg_class AS relation ON relation.oid = trigger_record.tgrelid
				JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'public' AND NOT trigger_record.tgisinternal
				  AND (relation.relname, trigger_record.tgname) IN (
					('st_client_key_history', 'st_client_key_history_immutable'),
					('st_client_key_head', 'st_client_key_head_identity'),
					('network_client', 'st_client_key_retire_on_client_delete')
				  )
			`).Scan(&triggerCount))
			if triggerCount != 3 {
				t.Fatalf("client-key append installed %d required triggers, want 3", triggerCount)
			}
		})
	})
}
