package nginx

// The lifetime of the front's limit state (owner: s-http). The limits
// themselves come from the route file, each interface's rate_limit and each
// service's route_rate_limits (D8); these settings only bound the state that
// enforces them.

import (
	"time"
)

// How long limit state lives. State is keyed by interface name and
// normalized client IP, and a route's state also by service and route.
// Read-only once the front is built.
type LimitSettings struct {
	// how long one key's state may sit idle, with nothing in flight and its
	// bucket refilled, before it is dropped: nginx frees a limit_req node idle
	// for 60 s with no excess left (ngx_http_limit_req_expire). Dropping it
	// changes no decision, since a refilled bucket admits as a new key's does.
	// Non-positive drops state as soon as it is idle and refilled.
	StateIdleTimeout time.Duration
	// how often idle state is swept, as alt's limiter sweeps every 60 s.
	// Non-positive disables the sweep.
	PruneInterval time.Duration
}

// A new instance on every call, with nginx's expiry.
func DefaultLimitSettings() *LimitSettings {
	return &LimitSettings{
		StateIdleTimeout: 60 * time.Second,
		PruneInterval:    60 * time.Second,
	}
}
