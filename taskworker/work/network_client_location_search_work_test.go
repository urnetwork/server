package work

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The re-index runs in one transaction that lifts the application role's
// transaction bound for itself (PARTITION-MAINTENANCE-PLAN.md A6), and a second
// run finds every value up to date.
func TestIndexSearchLocationsCompletesTwice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		for range 2 {
			if _, err := IndexSearchLocations(&IndexSearchLocationsArgs{}, clientSession); err != nil {
				t.Fatal(err)
			}
		}
	})
}
