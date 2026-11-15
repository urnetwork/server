// The gift gamma contract (migration_gift_gamma_schema.go): registered at its
// migration's version, and its predicate admits exactly the schema the
// migration publishes. Its predicate is evaluated directly rather than through
// the migrations signal, which reads a head beyond a gap in the catalog's
// contracts as cannot-observe.
package monitor

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// The contract follows its migration's position in the server's list, after
// the gift refund schema's, and pins its version's row column with no
// retirement.
func TestMigrationsGiftGammaSchemaContractFollowsItsVersion(t *testing.T) {
	version := server.GiftGammaSchemaVersion()
	head := server.MigrationCount()
	if version < 1 || head < version {
		t.Fatalf("gift gamma migration is not published: version=%d head=%d", version, head)
	}
	if version <= server.GiftRefundSchemaVersion() {
		t.Fatalf("gift gamma migration %d is not after the gift refund schema's %d", version, server.GiftRefundSchemaVersion())
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 {
		t.Fatalf("gift gamma contract is %+v", contract.artifact)
	}
}

// One version behind, the column is absent and the predicate refuses the
// schema; at its version the real schema is admitted; and each rolled-back
// fault of what it publishes is refused. One spend finds one order by its
// unique intent; the order of every other rail has none; and the gift
// schema's contract still admits the table the column was added to.
func TestMigrationsGiftGammaSchemaAdmitsThePublishedSchema(t *testing.T) {
	version := server.GiftGammaSchemaVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	giftContract := migrationCurrentSuffixTestContract(t, server.GiftSchemaVersion())
	faults := []struct{ name, sql string }{
		{"intent link absent", `ALTER TABLE gift_order DROP COLUMN gamma_intent_id`},
		{"one intent, two orders", `ALTER TABLE gift_order DROP CONSTRAINT gift_order_gamma_intent_id_key`},
		{"intent link required", `ALTER TABLE gift_order ALTER COLUMN gamma_intent_id SET NOT NULL`},
		{"intent link not a uuid", `ALTER TABLE gift_order ALTER COLUMN gamma_intent_id TYPE text`},
		{"intent link defaulted", `ALTER TABLE gift_order ALTER COLUMN gamma_intent_id SET DEFAULT gen_random_uuid()`},
		{"uniqueness deferred", `ALTER TABLE gift_order DROP CONSTRAINT gift_order_gamma_intent_id_key; ALTER TABLE gift_order ADD CONSTRAINT gift_order_gamma_intent_id_key UNIQUE (gamma_intent_id) DEFERRABLE INITIALLY DEFERRED`},
	}
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.ApplyDbMigrationsUpTo(t.Context(), version-1)
		snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
			snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
		})
		server.ApplyDbMigrationsUpTo(t.Context(), version)
		snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
			snMainnetMigrationAdmitted(t, ctx, tx, giftContract.query, true)
		})
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
