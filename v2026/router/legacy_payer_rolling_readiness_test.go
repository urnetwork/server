package router

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The known index-only tail may remain pending; the writer column and trigger
// must already be committed. The explicit full-head check remains strict.
func TestLegacyPayerRollingStartupReadiness(t *testing.T) {
	if server.MigrationCount() != 793 {
		t.Skip("rolling allowance is limited to the exact published head793")
	}
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrationsUpTo(ctx, 790)
		if err := CheckStartupReadiness(ctx); err == nil || !strings.Contains(err.Error(), "head 790") {
			t.Fatalf("writer admitted before payer column/trigger: %v", err)
		}
		server.ApplyDbMigrationsUpTo(ctx, 791)
		server.Db(ctx, func(conn server.PgConn) {
			var missing bool
			server.Raise(conn.QueryRow(ctx, `SELECT
			 to_regclass('public.legacy_settlement_intent_payer_due') IS NULL AND
			 to_regclass('public.legacy_settlement_intent_payer_missing') IS NULL`).Scan(&missing))
			if !missing {
				t.Fatal("fixture unexpectedly has optional payer indexes")
			}
		})
		if err := CheckStartupReadiness(ctx); err != nil {
			t.Fatalf("known head793 source not ready on committed791: %v", err)
		}
		for _, fullHead := range []int{793, 794} {
			if err := startupReadinessCheckAtMigration(ctx, fullHead); err == nil {
				t.Fatalf("explicit required head%d admitted database791", fullHead)
			}
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := CheckStartupReadiness(canceled); err == nil {
			t.Fatal("canceled startup admitted")
		}
	})
}
