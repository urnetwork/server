package jwt

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	// "github.com/golang/glog"

	"github.com/urnetwork/server"
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
