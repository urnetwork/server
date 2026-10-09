package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/router"
)

// The route table guard (route_authz.go, AUTHZ1.md). Pure: tokens are signed
// with the local vault key, and no request reaches a handler that would read
// the database or redis.

// routeAccessProblems lists every route with no classification, every route
// classified twice, and every classification with no route.
func routeAccessProblems(routes []*router.Route, table map[string]routeAccess) []string {
	problems := []string{}
	seen := map[string]bool{}
	for _, route := range routes {
		key := routeAccessKey(route)
		if seen[key] {
			problems = append(problems, fmt.Sprintf("%s is registered twice", key))
		}
		seen[key] = true
		if _, ok := table[key]; !ok {
			problems = append(problems, fmt.Sprintf("%s has no routeAccessByRoute entry", key))
		}
	}
	for key := range table {
		if !seen[key] {
			problems = append(problems, fmt.Sprintf("routeAccessByRoute entry %s has no route", key))
		}
	}
	slices.Sort(problems)
	return problems
}

// Every route is classified, and every classification names a route. A new
// route fails here until it is added to routeAccessByRoute.
func TestEveryRouteIsClassified(t *testing.T) {
	if problems := routeAccessProblems(Routes(), routeAccessByRoute); 0 < len(problems) {
		t.Fatalf("classify every route by credential (route_authz.go, AUTHZ1.md):\n%s", strings.Join(problems, "\n"))
	}
	for key, access := range routeAccessByRoute {
		if access < routeAccessPublic || routeAccessNetwork < access {
			t.Fatalf("%s has no access class (%d)", key, access)
		}
	}
}

// The guard catches a new route, and until it is classified the route refuses
// client tokens.
func TestAnUnclassifiedRouteFailsTheGuardAndRefusesClientTokens(t *testing.T) {
	unclassified := router.NewRoute("POST", "/network/new-admin-route", func(w http.ResponseWriter, r *http.Request) {})
	problems := routeAccessProblems(append(Routes(), unclassified), routeAccessByRoute)
	if !slices.Equal(problems, []string{"POST /network/new-admin-route has no routeAccessByRoute entry"}) {
		t.Fatalf("problems = %v", problems)
	}
	if !applyRouteAccess([]*router.Route{unclassified})[0].RefusesClientCredentials() {
		t.Fatal("an unclassified route serves client tokens")
	}

	// and a stale entry is caught too
	table := map[string]routeAccess{"GET /no/such/route": routeAccessClient}
	for key, access := range routeAccessByRoute {
		table[key] = access
	}
	if problems := routeAccessProblems(Routes(), table); !slices.Equal(problems, []string{"routeAccessByRoute entry GET /no/such/route has no route"}) {
		t.Fatalf("problems = %v", problems)
	}
}

// routeAccessGated reports whether a class carries the client token gate: the
// network-only routes refuse every client token, and the app admin and own
// client payout routes refuse an Embed network's.
func routeAccessGated(access routeAccess) bool {
	switch access {
	case routeAccessNetwork, routeAccessAppAdmin, routeAccessOwnClientPayout:
		return true
	default:
		return false
	}
}

// Exactly the admin routes and the own client payout routes carry the client
// token gate.
func TestOnlyAdminRoutesCarryTheGate(t *testing.T) {
	for _, route := range Routes() {
		access := routeAccessFor(route)
		gated := routeAccessGated(access)
		if route.RefusesClientCredentials() != gated {
			t.Fatalf("%s (access %d): gate = %t, want %t", routeAccessKey(route), access, route.RefusesClientCredentials(), gated)
		}
	}
}

// routeAccessTestPath is a request path the pattern matches.
func routeAccessTestPath(pattern string) string {
	path := strings.ReplaceAll(pattern, "([^/]+)", "x")
	return strings.ReplaceAll(path, `\.`, ".")
}

type routeAccessTokens struct {
	networkId    server.Id
	networkToken string
	clientToken  string
	apiKey       string
}

func newRouteAccessTokens() *routeAccessTokens {
	networkId := server.NewId()
	network := jwt.NewByJwt(networkId, server.NewId(), "route-access", false, false)
	return &routeAccessTokens{
		networkId:    networkId,
		networkToken: network.Sign(),
		clientToken:  network.Client(server.NewId(), server.NewId()).Sign(),
		// the gate reads only the prefix; the handler looks the key up
		apiKey: "urn_" + strings.Repeat("k", 52),
	}
}

