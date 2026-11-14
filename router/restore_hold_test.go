package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// §19.10's restore hold on one route (OPS-M3-03): held, it answers 503 with Retry-After in whole seconds and its
// handler does not run; released, the handler runs; and a test's handler swap keeps the hold outermost.
func TestRestoreHoldRefusesOnlyWhileHeld(t *testing.T) {
	held := true
	reached := 0
	route := NewRoute("POST", "/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		reached += 1
		w.WriteHeader(http.StatusOK)
	})
	route = RefuseWhileRestoreHeld(route, func() bool { return held }, 59500*time.Millisecond)
	connect.AssertEqual(t, route.RefusesWhileRestoreHeld(), true)
	serve := func(route *Route) *httptest.ResponseRecorder {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := httptest.NewRecorder()
		NewRouter(ctx, []*Route{route}).ServeHTTP(w, httptest.NewRequest("POST", "/oauth/token", nil))
		return w
	}

	w := serve(route)
	connect.AssertEqual(t, w.Code, http.StatusServiceUnavailable)
	connect.AssertEqual(t, w.Header().Get("Retry-After"), "60")
	connect.AssertEqual(t, reached, 0)

	stubbed := 0
	swapped := Testing_WithHandler(route, func(w http.ResponseWriter, r *http.Request) {
		stubbed += 1
		w.WriteHeader(http.StatusOK)
	})
	connect.AssertEqual(t, serve(swapped).Code, http.StatusServiceUnavailable)
	connect.AssertEqual(t, stubbed, 0)

	held = false
	connect.AssertEqual(t, serve(route).Code, http.StatusOK)
	connect.AssertEqual(t, reached, 1)
	connect.AssertEqual(t, serve(swapped).Code, http.StatusOK)
	connect.AssertEqual(t, stubbed, 1)
	connect.AssertEqual(t, NewRoute("GET", "/status", nil).RefusesWhileRestoreHeld(), false)
}
