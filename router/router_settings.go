package router

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// Tunables of the router and of the answers its wrappers give.
type RouterSettings struct {
	// The largest JSON request body a wrapper reads, in bytes. The default
	// matches the public load balancer's cap, so a request sent straight to a
	// backend cannot pass a body the public limit refuses. A route with
	// smaller payloads adds a tighter cap of its own.
	JsonRequestMaxByteCount int64
	// The Retry-After on the 503 that answers a request whose Postgres
	// connection was cut, as a Postgres or PgBouncer restart cuts it (QD-759).
	// The pool dials a new connection at its next use, so the default is the
	// shortest wait the header can carry, 1 s. Sent in whole seconds, rounded
	// up; non-positive sends the 503 with no hint.
	DbConnectionLostRetryAfter time.Duration
	// The Retry-After on the 503 that answers a lock wait the transaction's
	// bound ended (server.PgLockTimeout). The lock's holder is a transaction
	// in flight, so the default is 1 s. Whole seconds, rounded up;
	// non-positive sends no hint.
	LockTimeoutRetryAfter time.Duration
	// The Retry-After on the 503 that answers an authentication dependency's
	// outage with the body the failure declares safe. 1 s by default. Whole
	// seconds, rounded up; non-positive sends no hint.
	AuthUnavailableRetryAfter time.Duration
	// The window of the per-route stats a router logs. 60 s by default.
	StatsWindow time.Duration
}

func DefaultRouterSettings() *RouterSettings {
	return &RouterSettings{
		JsonRequestMaxByteCount:    16 * 1024 * 1024,
		DbConnectionLostRetryAfter: time.Second,
		LockTimeoutRetryAfter:      time.Second,
		AuthUnavailableRetryAfter:  time.Second,
		StatsWindow:                60 * time.Second,
	}
}

var routerSettings atomic.Pointer[RouterSettings]

// The settings every request reads while none is installed: one instance, so
// the request path allocates none.
var routerSettingsDefault = DefaultRouterSettings()

// Installs the process's router settings; nil restores the defaults. A router
// takes its stats window when it is made, and every request reads the rest as
// it is answered.
func SetRouterSettings(settings *RouterSettings) {
	routerSettings.Store(settings)
}

func currentRouterSettings() *RouterSettings {
	if settings := routerSettings.Load(); settings != nil {
		return settings
	}
	return routerSettingsDefault
}

// Sets Retry-After to retryAfter in whole seconds, rounded up. A non-positive
// retryAfter sets nothing.
func setRetryAfter(w http.ResponseWriter, retryAfter time.Duration) {
	if retryAfter <= 0 {
		return
	}
	seconds := (retryAfter + time.Second - 1) / time.Second
	w.Header().Set("Retry-After", strconv.FormatInt(int64(seconds), 10))
}
