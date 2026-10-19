// Published DDL is the healthy control on both sides of its exact successor.
package monitor

import (
	"testing"

	"github.com/urnetwork/server"
)

// Portable to the pre-fix detector: published v850 must stay healthy without
// removing any earlier lifecycle requirement or changing fixture DDL.
func TestMigrationsSourceGraphPublishedTransition(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{849, 850} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			_, drift := migrationPingDatabaseCheck(t, t.Context())
			if drift != "" {
				t.Fatalf("published source graph prefix %d reports drift: %s", head, drift)
			}
		}
	})
}
