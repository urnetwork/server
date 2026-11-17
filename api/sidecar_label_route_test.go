package api

// GET /sidecar/label in the route table (§12.5; owner decision 421, QD-935): Client class on §7.4's strict_exp
// allowlist, off the forwarding list, the proof routes and the lag.dev guest's. Through its gates with the handler
// stubbed, as route_authz_sidecar_test.go serves routes: a sidecar's token, a legacy client's token, the network
// credential and an API key reach the handler, an Embed network's client token too, and a forwarding client's token is
// refused 403 before it, counted as forward_only_route. Pure: tokens are signed with the local vault key and the
// model's decisions are replaced.

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

func TestSidecarLabelRouteTakesItsClass(t *testing.T) {
	const key = "GET /sidecar/label"
	if access := routeAccessByRoute[key]; access != routeAccessClient {
		t.Fatalf("%s is class %d, want Client", key, access)
	}
	if !strictExpRoutes[key] || forwardOnlyRoutes[key] || strictExpProofRoutes[key] || lagGuestRoutes[key] {
		t.Fatalf("%s: strict_exp %t, forwarding %t, proof %t, guest %t", key, strictExpRoutes[key], forwardOnlyRoutes[key], strictExpProofRoutes[key], lagGuestRoutes[key])
	}
	routes := Routes()
	index := slices.IndexFunc(routes, func(route *router.Route) bool { return routeAccessKey(route) == key })
	if index < 0 {
		t.Fatalf("no route serves %s", key)
	}
	route := routes[index]
	if route.RefusesClientCredentials() || route.RefusesStrictExpCredentials() || route.RequiresStrictExpProof() ||
		!route.RefusesForwardOnlyCredentials() || !route.RefusesIntendedNetworkMismatch() {
		t.Fatalf("%s carries the wrong gates", key)
	}

	sidecarTokens := newStrictExpTokens()
	forwardingTokens := newStrictExpTokens()
	tokens := newRouteAccessTokens()
	// the Client class asks no Embed decision, so even a network that refuses its client tokens administration
	// keeps this route for them
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return true
	})
	setClientForwardOnly(t, func(ctx context.Context, networkId server.Id, clientId server.Id) (bool, error) {
		return clientId == forwardingTokens.clientId, nil
	})
	for _, served := range []struct {
		name          string
		authorization string
	}{
		{name: "a sidecar's token", authorization: "Bearer " + sidecarTokens.strictToken},
		{name: "a legacy client's token", authorization: "Bearer " + tokens.clientToken},
		{name: "the network credential", authorization: "Bearer " + tokens.networkToken},
		{name: "an API key", authorization: "Bearer " + tokens.apiKey},
	} {
		if w, reached := serveStubbed(route, served.authorization, nil); !reached || w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, handler ran = %t; want the handler", served.name, w.Code, reached)
		}
	}

	before := childAdminCallCount(key, p0RefusalForwardOnly, p0ResultRefused)
	w, reached := serveStubbed(route, "Bearer "+forwardingTokens.strictToken, nil)
	if reached || w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.StrictExpCredentialRefusedMessage {
		t.Fatalf("a forwarding client's token: status = %d body = %q, handler ran = %t", w.Code, w.Body.String(), reached)
	}
	if after := childAdminCallCount(key, p0RefusalForwardOnly, p0ResultRefused); after != before+1 {
		t.Fatalf("counted %v forwarding refusals", after-before)
	}
}
