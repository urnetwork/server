package mcp

import (
	"context"
	"errors"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

func mcpWarmupTargets() []server.WarmupTarget {
	// MCP resolves provider locations for both the providerLocations tool and
	// fetch proxy creation. That path geolocates the caller, maps its country,
	// and optionally searches location names. It does not use network-name
	// search or the full location directory.
	return []server.WarmupTarget{
		server.WarmupTargetIPDatabase,
		server.WarmupTargetCountryLocations,
		server.WarmupTargetLocationSearch,
	}
}

// Startup gates all MCP warmup work on the same migration-aware readiness
// check used by the other schema-dependent services. The shared check latches
// /status to an error on failure so warpctl keeps the working old container.
func Startup(ctx context.Context) error {
	return startup(ctx, router.StartupReadiness, func() {
		server.Warmup(mcpWarmupTargets()...)
	})
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