func serveRouteAccess(handler http.Handler, method string, path string, authorization string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	r.RemoteAddr = "198.51.100.41:4000"
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// setNetworkRefusesClientAdmin replaces the Embed decision for one test.
func setNetworkRefusesClientAdmin(t *testing.T, refuses func(ctx context.Context, networkId server.Id) bool) {
	previous := networkRefusesClientAdmin
	networkRefusesClientAdmin = refuses
	t.Cleanup(func() {
		networkRefusesClientAdmin = previous
	})
}

// Through the router the API serves, every network-only route answers a
// client token with 403 before its handler, and so does every app admin and
// own client payout route for a network whose client tokens are refused (the
// Embed plan).
func TestAdminRoutesRefuseAClientTokenThroughTheRouter(t *testing.T) {
	tokens := newRouteAccessTokens()
	refusedNetworks := map[server.Id]bool{}
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		refusedNetworks[networkId] = true
		return true
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	apiRouter := router.NewRouter(ctx, Routes())

	adminCount := 0
	for _, route := range Routes() {
		access := routeAccessFor(route)
		if !routeAccessGated(access) {
			continue
		}
		adminCount += 1
		w := serveRouteAccess(apiRouter, route.Method(), routeAccessTestPath(route.Pattern()), "Bearer "+tokens.clientToken)
		if w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.ClientCredentialRefusedMessage {
			t.Fatalf("%s: client token status = %d body = %q, want 403 refused", routeAccessKey(route), w.Code, w.Body.String())
		}
	}
	if adminCount == 0 {
		t.Fatal("no admin routes")
	}
	if !refusedNetworks[tokens.networkId] {
		t.Fatal("the app admin refusal was not asked about the token's network")
	}
}

// Every gated route serves the network credential, a root token or an API
// key, and a request with no credential, exactly as before: the gate passes
// them to the handler, whose authentication decides.
func TestAdminRoutesServeTheNetworkCredential(t *testing.T) {
	tokens := newRouteAccessTokens()
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return true
	})

	for _, route := range Routes() {
		access := routeAccessFor(route)
		if !routeAccessGated(access) {
			continue
		}
		reached := false
		stubbed := router.Testing_WithHandler(route, func(w http.ResponseWriter, r *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})
		ctx, cancel := context.WithCancel(context.Background())
		routeRouter := router.NewRouter(ctx, []*router.Route{stubbed})
		for name, authorization := range map[string]string{
			"root token":    "Bearer " + tokens.networkToken,
			"api key":       "Bearer " + tokens.apiKey,
			"no credential": "",
		} {
			reached = false
			w := serveRouteAccess(routeRouter, route.Method(), routeAccessTestPath(route.Pattern()), authorization)
			if !reached || w.Code != http.StatusOK {
				t.Fatalf("%s: %s status = %d, handler ran = %t; want the handler", routeAccessKey(route), name, w.Code, reached)
			}
		}
		reached = false
		if w := serveRouteAccess(routeRouter, route.Method(), routeAccessTestPath(route.Pattern()), "Bearer "+tokens.clientToken); reached || w.Code != http.StatusForbidden {
			t.Fatalf("%s: client token status = %d, handler ran = %t; want 403", routeAccessKey(route), w.Code, reached)
		}
		cancel()
	}
}

// The app admin routes keep serving the URnetwork apps' client tokens on a
// network that does not refuse them, and the own client payout routes keep
// serving the provider apps', while the network-only routes refuse every
// client token.
func TestAppAdminRoutesServeAClientTokenOfAnOrdinaryNetwork(t *testing.T) {
	tokens := newRouteAccessTokens()
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return false
	})

	for _, route := range Routes() {
		access := routeAccessFor(route)
		if !routeAccessGated(access) {
			continue
		}
		reached := false
		stubbed := router.Testing_WithHandler(route, func(w http.ResponseWriter, r *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})
		ctx, cancel := context.WithCancel(context.Background())
		w := serveRouteAccess(router.NewRouter(ctx, []*router.Route{stubbed}), route.Method(), routeAccessTestPath(route.Pattern()), "Bearer "+tokens.clientToken)
		cancel()
		switch access {
		case routeAccessAppAdmin, routeAccessOwnClientPayout:
			if !reached || w.Code != http.StatusOK {
				t.Fatalf("%s: client token status = %d, handler ran = %t; want the handler", routeAccessKey(route), w.Code, reached)
			}
		case routeAccessNetwork:
			if reached || w.Code != http.StatusForbidden {
				t.Fatalf("%s: client token status = %d, handler ran = %t; want 403", routeAccessKey(route), w.Code, reached)
			}
		}
	}
}

