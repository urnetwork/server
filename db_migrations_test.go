package server

import (
	"context"
	"testing"
	// "github.com/urnetwork/connect"
)

func TestApplyDbMigrations(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()

		ApplyDbMigrations(ctx)
	})
}
