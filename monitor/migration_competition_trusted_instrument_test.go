// The trusted instrument overlay (v900) through the public migration signal:
// published prefixes on both sides of it are healthy, and faults in its own
// and its retained custody reach the drift alert under v900.
package monitor

import (
	"testing"

	"github.com/urnetwork/server"
)

// The 2026-10-30 page: at heads 900 and 901 the signal reported the v897
// custody absent, because 900 legitimately replaces the source authorization
// body that v897 pins. Every published prefix around the overlay is coherent.
func TestMigrationsTrustedInstrumentPublishedPrefixesHaveNoDrift(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{897, 899, 900, 901} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatalf("published prefix %d reports drift: %s", head, drift)
			}
		}
	})
}
