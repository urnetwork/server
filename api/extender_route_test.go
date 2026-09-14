package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// The extender activation route (connect/EXTENDER.md C2).

// A provider on a shipped binary posts to exactly this path and method, so the
// route is part of the wire contract rather than an implementation detail. One
// entry, because a second would make which handler answers depend on the order
// of the table.
func TestExtenderActivateRouteIsRegistered(t *testing.T) {
	routeIds := []string{}
	for _, route := range Routes() {
		if strings.Contains(route.String(), "extender-activate") {
			routeIds = append(routeIds, route.String())
		}
	}
	if !slices.Equal(routeIds, []string{"POST ^/network/extender-activate$"}) {
		t.Fatalf("extender activate routes = %v", routeIds)
	}
}

// Served through the router, the route requires a client jwt. An activation
// probes the caller's own address back, so an unauthenticated caller reaching
// it would be a dial of an address of its own choosing on the operator's
// budget.
func TestExtenderActivateRouteRequiresAClientJwt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	apiRouter := router.NewRouter(ctx, Routes())

	r := httptest.NewRequest(
		http.MethodPost,
		"/network/extender-activate",
		strings.NewReader(`{"public_key_hex":"aa","carriers":["tcp"]}`),
	)
	r.RemoteAddr = "198.51.100.30:4000"
	w := httptest.NewRecorder()
	apiRouter.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a client jwt", w.Code)
	}

	// and only on POST: a GET of the same path is not the activation
	r = httptest.NewRequest(http.MethodGet, "/network/extender-activate", nil)
	r.RemoteAddr = "198.51.100.30:4000"
	w = httptest.NewRecorder()
	apiRouter.ServeHTTP(w, r)
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("a GET reached the activation handler: %d", w.Code)
	}
}

// Alt mounts the same route table on http3 that the api serves behind the lb
// (EXTENDER.md L1), so one constructor builds both and hands back the close
// that joins the request-time cache it owns. A second table would let the two
// fronts drift apart silently, which is exactly what a client reaching one
// front and not the other looks like.
func TestNewRouterBuildsTheTableBothFrontsServe(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		apiRouter, closeApiRouter, err := NewRouter(ctx, ctx)
		if err != nil {
			t.Fatalf("NewRouter: %v", err)
		}
		if apiRouter == nil || closeApiRouter == nil {
			t.Fatal("NewRouter returned nothing to serve or nothing to close")
		}

		// the same probes through a router built straight from the table
		tableRouter := router.NewRouter(ctx, Routes())
		probes := []struct {
			method string
			path   string
			body   string
		}{
			{method: http.MethodGet, path: "/status"},
			{
				method: http.MethodPost,
				path:   "/network/extender-activate",
				body:   `{"public_key_hex":"aa","carriers":["tcp"]}`,
			},
			{method: http.MethodGet, path: "/no/such/route"},
		}
		for _, probe := range probes {
			serve := func(handler http.Handler) int {
				r := httptest.NewRequest(probe.method, probe.path, strings.NewReader(probe.body))
				r.RemoteAddr = "198.51.100.31:4000"
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				return w.Code
			}
			if got, want := serve(apiRouter), serve(tableRouter); got != want {
				t.Fatalf("%s %s = %d through NewRouter, %d through the table", probe.method, probe.path, got, want)
			}
		}

		closeApiRouter()
	})
}
