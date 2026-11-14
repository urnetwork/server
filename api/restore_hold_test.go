package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// §19.10's restore hold through the routes the API serves (plan-OPS-restore-hold; OPS-M3-03). Pure, as the other
// route table tests: while sidecar.yml holds, every route but the status route answers 503 with Retry-After 60 before
// its handler or any other gate runs, the token exchange among them; released, the stub runs again.
func TestRestoreHoldHoldsEveryRouteButStatus(t *testing.T) {
	statusSeen := false
	for _, route := range Routes() {
		key := routeAccessKey(route)
		if route.RefusesWhileRestoreHeld() == restoreHoldOpenRoutes[key] {
			t.Fatalf("%s: restore hold = %t", key, route.RefusesWhileRestoreHeld())
		}
		statusSeen = statusSeen || key == "GET /status"
	}
	connect.AssertEqual(t, statusSeen, true)

	serve := func(method string, path string) (int, string, bool) {
		for _, route := range Routes() {
			if route.Method() != method || routeAccessTestPath(route.Pattern()) != path {
				continue
			}
			reached := false
			stubbed := router.Testing_WithHandler(route, func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := serveRouteAccess(router.NewRouter(ctx, []*router.Route{stubbed}), method, path, "")
			return w.Code, w.Header().Get("Retry-After"), reached
		}
		t.Fatalf("no route %s %s", method, path)
		return 0, "", false
	}

	func() {
		defer server.Testing_PushSidecarConfig("sidecar:\n  restore_hold: true\n")()
		code, retryAfter, reached := serve("POST", "/oauth/token")
		connect.AssertEqual(t, code, http.StatusServiceUnavailable)
		connect.AssertEqual(t, retryAfter, "60")
		connect.AssertEqual(t, reached, false)
		code, _, reached = serve("GET", "/status")
		connect.AssertEqual(t, code, http.StatusOK)
		connect.AssertEqual(t, reached, true)
	}()
	defer server.Testing_PushSidecarConfig("sidecar:\n  restore_hold: false\n")()
	code, _, reached := serve("POST", "/oauth/token")
	connect.AssertEqual(t, code, http.StatusOK)
	connect.AssertEqual(t, reached, true)
}
