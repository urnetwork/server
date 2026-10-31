package alt

// The api service's route_rate_limits on the alt front (rate_limit.go): an
// exact route charges a per-address bucket of its own, as its nginx location
// does behind the load balancer, and not the shared default_rate_limit
// bucket. /connect/control carries one request per contract, and a busy
// provider spent the shared bucket on it (support inbox 8093, 8077).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// One request a minute refills nothing while a test runs, so each bucket is
// exactly its burst plus one.
const testRouteServicesYml = `
default_rate_limit: &default_rate_limit
    requests_per_minute: 1
    burst: 2
    net_connections: 50
    exclude_subnets:
        - "192.0.2.0/24"

versions:
-   services:
        api:
            expose_aliases:
                - api.alt.example
            route_rate_limits:
                -   path: /connect/control
                    requests_per_minute: 1
                    burst: 5
                -   path: /other/route
                    requests_per_second: 3
        connect:
            rate_limit: *default_rate_limit
`

// The api's /connect/control spends its own budget and leaves the shared one
// whole, and each is refused once its own budget is spent. Before the route
// had a bucket of its own it spent the shared one, and was refused after the
// shared burst.
func TestLimitedHandlerCountsARouteRateLimitApartFromTheSharedBucket(t *testing.T) {
	popResource := server.Vault.PushSimpleResource(servicesResourceName, []byte(testRouteServicesYml))
	defer popResource()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := NewLimitedHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		NewLimits(ctx, DefaultLimitsSettings()),
	)
	request := func(method string, path string, remoteAddr string) int {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = remoteAddr
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}

	const addr = "198.51.100.11:4000"
	for i := range 6 {
		if code := request(http.MethodPost, "/connect/control", addr); code != http.StatusOK {
			t.Fatalf("/connect/control request %d of its own budget = %d (it spent the shared bucket)", i, code)
		}
	}
	for i := range 3 {
		if code := request(http.MethodGet, "/hello", addr); code != http.StatusOK {
			t.Fatalf("shared request %d after the route's = %d, want the shared bucket whole", i, code)
		}
	}
	if code := request(http.MethodGet, "/hello", addr); code != http.StatusTooManyRequests {
		t.Fatalf("shared request above its bucket = %d, want 429", code)
	}
	if code := request(http.MethodPost, "/connect/control", addr); code != http.StatusTooManyRequests {
		t.Fatalf("/connect/control above its own bucket = %d, want 429", code)
	}
	// another address has both of its own
	if code := request(http.MethodPost, "/connect/control", "198.51.100.12:4000"); code != http.StatusOK {
		t.Fatalf("another address's /connect/control = %d", code)
	}
	// an excluded address is never refused on the route
	for i := range 10 {
		if code := request(http.MethodPost, "/connect/control", "192.0.2.11:4000"); code != http.StatusOK {
			t.Fatalf("excluded /connect/control request %d = %d", i, code)
		}
	}
}

// The routes are read from the api service with the nginx template's units:
// requests_per_minute wins over requests_per_second.
func TestLimitsSettingsReadTheApiRouteRateLimits(t *testing.T) {
	popResource := server.Vault.PushSimpleResource(servicesResourceName, []byte(testRouteServicesYml))
	defer popResource()
	routes := DefaultLimitsSettings().Routes
	if len(routes) != 2 {
		t.Fatalf("routes = %d, want 2", len(routes))
	}
	if routes[0].Path != "/connect/control" || routes[0].RequestsPerSecond != 1.0/60.0 || routes[0].Burst != 5 {
		t.Fatalf("/connect/control = %+v", routes[0])
	}
	if routes[1].Path != "/other/route" || routes[1].RequestsPerSecond != 3 || routes[1].Burst != 0 {
		t.Fatalf("/other/route = %+v", routes[1])
	}
	// a services.yml without the key has no routes
	popResource = server.Vault.PushSimpleResource(servicesResourceName, []byte(testServicesYml))
	defer popResource()
	if routes := DefaultLimitsSettings().Routes; len(routes) != 0 {
		t.Fatalf("routes without route_rate_limits = %+v", routes)
	}
}

// A route with a bucket of its own still counts against the address's
// concurrent request cap, as limit_conn applies to its nginx location too.
func TestLimitsRouteRequestsCountAgainstTheConcurrencyCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limits := NewLimits(ctx, &LimitsSettings{
		NetConnections: 2,
		Routes:         []*RouteLimitsSettings{{Path: "/connect/control"}},
	})
	addr := netip.MustParseAddr("198.51.100.11")
	now := time.Now()
	first, ok := limits.AcquireRouteRequest(addr, "/connect/control", now)
	if !ok {
		t.Fatal("first route request refused")
	}
	if _, ok := limits.AcquireRouteRequest(addr, "/hello", now); !ok {
		t.Fatal("a shared request under the cap refused")
	}
	if _, ok := limits.AcquireRouteRequest(addr, "/connect/control", now); ok {
		t.Fatal("a route request above the concurrency cap admitted")
	}
	first()
	if _, ok := limits.AcquireRouteRequest(addr, "/connect/control", now); !ok {
		t.Fatal("a route request under the cap again refused")
	}
}
