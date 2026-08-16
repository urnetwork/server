package server

import (
	"context"
	"strings"
	"testing"
	// "github.com/urnetwork/connect"
)

func TestApplyDbMigrations(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()

		ApplyDbMigrations(ctx)

		var reloptions []string
		var retiredIndexExists bool
		var idleTimeoutConfigured bool
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT coalesce(reloptions, ARRAY[]::text[])
				FROM pg_class
				WHERE oid = 'pending_task'::regclass
			`)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&reloptions))
				}
			})

			result, err = conn.Query(ctx, `
				SELECT to_regclass('transfer_contract_open_source_id_companion_contract_id') IS NOT NULL
			`)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&retiredIndexExists))
				}
			})

			result, err = conn.Query(ctx, `
				SELECT EXISTS (
					SELECT 1
					FROM pg_db_role_setting settings
					CROSS JOIN LATERAL unnest(settings.setconfig) config(value)
					WHERE settings.setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database())
					  AND settings.setrole = 0
					  AND config.value = 'idle_in_transaction_session_timeout=5min'
				)
			`)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&idleTimeoutConfigured))
				}
			})
		}, OptReadWrite())

		options := strings.Join(reloptions, ",")
		for _, want := range []string{
			"autovacuum_vacuum_scale_factor=0",
			"autovacuum_vacuum_threshold=50",
			"autovacuum_vacuum_cost_delay=0",
			"autovacuum_analyze_scale_factor=0",
			"autovacuum_analyze_threshold=50",
		} {
			if !strings.Contains(options, want) {
				t.Fatalf("pending_task reloptions %q missing %q", options, want)
			}
		}
		if retiredIndexExists {
			t.Fatal("retired 99GB companion index still exists after head migrations")
		}
		if !idleTimeoutConfigured {
			t.Fatal("database idle-in-transaction timeout was not configured")
		}
	})
}
