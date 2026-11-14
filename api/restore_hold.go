package api

// §19.10's restore hold at api (plan-OPS-restore-hold, QD-076; OPS-M3-03): while sidecar.yml's restore_hold reads
// true, every route but the status route answers 503 with Retry-After before any other gate or handler runs, the
// token exchange (oauth's routes) among them, so nothing serves the restored rows until the api step has advanced
// each network's head and re-applied every removal since the older dump. applyRouteAccess wraps each route, so api's
// own front and alt's http3 front, which mounts the same table, both hold.

import (
	"time"

	"github.com/urnetwork/server"
)

// The hold's tunables.
type RestoreHoldSettings struct {
	// What a held route's Retry-After says: 60 s, the plan's answer, which is also sidecar.yml's reread interval, so a
	// client that retries then finds the hold as a fresh read has it.
	RetryAfter time.Duration
}

// The defaults.
func DefaultRestoreHoldSettings() *RestoreHoldSettings {
	return &RestoreHoldSettings{
		RetryAfter: 60 * time.Second,
	}
}

// The routes that answer while the hold is on: the status route alone, so the lb and warp see a process that serves.
var restoreHoldOpenRoutes = map[string]bool{
	"GET /status": true,
}

// Whether sidecar.yml holds api's routes now.
func restoreHeld() bool {
	return server.CurrentSidecarConfig().RestoreHold()
}