// Every other route serves a client token: the model limits the own-client
// routes, and the rest act for the caller or need no credential.
func TestNonAdminRoutesServeAClientToken(t *testing.T) {
	tokens := newRouteAccessTokens()
	for _, route := range Routes() {
		access := routeAccessFor(route)
		if routeAccessGated(access) {
			continue
		}
		reached := false
		stubbed := router.Testing_WithHandler(route, func(w http.ResponseWriter, r *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})
		ctx, cancel := context.WithCancel(context.Background())
		w := serveRouteAccess(router.NewRouter(ctx, []*router.Route{stubbed}), route.Method(), routeAccessTestPath(route.Pattern()), "Bearer "+tokens.clientToken)
		cancel()
		if !reached || w.Code != http.StatusOK {
			t.Fatalf("%s: client token status = %d, handler ran = %t; want the handler", routeAccessKey(route), w.Code, reached)
		}
	}
}

// The routes that hand out a network credential, or the means to sign in as
// the network, are admin routes: while any of them served a client token, a
// client token could become the network and every other refusal would be
// moot. The URnetwork apps call the sign-in ones with their client token, so
// those wait on decision 1 in AUTHZ1.md for an ordinary network.
func TestCredentialMintingRoutesAreAdminRoutes(t *testing.T) {
	for key, want := range map[string]routeAccess{
		"POST /account/api-key":            routeAccessNetwork,
		"POST /oauth/authorize":            routeAccessNetwork,
		"POST /device/add":                 routeAccessNetwork,
		"POST /network/remove-clients":     routeAccessNetwork,
		"POST /auth/code-create":           routeAccessAppAdmin,
		"POST /auth/add-auth":              routeAccessAppAdmin,
		"POST /auth/remove-auth":           routeAccessAppAdmin,
		"POST /auth/generate-seedphrase":   routeAccessAppAdmin,
		"POST /auth/regenerate-seedphrase": routeAccessAppAdmin,
		"POST /auth/network-delete":        routeAccessAppAdmin,
		"POST /network/auth-client":        routeAccessOwnClient,
		"POST /network/remove-client":      routeAccessOwnClient,
	} {
		if got := routeAccessByRoute[key]; got != want {
			t.Fatalf("%s access = %d, want %d", key, got, want)
		}
	}
}

