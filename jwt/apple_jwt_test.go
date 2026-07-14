package jwt

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"

	// "github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

func TestAppleJwk(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		appleJwkValidator := NewAppleJwkValidator(ctx)

		keys := appleJwkValidator.Keys()
		connect.AssertNotEqual(t, 0, len(keys))
	})
}
