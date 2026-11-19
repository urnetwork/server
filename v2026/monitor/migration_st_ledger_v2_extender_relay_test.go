// The extender relay chunks contract (migration_st_ledger_v2_extender_relay.go):
// registered at its migration's version, and its predicate admits exactly the
// column the migration publishes. Its predicate is evaluated directly here, and
// through the migrations signal as well (migration_current_suffix_test.go).
package monitor

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The contract follows its migration's position in the server's list, after
// the extender pool's columns, and pins its version's row column with no
// retirement.
func TestMigrationsStLedgerV2ExtenderRelayContractFollowsItsVersion(t *testing.T) {
	version := server.StLedgerV2ExtenderRelayMigrationVersion()
	head := server.MigrationCount()
	if version < 1 || head < version {
		t.Fatalf("extender relay chunks migration is not published: version=%d head=%d", version, head)
	}
	if pool := server.StLedgerV2ExtenderMigrationVersion(); version <= pool {
		t.Fatalf("extender relay chunks migration %d is not after the extender pool's %d", version, pool)
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 {
		t.Fatalf("extender relay chunks contract is %+v", contract.artifact)
	}
}

// One version behind, the column is absent and the predicate refuses the
// schema; at its version the real schema is admitted; and each rolled-back
// shape fault is refused. A release from before the column inserts its epoch
// rows without it, so a required column would refuse its rows, and a default
// would give an epoch the amendment does not cover relay chunks.
func TestMigrationsStLedgerV2ExtenderRelayAdmitsThePublishedColumn(t *testing.T) {
	version := server.StLedgerV2ExtenderRelayMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	faults := []struct{ name, sql string }{
		{"relay chunks absent", `ALTER TABLE st_ledger_v2_epoch_rows DROP COLUMN extender_relay_chunks`},
		{"relay chunks required", `ALTER TABLE st_ledger_v2_epoch_rows ALTER COLUMN extender_relay_chunks SET NOT NULL`},
		{"relay chunks default to none", `ALTER TABLE st_ledger_v2_epoch_rows ALTER COLUMN extender_relay_chunks SET DEFAULT '{}'`},
		{"relay chunks of another type", `ALTER TABLE st_ledger_v2_epoch_rows ALTER COLUMN extender_relay_chunks TYPE varchar(71)[]`},
	}
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.ApplyDbMigrationsUpTo(t.Context(), version-1)
		snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
			snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
		})
		server.ApplyDbMigrationsUpTo(t.Context(), version)
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
			})
		}
	})
}
