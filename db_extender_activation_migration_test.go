package server

import (
	"context"
	"testing"
)

func TestExtenderActivationHistoryMigrationAfterUsageSnapshot(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		const previousVersion = 681
		if index := migrationIndex(t, "CREATE TABLE network_extender_activation ("); index != previousVersion {
			t.Fatalf("activation table migration index = %d, want %d", index, previousVersion)
		}
		if index := migrationIndex(t, "CREATE INDEX network_extender_activation_extender_id_activate_time"); index != previousVersion+1 {
			t.Fatalf("activation index migration index = %d, want %d", index, previousVersion+1)
		}

		checkArtifacts := func(want bool) {
			t.Helper()
			MaintenanceDb(ctx, func(conn PgConn) {
				for _, name := range []string{
					"public.network_extender_activation",
					"public.network_extender_activation_extender_id_activate_time",
				} {
					var exists bool
					Raise(conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists))
					if exists != want {
						t.Fatalf("artifact %s exists = %t, want %t", name, exists, want)
					}
				}
			}, OptReadOnly(), OptNoRetry())
		}

		ApplyDbMigrationsUpTo(ctx, previousVersion)
		checkArtifacts(false)
		ApplyDbMigrations(ctx)
		checkArtifacts(true)
		ApplyDbMigrations(ctx)
		checkArtifacts(true)
	})
}
