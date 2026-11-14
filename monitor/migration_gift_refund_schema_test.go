// The gift refund contract (migration_gift_refund_schema.go): registered at
// its migration's version, and its predicate admits exactly the schema the
// migration publishes. Its predicate is evaluated directly rather than through
// the migrations signal, which reads a head beyond a gap in the catalog's
// contracts as cannot-observe.
package monitor

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// The contract follows its migration's position in the server's list, and pins
// its version's row column with no retirement.
func TestMigrationsGiftRefundSchemaContractFollowsItsVersion(t *testing.T) {
	version := server.GiftRefundSchemaVersion()
	head := server.MigrationCount()
	if version < 1 || head < version {
		t.Fatalf("gift refund migration is not published: version=%d head=%d", version, head)
	}
	if version <= server.GiftSchemaVersion() {
		t.Fatalf("gift refund migration %d is not after the gift schema's %d", version, server.GiftSchemaVersion())
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 {
		t.Fatalf("gift refund contract is %+v", contract.artifact)
	}
}

// One version behind, the tables are absent and the predicate refuses the
// schema; at its version the real schema is admitted; and each rolled-back
// fault of what it publishes is refused.
func TestMigrationsGiftRefundSchemaAdmitsThePublishedSchema(t *testing.T) {
	version := server.GiftRefundSchemaVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	faults := []struct{ name, sql string }{
		{"voids absent", `DROP TABLE gift_code_void`},
		{"voids not durable", `ALTER TABLE gift_code_void SET UNLOGGED`},
		{"a code voided twice", `ALTER TABLE gift_code_void DROP CONSTRAINT gift_code_void_pkey`},
		{"void cause nullable", `ALTER TABLE gift_code_void ALTER COLUMN cause DROP NOT NULL`},
		{"void source narrowed", `ALTER TABLE gift_code_void ALTER COLUMN source_id TYPE varchar(32)`},
		{"void time zoned", `ALTER TABLE gift_code_void ALTER COLUMN void_time TYPE timestamp with time zone`},
		{"void year flag absent", `ALTER TABLE gift_code_void DROP COLUMN ended_year`},
		{"voids unindexed by order", `DROP INDEX gift_code_void_order`},
		{"refunds absent", `DROP TABLE gift_order_refund`},
		{"a refund counted twice", `ALTER TABLE gift_order_refund DROP CONSTRAINT gift_order_refund_pkey`},
		{"refund amount nullable", `ALTER TABLE gift_order_refund ALTER COLUMN amount_cents DROP NOT NULL`},
		{"refunds unindexed by order", `DROP INDEX gift_order_refund_order`},
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
