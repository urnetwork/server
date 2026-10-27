// The legacy settlement queue retires behind an exact refusal: its outcome
// guard, function and contract key go only once no intent remains.
package server

import (
	"context"
	"strings"
	"testing"
)

// The retirement's slice index, found by its identity so a merge that orders
// branches differently needs no renumbering.
func legacySettlementIntentRetirementIndex(t testing.TB) int {
	t.Helper()
	version := LegacySettlementIntentRetirementMigrationVersion()
	if version == 0 {
		t.Fatal("the legacy settlement retirement migration is missing")
	}
	return version - 1
}

// The index of the one plain SQL migration with exactly this text.
func legacySettlementIntentMigrationIndex(t testing.TB, sql string) int {
	t.Helper()
	found := -1
	for index, migration := range migrations {
		if candidate, ok := migration.(*SqlMigration); ok && candidate.sql == sql {
			if 0 <= found {
				t.Fatal("a legacy settlement migration is listed twice")
			}
			found = index
		}
	}
	if found < 0 {
		t.Fatal("a legacy settlement migration is missing")
	}
	return found
}

// The retirement is listed once, after every migration that created what it
// drops or what it keeps.
func TestLegacySettlementIntentRetirementFollowsTheQueueSchema(t *testing.T) {
	retirement := legacySettlementIntentMigrationIndex(t, legacySettlementIntentRetirementSql)
	if legacySettlementIntentRetirementIndex(t) != retirement {
		t.Fatal("the retirement version lookup disagrees with the migration list")
	}
	for _, sql := range []string{legacySettlementIntentSchemaSql, legacySettlementPayerSchemaSql, ContractCloseOwnerSchemaSql} {
		if retirement <= legacySettlementIntentMigrationIndex(t, sql) {
			t.Fatal("the retirement precedes the legacy settlement schema it retires")
		}
	}
	for _, statement := range []string{
		"LOCK TABLE transfer_contract IN ACCESS EXCLUSIVE MODE;\n\tLOCK TABLE legacy_settlement_intent IN ACCESS EXCLUSIVE MODE;",
		"DROP TRIGGER IF EXISTS transfer_contract_legacy_settlement_owner ON transfer_contract;",
		"DROP FUNCTION IF EXISTS guard_legacy_settlement_intent_outcome();",
		"DROP CONSTRAINT IF EXISTS legacy_settlement_intent_contract_id_fkey;",
	} {
		if !strings.Contains(legacySettlementIntentRetirementSql, statement) {
			t.Fatal("the retirement lost a required statement", statement)
		}
	}
	if strings.Contains(legacySettlementIntentRetirementSql, "DROP TABLE") {
		t.Fatal("the retirement drops the table the previous release still reads")
	}
}

// The catalog state the retirement changes, read on a direct session.
type legacySettlementIntentCatalog struct {
	table, guard, function, key bool
}

func readLegacySettlementIntentCatalog(ctx context.Context) (catalog legacySettlementIntentCatalog) {
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `SELECT
			to_regclass('public.legacy_settlement_intent') IS NOT NULL,
			EXISTS(SELECT 1 FROM pg_trigger WHERE tgname='transfer_contract_legacy_settlement_owner' AND NOT tgisinternal),
			to_regprocedure('public.guard_legacy_settlement_intent_outcome()') IS NOT NULL,
			EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public.legacy_settlement_intent') AND contype='f')`).Scan(
			&catalog.table, &catalog.guard, &catalog.function, &catalog.key))
	})
	return
}

// The retirement refuses while an intent remains, without advancing the head
// or dropping anything. Once the table is empty it applies, keeps the table,
// and an outcome update succeeds even beside a leftover intent row.
func TestLegacySettlementIntentRetirementRefusesPendingIntents(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		index := legacySettlementIntentRetirementIndex(t)
		ApplyDbMigrationsUpTo(ctx, index)
		if version := DbVersion(ctx); version != index {
			t.Fatalf("pre-retirement database version = %d, want %d", version, index)
		}
		if catalog := readLegacySettlementIntentCatalog(ctx); catalog != (legacySettlementIntentCatalog{table: true, guard: true, function: true, key: true}) {
			t.Fatalf("the published queue schema is incomplete before retirement: %+v", catalog)
		}
		contractId := NewId()
		insertIntent := func() {
			MaintenanceDb(ctx, func(conn PgConn) {
				RaisePgResult(conn.Exec(ctx, `INSERT INTO legacy_settlement_intent (contract_id, shard, outcome)
					VALUES ($1, get_byte(uuid_send($1), 15) % 16, 'settled')`, contractId))
			})
		}
		settle := func() (err error) {
			HandleError(func() {
				MaintenanceDb(ctx, func(conn PgConn) {
					RaisePgResult(conn.Exec(ctx, `UPDATE transfer_contract SET outcome='settled',close_time=$2,provider_usage=$3
						WHERE contract_id=$1`, contractId, NowUtc(), contractUsageGuardTestSnapshot(t)))
				})
			}, func(recovered error) { err = recovered })
			return
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(insertContractUsageGuardTestRow(ctx, conn, contractId, nil, nil, nil))
		})
		insertIntent()
		// Control: before the retirement the guard refuses this outcome.
		if err := settle(); err == nil || !strings.Contains(err.Error(), "pending legacy settlement owner") {
			t.Fatal("the outcome guard was not live before the retirement", err)
		}

		var refusal error
		HandleError(func() { ApplyDbMigrationsUpTo(ctx, index+1) }, func(err error) { refusal = err })
		if refusal == nil || !strings.Contains(refusal.Error(), "legacy_settlement_intent still holds pending legacy settlements") {
			t.Fatal("the retirement applied over a pending intent", refusal)
		}
		if version := DbVersion(ctx); version != index {
			t.Fatalf("the refused retirement advanced the head to %d", version)
		}
		if catalog := readLegacySettlementIntentCatalog(ctx); catalog != (legacySettlementIntentCatalog{table: true, guard: true, function: true, key: true}) {
			t.Fatalf("the refused retirement changed the catalog: %+v", catalog)
		}

		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `DELETE FROM legacy_settlement_intent WHERE contract_id=$1`, contractId))
		})
		ApplyDbMigrationsUpTo(ctx, index+1)
		if version := DbVersion(ctx); version != index+1 {
			t.Fatalf("the retirement over an empty table reached version %d, want %d", version, index+1)
		}
		if catalog := readLegacySettlementIntentCatalog(ctx); catalog != (legacySettlementIntentCatalog{table: true}) {
			t.Fatalf("the retirement left a guard, function or key, or dropped the table: %+v", catalog)
		}

		// A leftover row of an older release no longer refuses an outcome.
		insertIntent()
		if err := settle(); err != nil {
			t.Fatal("an outcome update failed without the guard", err)
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			var outcome *string
			Raise(conn.QueryRow(ctx, `SELECT outcome FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&outcome))
			if outcome == nil || *outcome != "settled" {
				t.Fatal("the outcome update did not commit", outcome)
			}
		})

		ApplyDbMigrations(ctx)
		if version := DbVersion(ctx); version != MigrationCount() {
			t.Fatalf("the stream after the retirement reached version %d, want %d", version, MigrationCount())
		}
	})
}
