//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world's chaos hook on every block (world_test.go's World.Restart): each block, stopped mid-run as a crash or a
// deploy's SIGTERM stops it, starts again on its stores and new listeners and reads ready again as at the world's
// start; its origin answers 503 while it is down, as the lb does with no upstream, and its routes again once it is
// back. Run it through the build's wrapper: -tags 'sidecar_e2e sidecar_pending sidecar_localstack'.

package sidecare2e

import (
	"net/http"
	"testing"
)

// Every block restarts on its stores, its origin unavailable between its stop and its start.
func TestE2eWorldRestartsEveryBlockOnItsStores(t *testing.T) {
	e2eTestEnv().Run(t, func(t testing.TB) {
		world := NewWorld(t, nil)
		defer world.Close()
		for _, block := range world.blocks {
			if err := world.Stop(block.Name()); err != nil {
				t.Fatalf("%s's stop: %v", block.Name(), err)
			}
			if block.service != "" {
				status, answer := worldCall(t.Context(), world, http.MethodGet, world.T3().Api().ServiceOrigin(block.service)+"/status", "", "")
				if status != http.StatusServiceUnavailable {
					t.Fatalf("%s's origin answered %d while it was stopped, not 503: %s", block.Name(), status, answer)
				}
			}
			if err := world.Start(block.Name()); err != nil {
				world.FailProgress(t, block.Name()+"'s start again", err, nil)
			}
			if runs := block.RunCount(); runs != 2 {
				t.Fatalf("%s ran %d times after one restart", block.Name(), runs)
			}
			if block.service != "" {
				status, answer := worldCall(t.Context(), world, http.MethodGet, world.T3().Api().ServiceOrigin(block.service)+"/status", "", "")
				if status != http.StatusOK {
					t.Fatalf("%s's origin answered /status %d after its restart: %s", block.Name(), status, answer)
				}
			}
		}
	})
}
