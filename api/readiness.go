package api

import (
	"context"

	"github.com/urnetwork/server/router"
)

// ReadinessCheck runs the one-shot deep checks behind the /status readiness
// latch: the successful PostgreSQL migration head must satisfy the running
// binary and Redis must answer, or this container must not take over from the
// working old one. It delegates to the shared check used by taskworker and
// Connect so the service entry points cannot silently drift back to SELECT 1.
// It runs once at startup by design — /status stays O(1) per poll, and runtime
// health remains the monitor's job
// (APIDRAIN1.md §2.1; same shape as the taskworker's check, TASKDRAIN1 §2.2).
func ReadinessCheck(ctx context.Context) error {
	return readinessCheckWith(ctx, router.CheckStartupReadiness)
}

func readinessCheckWith(
	ctx context.Context,
	check func(context.Context) error,
) error {
	return check(ctx)
}
