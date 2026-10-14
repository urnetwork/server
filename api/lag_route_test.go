package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// A lag.dev guest's client token through the routes the API serves
// (lag.dev/PLAN.md A1, session/AUTH.md §1). Pure, as the other route table
// tests: tokens are signed with the local vault key and no handler runs.

// The guest's token reaches only the guest's two routes. Every other route
// refuses it with 403 before its handler runs, whatever its class: a public
// route, a client route, an own client route and every admin route alike.
func TestLagGuestTokenServesOnlyTheLagRoutes(t *testing.T) {
	guest := session.NewByJwt(server.NewId(), server.NewId(), "lag-guest", false, false)
	guest.LagGuest = true
	guestToken := guest.Client(server.NewId(), server.NewId()).Testing_Sign()
	// as an ordinary network, so an app admin route's own gate passes it on
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return false
	})

	served := []string{}
	for _, route := range Routes() {
		key := routeAccessKey(route)
		if route.RefusesLagGuestCredentials() == lagGuestRoutes[key] {
			t.Fatalf("%s: lag guest gate = %t", key, route.RefusesLagGuestCredentials())
		}
		reached := false
		stubbed := router.Testing_WithHandler(route, func(w http.ResponseWriter, r *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})
		ctx, cancel := context.WithCancel(context.Background())
		w := serveRouteAccess(router.NewRouter(ctx, []*router.Route{stubbed}), route.Method(), routeAccessTestPath(route.Pattern()), "Bearer "+guestToken)
		cancel()
		if lagGuestRoutes[key] {
			if !reached || w.Code != http.StatusOK {
				t.Fatalf("%s: the guest's token status = %d, handler ran = %t; want the handler", key, w.Code, reached)
			}
			served = append(served, key)
		} else if reached || w.Code != http.StatusForbidden {
			t.Fatalf("%s: the guest's token status = %d, handler ran = %t; want 403", key, w.Code, reached)
		}
	}
	connect.AssertEqual(t, len(served), len(lagGuestRoutes))

	// the guest's own routes are Client routes, and the guest's session is
	// created without a credential
	for key := range lagGuestRoutes {
		connect.AssertEqual(t, routeAccessByRoute[key], routeAccessClient)
	}
}
