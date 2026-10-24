// The write-path migrations of PARTITION-MAINTENANCE-PLAN.md Part B, read
// back from the migrated schema.
package server

import (
	"context"
	"testing"
)

// The legacy auth attempt table is gone after every migration: rate limiting
// counts attempts in Redis and nothing reads or writes the table.
func TestUserAuthAttemptTableIsDropped(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var exists bool
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT to_regclass('public.user_auth_attempt') IS NOT NULL`).Scan(&exists))
		}, OptReadOnly())
		if exists {
			t.Fatal("user_auth_attempt exists after every migration")
		}
	})
}
