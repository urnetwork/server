package server

import (
	"context"
	"testing"
	// "github.com/urnetwork/connect/v2026"
)

func TestApplyDbMigrations(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()

		ApplyDbMigrations(ctx)
	})
}
