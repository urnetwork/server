package session

import (
	"context"
	"os"
	"testing"

	"github.com/urnetwork/connect"

	// "github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Fetches Apple's live key set, so it runs only on request.
// TestJwkValidatorLoadsServedKeySet covers the same fetch against a local
// server.
func TestAppleJwk(t *testing.T) {
	if os.Getenv("URNETWORK_LIVE_THIRD_PARTY_TESTS") != "1" {
		t.Skip("live Apple key set; set URNETWORK_LIVE_THIRD_PARTY_TESTS=1 to run")
	}
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		appleJwkValidator := NewAppleJwkValidator(ctx)

		keys := appleJwkValidator.Keys()
		connect.AssertNotEqual(t, 0, len(keys))
	})
}
