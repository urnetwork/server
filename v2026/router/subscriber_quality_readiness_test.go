// A guarded writer must wait for its schema; older serving binaries retain the
// existing lower-bound readiness contract on the successor database.
package router

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Verify real migration audit reads at both sides of the 750-to-751 boundary.
func TestSubscriberQualityWriteGuardReadiness(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrationsUpTo(ctx, 750)
		if err := startupReadinessCheckAtMigration(ctx, 751); err == nil || !strings.Contains(err.Error(), "database migration head 750") {
			t.Fatalf("successor readiness on schema750: %v", err)
		}
		if err := startupReadinessCheckAtMigration(ctx, 750); err != nil {
			t.Fatalf("schema750 binary readiness before upgrade: %v", err)
		}
		server.ApplyDbMigrations(ctx)
		for _, required := range []int{749, 750, server.MigrationCount()} {
			if err := startupReadinessCheckAtMigration(ctx, required); err != nil {
				t.Fatalf("binary requiring schema%d after upgrade: %v", required, err)
			}
		}
	})
}
