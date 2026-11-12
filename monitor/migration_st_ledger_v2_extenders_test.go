// The SN25 extender pool contract (migration_st_ledger_v2_extenders.go):
// registered at its migration's version, and its predicate admits exactly the
// tables, keys, checks and epoch row columns the migration publishes. Its
// predicate is evaluated directly here, and through the migrations signal as
// well (migration_current_suffix_test.go).
package monitor

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// The contract follows its migration's position in the server's list, and
// pins its version's row column with no retirement.
func TestMigrationsStLedgerV2ExtenderContractFollowsItsVersion(t *testing.T) {
	version := server.StLedgerV2ExtenderMigrationVersion()
	head := server.MigrationCount()
	if version < 1 || head < version {
		t.Fatalf("extender pool migration is not published: version=%d head=%d", version, head)
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 {
		t.Fatalf("extender pool contract is %+v", contract.artifact)
	}
}

// One version behind, the tables and columns are absent and the predicate
// refuses the schema; at its version the real schema is admitted; and each
// rolled-back shape fault is refused. A release from before the columns
// inserts its epoch rows without them, so a required column would refuse its
// rows, and a default would give an epoch the amendment does not cover an
// extender component.
func TestMigrationsStLedgerV2ExtenderAdmitsThePublishedSchema(t *testing.T) {
	version := server.StLedgerV2ExtenderMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	faults := []struct{ name, sql string }{
		{"amendments absent", `DROP TABLE st_amendments`},
		{"amendments without their key", `ALTER TABLE st_amendments DROP CONSTRAINT st_amendments_pkey`},
		{"shadow report absent", `DROP TABLE st_ledger_v2_extender_shadow`},
		{"shadow report of epoch 0", `ALTER TABLE st_ledger_v2_extender_shadow DROP CONSTRAINT st_ledger_v2_extender_shadow_epoch_check`},
		{"census absent", `DROP TABLE st_ledger_v2_extender_census`},
		{"census of a negative row count", `ALTER TABLE st_ledger_v2_extender_census DROP CONSTRAINT st_ledger_v2_extender_census_census_rows_check`},
		{"census without its key", `ALTER TABLE st_ledger_v2_extender_census DROP CONSTRAINT st_ledger_v2_extender_census_pkey`},
		{"platform endpoint registry absent", `DROP TABLE st_platform_endpoint`},
		{"platform endpoint of any kind", `ALTER TABLE st_platform_endpoint DROP CONSTRAINT st_platform_endpoint_kind_check`},
		{"platform endpoint of a fourth kind", `ALTER TABLE st_platform_endpoint DROP CONSTRAINT st_platform_endpoint_kind_check;
			ALTER TABLE st_platform_endpoint ADD CONSTRAINT st_platform_endpoint_kind_check CHECK (kind IN ('tlsedge', 'smtpedge', 'mxedge', 'httpedge'))`},
		{"platform endpoint registered by no one", `ALTER TABLE st_platform_endpoint DROP CONSTRAINT st_platform_endpoint_registered_by_check`},
		{"platform endpoint revoked before it began", `ALTER TABLE st_platform_endpoint DROP CONSTRAINT st_platform_endpoint_check`},
		{"platform endpoint revoked by no one", `ALTER TABLE st_platform_endpoint DROP CONSTRAINT st_platform_endpoint_check1`},
		{"platform endpoint revocation required", `ALTER TABLE st_platform_endpoint ALTER COLUMN revoke_time SET NOT NULL`},
		{"extender bytes absent", `ALTER TABLE st_ledger_v2_epoch_rows DROP COLUMN extender_bytes`},
		{"extender bytes required", `ALTER TABLE st_ledger_v2_epoch_rows ALTER COLUMN extender_bytes SET NOT NULL`},
		{"extender bytes default to zero", `ALTER TABLE st_ledger_v2_epoch_rows ALTER COLUMN extender_bytes SET DEFAULT 0`},
		{"extender census chunks of another type", `ALTER TABLE st_ledger_v2_epoch_rows ALTER COLUMN extender_census_chunks TYPE varchar(71)[]`},
		{"extender payers default to zero", `ALTER TABLE st_ledger_v2_epoch_rows ALTER COLUMN extender_payers SET DEFAULT 0`},
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
