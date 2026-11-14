package router

// §19.10's restore hold on a route (plan-OPS-restore-hold, QD-076; OPS-M3-03): while held answers true, the route
// answers 503 with Retry-After before its handler runs, so after a Postgres restore no public route, the token
// exchange among them, serves the restored rows before the api step has replayed the authorization log and
// re-applied every removal since the older dump. api wraps every route but its status route (api/route_authz.go).
// The check is one atomic read of sidecar.yml's latest parse; a request in flight when the hold goes on finishes.

import (
	"net/http"
	"strconv"
	"time"
)

// What a held route answers in its body: no detail, only that it will answer again.
const RestoreHoldMessage = "Restoring. Try again shortly."

// Answers 503 with Retry-After, retryAfter rounded up to whole seconds, while held answers true, and runs route's
// handler otherwise. It is the route's outermost gate when applied last, as api applies it.
func RefuseWhileRestoreHeld(route *Route, held func() bool, retryAfter time.Duration) *Route {
	refused := *route
	refused.restoreHeld = held
	refused.restoreHoldRetryAfter = retryAfter
	refused.handler = restoreHoldGate(held, retryAfter, route.handler)
	return &refused
}

// Whether the route answers 503 while the restore hold holds it.
func (self *Route) RefusesWhileRestoreHeld() bool {
	return self.restoreHeld != nil
}

// handler behind the hold: see RefuseWhileRestoreHeld.
func restoreHoldGate(held func() bool, retryAfter time.Duration, handler http.HandlerFunc) http.HandlerFunc {
	retryAfterSeconds := int64((retryAfter + time.Second - 1) / time.Second)
	return func(w http.ResponseWriter, r *http.Request) {
		if held() {
			w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds, 10))
			http.Error(w, RestoreHoldMessage, http.StatusServiceUnavailable)
			return
		}
		handler(w, r)
	}
}
