package jwt

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	// "github.com/urnetwork/glog/v2025"

	"github.com/urnetwork/server/v2025"
)

func TestAppleJwk(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		appleJwkValidator := NewAppleJwkValidator(ctx)

		keys := appleJwkValidator.Keys()
		assert.NotEqual(t, 0, len(keys))
	})
}
