package jwt

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"

	// "github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

func TestGoogleJwk(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		appleJwkValidator := NewGoogleJwkValidator(ctx)

		keys := appleJwkValidator.Keys()
		connect.AssertNotEqual(t, 0, len(keys))
	})
}
