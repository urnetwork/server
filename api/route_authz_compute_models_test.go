package api

// The model routes' classification (CSRV-M11-01; DESIGN-COMPUTE §14.3, CO-49): four public routes that read no
// credential at all, so no gate in front of them reads a bearer, and a sidecar's token, a forwarding client's, a
// guest's, any other and none reach the handler alike. Pure, as the other route table tests: tokens are signed with
// the local vault key and the handler is stubbed.

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

// The model routes as route_authz.go keys them.
var computeModelRouteKeys = []string{
	"GET /compute/models/([^/]+)/index",
	"HEAD /compute/models/([^/]+)/index",
	"GET /compute/models/([^/]+)/chunks/([^/]+)",
	"HEAD /compute/models/([^/]+)/chunks/([^/]+)",
}

// TestEveryRouteIsClassified's guard holds with the four rows; each is public and reads no credential: it carries no
// gate that would read one, the restore hold alone, which reads none, and is on no credential's allowlist. Every
// credential and none reach the handler alike, a forwarding client's mark is never looked up, and no sidecar
// refusal is counted.
func TestComputeModelRoutesArePublic(t *testing.T) {
	routes := Routes()
	if problems := routeAccessProblems(routes, routeAccessByRoute); 0 < len(problems) {
		t.Fatalf("classify every route by credential:\n%s", strings.Join(problems, "\n"))
	}
	byKey := map[string]*router.Route{}
	for _, route := range routes {
		byKey[routeAccessKey(route)] = route
	}
	for _, key := range computeModelRouteKeys {
		route := byKey[key]
		if route == nil {
			t.Fatalf("no route serves %s", key)
		}
		if routeAccessByRoute[key] != routeAccessPublic || !credentialUnreadRoutes[key] {
			t.Fatalf("%s: class %d, credential unread %t", key, routeAccessByRoute[key], credentialUnreadRoutes[key])
		}
		if strictExpRoutes[key] || forwardOnlyRoutes[key] || lagGuestRoutes[key] || strictExpProofRoutes[key] {
			t.Fatalf("%s is on a credential's allowlist", key)
		}
		if route.RefusesClientCredentials() || route.RefusesStrictExpCredentials() || route.RefusesForwardOnlyCredentials() ||
			route.RequiresStrictExpProof() || route.RefusesLagGuestCredentials() || route.RefusesIntendedNetworkMismatch() {
			t.Fatalf("%s carries a gate that reads a credential", key)
		}
		if !route.RefusesWhileRestoreHeld() {
			t.Fatalf("%s is served through a restore hold", key)
		}
	}

	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return false
	})
	markLookupCount := 0
	setClientForwardOnly(t, func(ctx context.Context, networkId server.Id, clientId server.Id) (bool, error) {
		markLookupCount += 1
		return true, nil
	})
	strictTokens := newStrictExpTokens()
	accessTokens := newRouteAccessTokens()
	guest := session.NewByJwt(server.NewId(), server.NewId(), "lag-guest", false, false)
	guest.LagGuest = true
	guestToken := guest.Client(server.NewId(), server.NewId()).Testing_Sign()
	authorizations := []string{
		"",
		"Bearer " + strictTokens.strictToken,
		"Bearer " + strictTokens.clientToken,
		"Bearer " + accessTokens.networkToken,
		"Bearer " + accessTokens.apiKey,
		"Bearer " + guestToken,
		"Bearer not-a-token",
	}
	for _, key := range computeModelRouteKeys {
		before := childAdminCallCount(key, p0RefusalStrictExp, p0ResultRefused)
		for _, authorization := range authorizations {
			w, reached := serveStubbed(byKey[key], authorization, func(r *http.Request) {
				// a credential route's screen, which a public route ignores
				r.Header.Set("X-UR-Intended-Network-Id", server.NewId().String())
			})
			if !reached || w.Code != http.StatusOK {
				t.Fatalf("%s with %.24q: status = %d body = %q, handler ran = %t; want the handler", key, authorization, w.Code, w.Body.String(), reached)
			}
		}
		if after := childAdminCallCount(key, p0RefusalStrictExp, p0ResultRefused); after != before {
			t.Fatalf("%s counted %v sidecar refusals", key, after-before)
		}
	}
	if markLookupCount != 0 {
		t.Fatalf("a forwarding client's mark was looked up %d times", markLookupCount)
	}
}

// Every route credentialUnreadRoutes names is a route of the public class. Only the public class drops the gates: a
// route of another class listed by mistake keeps every gate, and a public route the set does not list keeps the
// strict_exp and guest gates every public route has.
func TestCredentialUnreadRoutesArePublic(t *testing.T) {
	served := map[string]bool{}
	for _, route := range Routes() {
		served[routeAccessKey(route)] = true
	}
	listed := []string{}
	for key := range credentialUnreadRoutes {
		listed = append(listed, key)
		if !served[key] {
			t.Errorf("credentialUnreadRoutes names %s, which no route serves", key)
		}
		if access := routeAccessByRoute[key]; access != routeAccessPublic {
			t.Errorf("%s reads no credential and is class %d, not public", key, access)
		}
	}
	slices.Sort(listed)
	want := slices.Clone(computeModelRouteKeys)
	slices.Sort(want)
	if !slices.Equal(listed, want) {
		t.Fatalf("credentialUnreadRoutes = %v, want the model routes %v", listed, want)
	}

	unclassified := router.NewRoute("POST", "/network/new-admin-route", func(w http.ResponseWriter, r *http.Request) {})
	key := routeAccessKey(unclassified)
	credentialUnreadRoutes[key] = true
	t.Cleanup(func() {
		delete(credentialUnreadRoutes, key)
	})
	gated := applyRouteAccess([]*router.Route{unclassified})[0]
	if !gated.RefusesClientCredentials() || !gated.RefusesStrictExpCredentials() || !gated.RefusesLagGuestCredentials() ||
		!gated.RefusesIntendedNetworkMismatch() {
		t.Fatal("a network route listed as reading no credential dropped its gates")
	}

	seed := applyRouteAccess([]*router.Route{router.NewRoute("GET", "/v1/seed\\.bin", func(w http.ResponseWriter, r *http.Request) {})})[0]
	if !seed.RefusesStrictExpCredentials() || !seed.RefusesLagGuestCredentials() {
		t.Fatal("a public route the set does not list dropped the strict_exp or guest gate")
	}
}
