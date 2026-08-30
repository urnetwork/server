package mcp

import (
	"context"
	"errors"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// Startup gates all MCP warmup work on the same migration-aware readiness
// check used by the other schema-dependent services. The shared check latches
// /status to an error on failure so warpctl keeps the working old container.
func Startup(ctx context.Context) error {
	return startup(ctx, router.StartupReadiness, server.Warmup)
}

func startup(
	ctx context.Context,
	readiness func(context.Context) error,
	warmup func(),
) error {
	if ctx == nil {
		return errors.New("mcp startup context is nil")
	}
	if err := readiness(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	warmup()
	return nil
}
