package nginx

// The lifetime and size of the front's limit state (owner: s-http). The
// limits themselves come from the route file, each interface's rate_limit and
// each service's route_rate_limits (D8); these settings only bound the state
// that enforces them, and say how a request's credential is identified for
// the identity limit (LBALTOPT2 R15), which the route file turns on.

import (
	"net/http"
	"time"
)

// How long limit state lives, and how much of it there may be. State is
// keyed by interface name and normalized client IP, and a route's bucket
// lives in its address's state; the identity limit's state is keyed by
// interface name and identity. Read-only once the front is built.
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
	// Maximum idle states of one kind examined in one admission lock
	// acquisition. Values below one examine one state.
	PruneBatchSize int
	// the most (interface, address) states held at once, so a source that
	// sprays addresses can't grow the state without bound. At the cap, a new
	// address replaces the least recently used state with nothing in flight,
	// forgetting that address's buckets, as nginx frees the oldest nodes of a
	// full zone. When every state has something in flight, a new address is
	// refused with 429, as nginx's limit_conn refuses when its zone is full.
	// nginx's 256 MiB zones hold about two million IPv6 nodes; a state here
	// costs about 250 bytes with its place in the map and the idle order, so
	// the default bounds the state near 250 MiB. Non-positive means no cap.
	MaxAddressStates int
	// the most (interface, identity) states the identity limit holds at once,
	// bounded as MaxAddressStates bounds the addresses': at the cap, a new
	// identity replaces the least recently used identity with nothing in
	// flight. When every identity has something in flight, a new identity's
	// request is charged as a request without one, so a full table never
	// refuses what the address limits admit. Only a verified credential
	// creates identity state. A state costs about 200 bytes, so the default
	// bounds it near 200 MiB. Non-positive means no cap.
	MaxIdentityStates int
	// identifies a request's credential for the identity limit: the client
	// or network its credential verifies as, or false for a request with no
	// credential, or one that doesn't verify, which is charged as if the
	// identity limit were off. The front calls it only on an interface whose
	// rate_limit turns the identity limit on, for a request of the shared
	// zone (not a route zone's) to the api or the connect service, named by
	// upstream (NameClassApi or NameClassConnect), before the request is
	// charged. It must be safe for concurrent use and must not keep r. Nil
	// leaves the identity limit off whatever the route file says.
	IdentifyRequest func(r *http.Request, upstream NameClass) (identity LimitIdentity, ok bool)
}

// A new instance on every call, with nginx's expiry and no identification.
func DefaultLimitSettings() *LimitSettings {
	return &LimitSettings{
		StateIdleTimeout:  60 * time.Second,
		PruneInterval:     60 * time.Second,
		PruneBatchSize:    256,
		MaxAddressStates:  1024 * 1024,
		MaxIdentityStates: 1024 * 1024,
	}
}