// The provider payout of a client's own client, its subnet wallet mapping, the
// mapping's consent, the wallet read and its fleet binding, is refused to the
// client token of an Embed network and served to an ordinary network's, which
// the provider apps send (AUTHZ1.md, decision 3). The network-wide payout
// routes stay network only, and the network credential reaches every one.
func TestOwnClientPayoutRoutesRefuseOnlyAnEmbedNetworksClientToken(t *testing.T) {
	payoutRoutes := []string{
		"POST /sn/wallet",
		"POST /sn/wallet/consent",
		"GET /sn/wallet",
		"POST /sn/head/binding",
	}
	for key, access := range routeAccessByRoute {
		if (access == routeAccessOwnClientPayout) != slices.Contains(payoutRoutes, key) {
			t.Fatalf("%s access = %d; the own client payout routes are %v", key, access, payoutRoutes)
		}
	}
	for _, key := range []string{
		"POST /sn/wallet/network-consent",
		"POST /sn/wallet/hotkey-consent",
		"POST /sn/wallet/hotkey-delegation",
		"POST /account/payout-wallet",
	} {
		if access := routeAccessByRoute[key]; access != routeAccessNetwork && access != routeAccessAppAdmin {
			t.Fatalf("%s access = %d, want an admin class", key, access)
		}
	}

	tokens := newRouteAccessTokens()
	embedNetworks := map[server.Id]bool{}
	askedNetworks := map[server.Id]bool{}
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		askedNetworks[networkId] = true
		return embedNetworks[networkId]
	})

	served := 0
	for _, route := range Routes() {
		if routeAccessFor(route) != routeAccessOwnClientPayout {
			continue
		}
		served += 1
		reached := false
		stubbed := router.Testing_WithHandler(route, func(w http.ResponseWriter, r *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})
		ctx, cancel := context.WithCancel(context.Background())
		routeRouter := router.NewRouter(ctx, []*router.Route{stubbed})
		path := routeAccessTestPath(route.Pattern())

		// an ordinary network: the provider app's client token reaches the handler
		embedNetworks[tokens.networkId] = false
		reached = false
		if w := serveRouteAccess(routeRouter, route.Method(), path, "Bearer "+tokens.clientToken); !reached || w.Code != http.StatusOK {
			t.Fatalf("%s: client token of an ordinary network status = %d, handler ran = %t; want the handler", routeAccessKey(route), w.Code, reached)
		}

		// an Embed network: 403 before the handler, whose credential still works
		embedNetworks[tokens.networkId] = true
		reached = false
		w := serveRouteAccess(routeRouter, route.Method(), path, "Bearer "+tokens.clientToken)
		if reached || w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.ClientCredentialRefusedMessage {
			t.Fatalf("%s: client token of an Embed network status = %d body = %q, handler ran = %t; want 403 refused", routeAccessKey(route), w.Code, w.Body.String(), reached)
		}
		for name, authorization := range map[string]string{
			"root token": "Bearer " + tokens.networkToken,
			"api key":    "Bearer " + tokens.apiKey,
		} {
			reached = false
			if w := serveRouteAccess(routeRouter, route.Method(), path, authorization); !reached || w.Code != http.StatusOK {
				t.Fatalf("%s: %s of an Embed network status = %d, handler ran = %t; want the handler", routeAccessKey(route), name, w.Code, reached)
			}
		}
		cancel()
	}
	if served != len(payoutRoutes) {
		t.Fatalf("served %d own client payout routes, want %d", served, len(payoutRoutes))
	}
	if !askedNetworks[tokens.networkId] {
		t.Fatal("the payout refusal was not asked about the token's network")
	}
}

// The spec (connect/api/bringyour.yml) documents the gate's 403 on exactly the
// gated routes, with the shared response of the route's class:
// ClientJwtRefused on the network-only routes, EmbedClientJwtRefused on the
// app admin and own client payout routes. A route that changes class fails
// here until its documented 403 changes with it. Both shared responses carry
// the gate's message.
func TestSpecDocumentsTheClientTokenRefusal(t *testing.T) {
	spec := loadSpec(t)
	const clientJwtRefused = "#/components/responses/ClientJwtRefused"
	const embedClientJwtRefused = "#/components/responses/EmbedClientJwtRefused"

	sharedResponses := asMap(asMap(spec.root["components"])["responses"])
	for _, ref := range []string{clientJwtRefused, embedClientJwtRefused} {
		name := ref[strings.LastIndex(ref, "/")+1:]
		example, _ := asMap(asMap(asMap(sharedResponses[name])["content"])["text/plain"])["example"].(string)
		if example != router.ClientCredentialRefusedMessage {
			t.Fatalf("%s example = %q, want the gate's message %q", name, example, router.ClientCredentialRefusedMessage)
		}
	}

	specOperations := map[string]map[string]any{}
	for path, methods := range spec.paths {
		for method, operation := range asMap(methods) {
			specOperations[strings.ToUpper(method)+" "+normalizePath(path)] = asMap(operation)
		}
	}
	documentedCount := 0
	for _, route := range Routes() {
		access := routeAccessFor(route)
		want := ""
		switch access {
		case routeAccessNetwork:
			want = clientJwtRefused
		case routeAccessAppAdmin, routeAccessOwnClientPayout:
			want = embedClientJwtRefused
		}
		operation := specOperations[route.Method()+" "+normalizePath(route.Pattern())]
		if operation == nil {
			if want != "" {
				t.Errorf("%s (access %d) is gated and not in the spec", routeAccessKey(route), access)
			}
			continue
		}
		ref, _ := asMap(asMap(operation["responses"])["403"])["$ref"].(string)
		if ref != want {
			t.Errorf("%s (access %d): the spec's 403 is %q, want %q", routeAccessKey(route), access, ref, want)
		}
		if want != "" {
			documentedCount += 1
		}
	}
	if documentedCount == 0 {
		t.Fatal("no gated route documents the refusal")
	}
}
