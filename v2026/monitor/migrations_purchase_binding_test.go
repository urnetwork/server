package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestMigrationsPurchaseBindingCatalog(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		check := func(apple, play bool) {
			t.Helper()
			server.Db(ctx, func(conn server.PgConn) {
				var actualApple, actualPlay bool
				server.Raise(conn.QueryRow(ctx, "SELECT "+appleOfferCodeBindingArtifactQuery+", "+playPurchaseBindingArtifactQuery).Scan(&actualApple, &actualPlay))
				if actualApple != apple || actualPlay != play {
					t.Fatalf("store binding catalogs = (%t,%t), want (%t,%t)", actualApple, actualPlay, apple, play)
				}
			})
		}
		server.ApplyDbMigrationsUpTo(ctx, 764)
		check(false, false)
		server.ApplyDbMigrationsUpTo(ctx, 765)
		check(true, false)
		server.ApplyDbMigrationsUpTo(ctx, 766)
		check(true, true)
		for _, fault := range []struct{ apply, restore, artifact string }{
			{`ALTER TABLE apple_offer_code_binding ALTER COLUMN transaction_id TYPE varchar(129)`, `ALTER TABLE apple_offer_code_binding ALTER COLUMN transaction_id TYPE varchar(128)`, "Apple offer-code transaction bindings@v765"},
			{`ALTER TABLE apple_offer_code_binding ALTER COLUMN network_id DROP NOT NULL`, `ALTER TABLE apple_offer_code_binding ALTER COLUMN network_id SET NOT NULL`, "Apple offer-code transaction bindings@v765"},
			{`ALTER TABLE apple_offer_code_binding ALTER COLUMN bound_at SET DEFAULT now()`, `ALTER TABLE apple_offer_code_binding ALTER COLUMN bound_at DROP DEFAULT`, "Apple offer-code transaction bindings@v765"},
			{`ALTER TABLE apple_offer_code_binding DROP CONSTRAINT apple_offer_code_binding_pkey; ALTER TABLE apple_offer_code_binding ADD PRIMARY KEY(original_transaction_id,network_id)`, `ALTER TABLE apple_offer_code_binding DROP CONSTRAINT apple_offer_code_binding_pkey; ALTER TABLE apple_offer_code_binding ADD PRIMARY KEY(original_transaction_id)`, "Apple offer-code transaction bindings@v765"},
			{`ALTER TABLE play_purchase_binding ALTER COLUMN offer TYPE varchar(255)`, `ALTER TABLE play_purchase_binding ALTER COLUMN offer TYPE varchar(256)`, "Google Play purchase-token bindings@v766"},
			{`ALTER TABLE play_purchase_binding ALTER COLUMN root_purchase_token DROP NOT NULL`, `ALTER TABLE play_purchase_binding ALTER COLUMN root_purchase_token SET NOT NULL`, "Google Play purchase-token bindings@v766"},
			{`ALTER TABLE play_purchase_binding ALTER COLUMN bound_at SET DEFAULT now()`, `ALTER TABLE play_purchase_binding ALTER COLUMN bound_at DROP DEFAULT`, "Google Play purchase-token bindings@v766"},
			{`ALTER TABLE play_purchase_binding DROP CONSTRAINT play_purchase_binding_pkey; ALTER TABLE play_purchase_binding ADD PRIMARY KEY(root_purchase_token)`, `ALTER TABLE play_purchase_binding DROP CONSTRAINT play_purchase_binding_pkey; ALTER TABLE play_purchase_binding ADD PRIMARY KEY(purchase_token)`, "Google Play purchase-token bindings@v766"},
		} {
			server.Db(ctx, func(conn server.PgConn) { server.RaisePgResult(conn.Exec(ctx, fault.apply)) })
			_, drift := migrationPingDatabaseCheck(t, ctx)
			if !strings.Contains(drift, fault.artifact) {
				t.Fatalf("store binding drift missing %q: %s", fault.artifact, drift)
			}
			server.Db(ctx, func(conn server.PgConn) { server.RaisePgResult(conn.Exec(ctx, fault.restore)) })
			if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
				t.Fatalf("restored store binding schema reports drift: %s", drift)
			}
		}
	})
}
