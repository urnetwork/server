package nginx

// The lifetime and size of the front's limit state (owner: s-http). The
// limits themselves come from the route file, each interface's rate_limit and
// each service's route_rate_limits (D8); these settings only bound the state
// that enforces them.

import (
	"time"
)

// How long limit state lives, and how much of it there may be. State is
// keyed by interface name and normalized client IP, and a route's bucket
// lives in its address's state. Read-only once the front is built.
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
	// Maximum idle addresses examined in one admission lock acquisition.
	// Values below one examine one address.
	PruneBatchSize int
	// the most (interface, address) states held at once, so a source that
	// sprays addresses can't grow the state without bound. At the cap, a new
	// address replaces the least recently used state with nothing in flight,
	// forgetting that address's buckets, as nginx frees the oldest nodes of a
	// full zone. When every state has something in flight, a new address is
	// refused with 429, as nginx's limit_conn refuses when its zone is full.
	// nginx's 256 MiB zones hold about two million IPv6 nodes; a state here
	// costs about 200 bytes, so the default bounds the state near 200 MiB.
	// Non-positive means no cap.
	MaxAddressStates int
}

// A new instance on every call, with nginx's expiry.
func DefaultLimitSettings() *LimitSettings {
	return &LimitSettings{
		StateIdleTimeout: 60 * time.Second,
		PruneInterval:    60 * time.Second,
		PruneBatchSize:   256,
		MaxAddressStates: 1024 * 1024,
	}
}
