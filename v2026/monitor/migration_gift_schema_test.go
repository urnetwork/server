// The gift code contract (migration_gift_schema.go): registered at its
// migration's version, and its predicate admits exactly the schema the
// migration publishes. Its predicate is evaluated directly rather than through
// the migrations signal, which reads a head beyond a gap in the catalog's
// contracts as cannot-observe.
package monitor

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The contract follows its migration's position in the server's list, and pins
// its version's row column with no retirement.
func TestMigrationsGiftSchemaContractFollowsItsVersion(t *testing.T) {
	version := server.GiftSchemaVersion()
	head := server.MigrationCount()
	if version < 1 || head < version {
		t.Fatalf("gift schema migration is not published: version=%d head=%d", version, head)
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 {
		t.Fatalf("gift schema contract is %+v", contract.artifact)
	}
}

// One version behind, the columns and tables are absent and the predicate
// refuses the schema; at its version the real schema is admitted; and each
// rolled-back fault of what it publishes is refused. The redeem and check
// paths read kind on every code, and its default is what every code sold
// before reads; one payment finds one order by its unique session or invoice;
// an order's recipients and cards are keyed by their index and part.
func TestMigrationsGiftSchemaAdmitsThePublishedSchema(t *testing.T) {
	version := server.GiftSchemaVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	faults := []struct{ name, sql string }{
		{"code kind absent", `ALTER TABLE transfer_balance_code DROP COLUMN kind`},
		{"code kind defaults to a gift", `ALTER TABLE transfer_balance_code ALTER COLUMN kind SET DEFAULT 'pro_gift'`},
		{"code kind nullable", `ALTER TABLE transfer_balance_code ALTER COLUMN kind DROP NOT NULL`},
		{"code kind narrowed", `ALTER TABLE transfer_balance_code ALTER COLUMN kind TYPE varchar(8)`},
		{"code pro duration required", `ALTER TABLE transfer_balance_code ALTER COLUMN pro_duration_seconds SET NOT NULL`},
		{"code order absent", `ALTER TABLE transfer_balance_code DROP COLUMN gift_order_id`},
		{"code referrer not a uuid", `ALTER TABLE transfer_balance_code ALTER COLUMN referrer_network_id TYPE text`},
		{"orders absent", `DROP TABLE gift_order`},
		{"orders not durable", `ALTER TABLE gift_order SET UNLOGGED`},
		{"orders unkeyed", `ALTER TABLE gift_order DROP CONSTRAINT gift_order_pkey`},
		{"one session, two orders", `ALTER TABLE gift_order DROP CONSTRAINT gift_order_stripe_session_id_key`},
		{"one invoice, two orders", `ALTER TABLE gift_order DROP CONSTRAINT gift_order_crypto_intent_id_key`},
		{"order token hash nullable", `ALTER TABLE gift_order ALTER COLUMN order_token_hash DROP NOT NULL`},
		{"order token stored as text", `ALTER TABLE gift_order ALTER COLUMN order_token_hash TYPE text`},
		{"order status defaulted", `ALTER TABLE gift_order ALTER COLUMN status SET DEFAULT 'paid'`},
		{"order redeem-by zoned", `ALTER TABLE gift_order ALTER COLUMN redeem_by TYPE timestamp with time zone`},
		{"order send date a timestamp", `ALTER TABLE gift_order ALTER COLUMN send_on TYPE timestamp`},
		{"order purchase event absent", `ALTER TABLE gift_order DROP COLUMN purchase_event_base`},
		{"recipients absent", `DROP TABLE gift_order_recipient`},
		{"recipients keyed by order", `ALTER TABLE gift_order_recipient DROP CONSTRAINT gift_order_recipient_pkey; ALTER TABLE gift_order_recipient ADD PRIMARY KEY (gift_order_id, email)`},
		{"recipient code required", `ALTER TABLE gift_order_recipient ALTER COLUMN balance_code_id SET NOT NULL`},
		{"cards absent", `DROP TABLE gift_order_pdf`},
		{"cards unkeyed", `ALTER TABLE gift_order_pdf DROP CONSTRAINT gift_order_pdf_pkey`},
		{"card bytes nullable", `ALTER TABLE gift_order_pdf ALTER COLUMN pdf DROP NOT NULL`},
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
