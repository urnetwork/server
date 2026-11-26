package api

// DESIGN-COMPUTE §14.3's api/compute routes' classification (CSRV-M12-04): eleven Client routes on the strict_exp
// allowlist and off the forwarding one, so a sidecar's token, the network's credential and any client token reach the
// mount, which resolves and narrows the caller, while a forwarding client's token and a lag.dev guest's are refused
// before it. Pure, as the other route table tests: tokens are signed with the local vault key and the handler is
// stubbed.

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// The api/compute routes as route_authz.go keys them, in §14.3's order.
var computeApiRouteKeys = []string{
	"GET /compute/balance",
	"POST /compute/checkout",
	"GET /compute/auto-renew",
	"POST /compute/auto-renew",
	"GET /compute/charges",
	"GET /compute/statement",
	"GET /compute/spend",
	"POST /compute/refund",
	"POST /compute/promo/redeem",
	"GET /compute/terms",
	"POST /compute/terms",
}

// The served routes by key.
func computeApiRoutesByKey(t *testing.T) map[string]*router.Route {
	t.Helper()
	routes := Routes()
	if problems := routeAccessProblems(routes, routeAccessByRoute); 0 < len(problems) {
		t.Fatalf("classify every route by credential:\n%s", strings.Join(problems, "\n"))
	}
	byKey := map[string]*router.Route{}
	for _, route := range routes {
		byKey[routeAccessKey(route)] = route
	}
	return byKey
}

// Each route is Client, on the strict_exp allowlist, off the forwarding one and every other list, and carries the
// gates that follow: the forwarding and guest refusals and the intended network screen, and neither the admin nor the
// role gate nor the proof gate.
func TestComputeApiRoutesAreClassified(t *testing.T) {
	byKey := computeApiRoutesByKey(t)
	for _, key := range computeApiRouteKeys {
		route := byKey[key]
		if route == nil {
			t.Fatalf("no route serves %s", key)
		}
		if routeAccessByRoute[key] != routeAccessClient {
			t.Errorf("%s: class %d, want Client", key, routeAccessByRoute[key])
		}
		if !strictExpRoutes[key] || forwardOnlyRoutes[key] || strictExpProofRoutes[key] || lagGuestRoutes[key] || credentialUnreadRoutes[key] {
			t.Errorf("%s: strict_exp %t, forwarding %t, proof %t, guest %t, credential unread %t", key,
				strictExpRoutes[key], forwardOnlyRoutes[key], strictExpProofRoutes[key], lagGuestRoutes[key], credentialUnreadRoutes[key])
		}
		if route.RefusesStrictExpCredentials() || !route.RefusesForwardOnlyCredentials() || route.RequiresStrictExpProof() {
			t.Errorf("%s: strict_exp gate %t, forwarding gate %t, proof gate %t", key,
				route.RefusesStrictExpCredentials(), route.RefusesForwardOnlyCredentials(), route.RequiresStrictExpProof())
		}
		if route.RefusesClientCredentials() || route.RequiresRolePermission() {
			t.Errorf("%s carries the admin or the role gate", key)
		}
		if !route.RefusesLagGuestCredentials() || !route.RefusesIntendedNetworkMismatch() || !route.RefusesWhileRestoreHeld() {
			t.Errorf("%s lacks the guest gate, the intended network screen or the restore hold", key)
		}
	}
	// and no other route under /compute/ but the public model routes
	for key := range byKey {
		_, path, _ := strings.Cut(key, " ")
		if strings.HasPrefix(path, "/compute/") && !slices.Contains(computeApiRouteKeys, key) && !slices.Contains(computeModelRouteKeys, key) {
			t.Errorf("%s is under /compute/ and is neither an api/compute route nor a model route", key)
		}
	}
}

// A sidecar's strict_exp token reaches every route's handler, as do the network's credential and an ordinary client
// token, which the mount then resolves or refuses; a forwarding client's token, its mark read, and a lag.dev guest's
// are refused before the handler, the first counted as the forwarding refusal.
func TestComputeApiRoutesFromAStrictExpToken(t *testing.T) {
	byKey := computeApiRoutesByKey(t)
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return false
	})
	strictTokens := newStrictExpTokens()
	forwardOnly := false
	setClientForwardOnly(t, func(ctx context.Context, networkId server.Id, clientId server.Id) (bool, error) {
		return forwardOnly && clientId == strictTokens.clientId, nil
	})
	accessTokens := newRouteAccessTokens()
	guest := session.NewByJwt(server.NewId(), server.NewId(), "lag-guest", false, false)
	guest.LagGuest = true
	guestToken := guest.Client(server.NewId(), server.NewId()).Testing_Sign()

	for _, key := range computeApiRouteKeys {
		route := byKey[key]
		for label, authorization := range map[string]string{
			"a sidecar's token":             "Bearer " + strictTokens.strictToken,
			"the network's token":           "Bearer " + accessTokens.networkToken,
			"an ordinary client's":          "Bearer " + accessTokens.clientToken,
			"an API key":                    "Bearer " + accessTokens.apiKey,
			"no credential, as sent":        "",
			"another client of its network": "Bearer " + strictTokens.clientToken,
		} {
			if w, reached := serveStubbed(route, authorization, nil); !reached || w.Code != http.StatusOK {
				t.Errorf("%s with %s: status = %d body = %q, handler ran = %t; want the handler", key, label, w.Code, w.Body.String(), reached)
			}
		}
		if w, reached := serveStubbed(route, "Bearer "+guestToken, nil); reached || w.Code != http.StatusForbidden {
			t.Errorf("%s with a lag.dev guest's token: status = %d, handler ran = %t; want the guest refusal", key, w.Code, reached)
		}
	}

	forwardOnly = true
	for _, key := range computeApiRouteKeys {
		before := childAdminCallCount(key, p0RefusalForwardOnly, p0ResultRefused)
		w, reached := serveStubbed(byKey[key], "Bearer "+strictTokens.strictToken, nil)
		if reached || w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.StrictExpCredentialRefusedMessage {
			t.Errorf("%s with a forwarding client's token: status = %d body = %q, handler ran = %t; want the forwarding refusal", key, w.Code, w.Body.String(), reached)
		}
		if after := childAdminCallCount(key, p0RefusalForwardOnly, p0ResultRefused); after != before+1 {
			t.Errorf("%s: counted %v forwarding refusals", key, after-before)
		}
	}
}
