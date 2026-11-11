package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// The P0 auth fix's route gates (route_authz.go; §7.4's stage 1, D-TEAM-14;
// §22.5's §7 row). Pure except where a test says: tokens are signed with the
// local vault key, the model's decisions are replaced, and no request reaches a
// handler.

// What sidecar.yml can say about M0's p0_enforce switch, none of which moves a
// refusal since M1: absent, off and on.
var p0EnforceConfigs = []string{
	"",
	"sidecar:\n  p0_enforce: {enabled: false}\n",
	"sidecar:\n  p0_enforce: {enabled: true}\n",
}

type strictExpTokens struct {
	networkId   server.Id
	clientId    server.Id
	strictToken string
	clientToken string
}

func newStrictExpTokens() *strictExpTokens {
	networkId := server.NewId()
	clientId := server.NewId()
	network := session.NewByJwt(networkId, server.NewId(), "strict-route-access", false, false)
	strict := network.Client(server.NewId(), clientId)
	strict.SetStrictExp(true)
	return &strictExpTokens{
		networkId:   networkId,
		clientId:    clientId,
		strictToken: strict.Testing_Sign(),
		clientToken: network.Client(server.NewId(), server.NewId()).Testing_Sign(),
	}
}

// Sends one request to the route behind its gates and reports whether its handler
// ran. The handler stands in for the route's own: its gates are what is being
// tested.
func serveStubbed(route *router.Route, authorization string, edit func(r *http.Request)) (*httptest.ResponseRecorder, bool) {
	reached := false
	stubbed := router.Testing_WithHandler(route, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(route.Method(), routeAccessTestPath(route.Pattern()), strings.NewReader(`{}`))
	r.RemoteAddr = "198.51.100.43:4000"
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	router.NewRouter(ctx, []*router.Route{stubbed}).ServeHTTP(w, r)
	return w, reached
}

// the 27 app admin and 10 own client payout routes (§7.4)
func clientAdminRoutes() []*router.Route {
	routes := []*router.Route{}
	for _, route := range Routes() {
		switch routeAccessFor(route) {
		case routeAccessAppAdmin, routeAccessOwnClientPayout:
			routes = append(routes, route)
		}
	}
	return routes
}

func childAdminCallCount(routeKey string, refusal string, result string) float64 {
	return testutil.ToFloat64(childAdminCallCounter.WithLabelValues(routeKey, refusal, result))
}

// Stage 1 enforced: a child or admin_refused client's token is refused with 403
// on each of the 37 routes, before its handler, and each refusal is counted,
// whatever sidecar.yml says about M0's switch or whether it is there at all.
func TestClientAdminRefusalIsEnforcedWhateverTheConfigSays(t *testing.T) {
	tokens := newRouteAccessTokens()
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return false
	})
	asked := map[server.Id]bool{}
	setClientRefusesAdmin(t, func(ctx context.Context, networkId server.Id, clientId server.Id) bool {
		asked[networkId] = true
		return true
	})

	routes := clientAdminRoutes()
	if len(routes) != 37 {
		t.Fatalf("the gated routes = %d, want §7.4's 37", len(routes))
	}
	authorization := "Bearer " + tokens.clientToken

	for _, config := range p0EnforceConfigs {
		func() {
			defer server.Testing_PushSidecarConfig(config)()
			for _, route := range routes {
				key := routeAccessKey(route)
				before := childAdminCallCount(key, p0RefusalClientAdmin, p0ResultRefused)
				w, reached := serveStubbed(route, authorization, nil)
				if reached || w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.ClientCredentialRefusedMessage {
					t.Fatalf("%q %s: status = %d body = %q, handler ran = %t; want 403 refused", config, key, w.Code, w.Body.String(), reached)
				}
				if after := childAdminCallCount(key, p0RefusalClientAdmin, p0ResultRefused); after != before+1 {
					t.Fatalf("%q %s: counted %v, want one", config, key, after-before)
				}
			}
		}()
	}
	if !asked[tokens.networkId] {
		t.Fatal("the client refusal was not asked about the token's network")
	}
}

// A legacy client keeps the routes: the shipped apps call them with such
// a token until stage 3, so it is neither refused nor counted.
func TestEnforcementLeavesALegacyClientItsAdminRoutes(t *testing.T) {
	tokens := newRouteAccessTokens()
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return false
	})

	for _, route := range clientAdminRoutes() {
		key := routeAccessKey(route)
		before := childAdminCallCount(key, p0RefusalClientAdmin, p0ResultRefused)
		w, reached := serveStubbed(route, "Bearer "+tokens.clientToken, nil)
		if !reached || w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, handler ran = %t; want the handler", key, w.Code, reached)
		}
		if after := childAdminCallCount(key, p0RefusalClientAdmin, p0ResultRefused); after != before {
			t.Fatalf("%s: counted a legacy client", key)
		}
	}
}

// What an Embed network already refuses it refuses on its own, and is not one of
// stage 1's refusals, so the count stays a measure of what stage 1 refuses.
func TestEmbedRefusalIsNotCountedAsStageOne(t *testing.T) {
	tokens := newRouteAccessTokens()
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return true
	})
	setClientRefusesAdmin(t, func(ctx context.Context, networkId server.Id, clientId server.Id) bool {
		t.Fatal("the client refusal was asked about a refused network")
		return true
	})

	for _, route := range clientAdminRoutes() {
		key := routeAccessKey(route)
		before := childAdminCallCount(key, p0RefusalClientAdmin, p0ResultRefused)
		if w, reached := serveStubbed(route, "Bearer "+tokens.clientToken, nil); reached || w.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, handler ran = %t; want 403", key, w.Code, reached)
		}
		if after := childAdminCallCount(key, p0RefusalClientAdmin, p0ResultRefused); after != before {
			t.Fatalf("%s: counted an Embed refusal", key)
		}
	}
}

// §7.4's list of the routes a strict_exp bearer reaches, as of the routes that
// exist. A route of §5-§21 joins it when it lands, and this copy with it.
var sidecarAllowlist = []string{
	"GET /auth/refresh",
	"POST /network/auth-client",
	"POST /network/remove-client",
	"GET /key/([^/]+)",
	"POST /connect/control",
	// §5.15's (CORE-M1-06)
	"POST /sidecar/bind",
	"GET /sidecar/identity/([^/]+)",
	"GET /sidecar/identity",
	// §8.9's (ACL-M2-12)
	"POST /acl/ack",
	// §16.6's cancel (MCP-M3-23)
	"POST /approval/cancel",
	// §6.8's sync routes (PROTO-M1-10)
	"GET /sidecar/sync/poll",
	"GET /sidecar/sync/versions",
	"GET /sidecar/sync/([^/]+)",
	// §20.7's limits (QUOTA-M1-03)
	"GET /sidecar/limits",
	// §18.7's wake token (SDK-M8-09)
	"POST /sidecar/push-token",
	// §18.8's unload beacon (TEAM-M1-10)
	"POST /sidecar/close",
	// §7.8's enrollment (TEAM-M1-04)
	"POST /network/enroll",
	// §17.16's share tables (BRIDGE-M6-02)
	"GET /network/bridge/share/([^/]+)",
	"PUT /network/bridge/share/([^/]+)",
	// §7.9's authorization log (TEAM-M2-13)
	"POST /network/config/delta",
	"POST /network/config/projection",
	"GET /network/config/projection",
}

// The route families §7.4 classifies sidecar. A route under one of these paths
// joins the allowlist, or is named below as refused.
var sidecarRouteFamilies = []string{
	"/sidecar/",
	"/network/config/",
	"/network/enroll",
	"/network/bridge/share/",
	"/acl/",
	"/approval/cancel",
	"/tls/",
	"/mail/",
	"/smtp/",
	"/socket/",
	"/dns/",
	"/abuse/status",
}

// The routes under those families whose own class keeps them refused (§7.4).
var sidecarRouteFamilyRefusals = []string{
	"/smtp/appeal",
	"/mail/direct-mx",
}

func strictExpProofStub(t *testing.T) {
	t.Helper()
	previous := clientProofPublicKey
	clientProofPublicKey = func(ctx context.Context, clientId server.Id) (ed25519.PublicKey, error) {
		return nil, model.ErrClientProofKeyNotBound
	}
	t.Cleanup(func() {
		clientProofPublicKey = previous
	})
}

// With the allowlist on, a strict_exp bearer reaches exactly §7.4's routes, with
// its proof where they want one, and every other route answers it 403, with its
// own message and before its handler. The check runs over the served route
// table, so a new route is covered without a change here: off the list it
// refuses, and under a sidecar family it fails until it is classified. Reads
// Redis for the proofs' jtis.
//
// §7.4 also has this test run §22.7's S1-S10 and an approval cancel with the
// allowlist on, so that a route a real flow calls and the list lacks fails here.
// That part is not built: the scenarios run on §5-§21's services, which land from
// M1 on, and it gates M1-out. Until then nothing here derives the list from what
// a sidecar calls; it checks the list against §7.4's text alone.
func TestSidecarTokenRoutes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		self := newStrictExpProofFixture(t)
		setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
			return false
		})

		listed := []string{}
		for key := range strictExpRoutes {
			listed = append(listed, key)
		}
		slices.Sort(listed)
		want := slices.Clone(sidecarAllowlist)
		slices.Sort(want)
		if !slices.Equal(listed, want) {
			t.Fatalf("strictExpRoutes = %v, want §7.4's %v", listed, want)
		}

		routes := Routes()
		served := map[string]bool{}
		for _, route := range routes {
			served[routeAccessKey(route)] = true
		}
		for _, key := range sidecarAllowlist {
			if !served[key] {
				t.Errorf("the allowlist names %s, which no route serves", key)
			}
		}

		authorization := "Bearer " + self.tokens.strictToken
		refusedCount := 0
		for _, route := range routes {
			key := routeAccessKey(route)
			if route.RefusesStrictExpCredentials() == strictExpRoutes[key] {
				t.Errorf("%s: strict_exp gate = %t, on the allowlist = %t", key, route.RefusesStrictExpCredentials(), strictExpRoutes[key])
			}
			for _, family := range sidecarRouteFamilies {
				if strings.HasPrefix(route.Pattern(), family) && !strictExpRoutes[key] && !slices.Contains(sidecarRouteFamilyRefusals, route.Pattern()) {
					t.Errorf("%s is under §7.4's sidecar family %s and off the allowlist", key, family)
				}
			}

			var proof func(r *http.Request)
			if strictExpProofRoutes[key] {
				proof = self.proof(route, self.privateKey)
			}
			w, reached := serveStubbed(route, authorization, proof)
			if strictExpRoutes[key] {
				if !reached || w.Code != http.StatusOK {
					t.Errorf("%s: status = %d body = %q, handler ran = %t; want the handler", key, w.Code, w.Body.String(), reached)
				}
				continue
			}
			refusedCount += 1
			if reached || w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.StrictExpCredentialRefusedMessage {
				t.Errorf("%s: status = %d body = %q, handler ran = %t; want the sidecar refusal", key, w.Code, w.Body.String(), reached)
			}
		}
		if refusedCount != len(routes)-len(sidecarAllowlist) {
			t.Fatalf("refused %d of %d routes", refusedCount, len(routes))
		}

		// an ordinary client's token is none of this: the gate looks at the mark
		for _, key := range []string{"GET /transfer/stats", "POST /stripe/payment-intent"} {
			route := routes[slices.IndexFunc(routes, func(route *router.Route) bool { return routeAccessKey(route) == key })]
			if w, reached := serveStubbed(route, "Bearer "+self.tokens.clientToken, nil); !reached || w.Code != http.StatusOK {
				t.Errorf("%s: client token status = %d, handler ran = %t; want the handler", key, w.Code, reached)
			}
		}
	})
}

// §7.4 names what the list leaves out: no payment, purchase, redemption, rename,
// account, stats, provider or extender route. The families are spelled out here so
// a route that moved onto the list by mistake fails by name.
func TestStrictExpTokenIsRefusedOnEveryPaymentRoute(t *testing.T) {
	tokens := newStrictExpTokens()
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return false
	})

	families := []string{
		"/pay/", "/stripe/", "/solana/", "/subscription/", "/x402/", "/wallet/", "/test/balance-",
		"/account/", "/stats/", "/network/provider-", "/network/extender-", "/network/find-",
		"/device/set-name", "/referral-code/", "/onboarding/", "/sn/wallet",
	}
	counted := map[string]int{}
	for _, route := range Routes() {
		family := ""
		for _, candidate := range families {
			if strings.HasPrefix(route.Pattern(), candidate) {
				family = candidate
			}
		}
		if family == "" {
			continue
		}
		counted[family] += 1
		key := routeAccessKey(route)
		w, reached := serveStubbed(route, "Bearer "+tokens.strictToken, nil)
		if reached || w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.StrictExpCredentialRefusedMessage {
			t.Errorf("%s: status = %d body = %q, handler ran = %t; want the sidecar refusal", key, w.Code, w.Body.String(), reached)
		}
	}
	for _, family := range families {
		if counted[family] == 0 {
			t.Errorf("no route under %s", family)
		}
	}
}

// Off its list a strict_exp token is refused and counted, whatever sidecar.yml
// says about M0's switch, on a route any client token reaches and on one that
// refuses every client token alike: the sidecar refusal is what it hears.
func TestStrictExpGateRefusesWhateverTheConfigSays(t *testing.T) {
	tokens := newStrictExpTokens()
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return false
	})

	routes := Routes()
	byKey := func(key string) *router.Route {
		return routes[slices.IndexFunc(routes, func(route *router.Route) bool { return routeAccessKey(route) == key })]
	}

	for _, config := range p0EnforceConfigs {
		func() {
			defer server.Testing_PushSidecarConfig(config)()
			for _, key := range []string{"GET /transfer/stats", "POST /stats/leaderboard", "POST /account/api-key"} {
				before := childAdminCallCount(key, p0RefusalStrictExp, p0ResultRefused)
				w, reached := serveStubbed(byKey(key), "Bearer "+tokens.strictToken, nil)
				if reached || w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.StrictExpCredentialRefusedMessage {
					t.Errorf("%q %s: status = %d body = %q, handler ran = %t; want the sidecar refusal", config, key, w.Code, w.Body.String(), reached)
				}
				if after := childAdminCallCount(key, p0RefusalStrictExp, p0ResultRefused); after != before+1 {
					t.Errorf("%q %s: counted %v, want one", config, key, after-before)
				}
			}
		}()
	}
}

// §7.4 admits a sidecar to POST /oauth/token for its exchange alone, which takes
// the client token in its form, never as a bearer, so a strict_exp bearer is
// refused there on every grant, the exchange's included.
func TestStrictExpTokenIsRefusedOnTheTokenRoute(t *testing.T) {
	tokens := newStrictExpTokens()
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return false
	})

	const key = "POST /oauth/token"
	routes := Routes()
	i := slices.IndexFunc(routes, func(route *router.Route) bool { return routeAccessKey(route) == key })
	if i < 0 {
		t.Fatalf("no route serves %s", key)
	}
	route := routes[i]
	if !route.RefusesStrictExpCredentials() {
		t.Fatalf("%s serves a strict_exp token", key)
	}
	for _, grantType := range []string{
		"authorization_code",
		"refresh_token",
		"urn:ietf:params:oauth:grant-type:token-exchange",
	} {
		before := childAdminCallCount(key, p0RefusalStrictExp, p0ResultRefused)
		w, reached := serveStubbed(route, "Bearer "+tokens.strictToken, func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader("grant_type=" + url.QueryEscape(grantType)))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		})
		if reached || w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.StrictExpCredentialRefusedMessage {
			t.Errorf("%s: status = %d body = %q, handler ran = %t; want the sidecar refusal", grantType, w.Code, w.Body.String(), reached)
		}
		if after := childAdminCallCount(key, p0RefusalStrictExp, p0ResultRefused); after != before+1 {
			t.Errorf("%s: counted %v, want one", grantType, after-before)
		}
	}
}

// An unclassified route is off the list too.
func TestAnUnclassifiedRouteRefusesAStrictExpToken(t *testing.T) {
	unclassified := router.NewRoute("POST", "/network/new-route", func(w http.ResponseWriter, r *http.Request) {})
	gated := applyRouteAccess([]*router.Route{unclassified})[0]
	if !gated.RefusesStrictExpCredentials() {
		t.Fatal("an unclassified route serves a strict_exp token")
	}
}

// Only the two routes a sidecar renews and mints on want a proof at the gate. The
// third §7.4 names, the exchange, checks its own (oauth/exchange.go).
func TestStrictExpProofRoutes(t *testing.T) {
	for _, route := range Routes() {
		key := routeAccessKey(route)
		if route.RequiresStrictExpProof() != strictExpProofRoutes[key] {
			t.Errorf("%s: proof gate = %t", key, route.RequiresStrictExpProof())
		}
		if strictExpProofRoutes[key] && !strictExpRoutes[key] {
			t.Errorf("%s wants a proof and is off the allowlist", key)
		}
	}
	for _, key := range []string{"GET /auth/refresh", "POST /network/auth-client"} {
		if !strictExpProofRoutes[key] {
			t.Errorf("%s wants no proof", key)
		}
	}
}

// A strict_exp client and the key it has bound.
type strictExpProofFixture struct {
	tokens     *strictExpTokens
	boundKey   ed25519.PublicKey
	privateKey ed25519.PrivateKey
	olderKey   ed25519.PrivateKey
	jti        int
}

func newStrictExpProofFixture(t *testing.T) *strictExpProofFixture {
	t.Helper()
	boundKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	server.Raise(err)
	_, olderKey, err := ed25519.GenerateKey(rand.Reader)
	server.Raise(err)
	self := &strictExpProofFixture{
		tokens:     newStrictExpTokens(),
		boundKey:   boundKey,
		privateKey: privateKey,
		olderKey:   olderKey,
	}
	previous := clientProofPublicKey
	clientProofPublicKey = func(ctx context.Context, clientId server.Id) (ed25519.PublicKey, error) {
		if clientId != self.tokens.clientId {
			return nil, model.ErrClientProofKeyNotBound
		}
		return self.boundKey, nil
	}
	t.Cleanup(func() {
		clientProofPublicKey = previous
	})
	return self
}

// A proof of the route's request, by the key given, under a jti of its own.
func (self *strictExpProofFixture) proof(route *router.Route, key ed25519.PrivateKey) func(r *http.Request) {
	self.jti += 1
	jti := fmt.Sprintf("jti-%d", self.jti)
	return func(r *http.Request) {
		url := session.Testing_DpopApiUrl(r.URL.Path)
		r.Header.Set(session.DpopHeader, session.Testing_SignDpopProof(key, r.Method, url, self.tokens.strictToken, jti, server.NowUtc()))
	}
}

func proofRoutes() []*router.Route {
	routes := []*router.Route{}
	for _, route := range Routes() {
		if route.RequiresStrictExpProof() {
			routes = append(routes, route)
		}
	}
	return routes
}

// §22.5's §7 row, through the gate: a token copied alone fails refresh and
// auth-client, as do a replayed proof and one by an older generation's key, and
// the key's own fresh proof passes. Every refusal is a 403, so the sdk does not
// sign out. Reads Redis for the jti.
func TestACopiedStrictExpTokenFailsWithoutItsKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		self := newStrictExpProofFixture(t)
		authorization := "Bearer " + self.tokens.strictToken

		routes := proofRoutes()
		if len(routes) != 2 {
			t.Fatalf("the proof routes = %d", len(routes))
		}
		for _, route := range routes {
			key := routeAccessKey(route)
			requireRefused := func(name string, edit func(r *http.Request)) {
				t.Helper()
				before := childAdminCallCount(key, p0RefusalProof, p0ResultRefused)
				w, reached := serveStubbed(route, authorization, edit)
				if reached || w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.StrictExpProofRefusedMessage {
					t.Fatalf("%s %s: status = %d body = %q, handler ran = %t; want the proof refused", key, name, w.Code, w.Body.String(), reached)
				}
				if after := childAdminCallCount(key, p0RefusalProof, p0ResultRefused); after != before+1 {
					t.Fatalf("%s %s: counted %v", key, name, after-before)
				}
			}

			requireRefused("copied token", nil)
			requireRefused("older generation's key", self.proof(route, self.olderKey))

			good := self.proof(route, self.privateKey)
			if w, reached := serveStubbed(route, authorization, good); !reached || w.Code != http.StatusOK {
				t.Fatalf("%s: good proof status = %d body = %q, handler ran = %t; want the handler", key, w.Code, w.Body.String(), reached)
			}
			requireRefused("replayed proof", good)
		}
	})
}

// With no key bound a proof cannot pass (§7.4), so the call is refused and
// counted, whatever sidecar.yml says. The network credential and an ordinary
// client token are never asked for a proof.
func TestStrictExpProofGateRefusesAClientWithNoBoundKey(t *testing.T) {
	tokens := newStrictExpTokens()
	strictExpProofStub(t)

	for _, config := range p0EnforceConfigs {
		func() {
			defer server.Testing_PushSidecarConfig(config)()
			for _, route := range proofRoutes() {
				key := routeAccessKey(route)
				before := childAdminCallCount(key, p0RefusalProof, p0ResultRefused)
				w, reached := serveStubbed(route, "Bearer "+tokens.strictToken, nil)
				if reached || w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.StrictExpProofRefusedMessage {
					t.Fatalf("%q %s: status = %d body = %q, handler ran = %t; want the proof refused", config, key, w.Code, w.Body.String(), reached)
				}
				if after := childAdminCallCount(key, p0RefusalProof, p0ResultRefused); after != before+1 {
					t.Fatalf("%q %s: counted %v, want one", config, key, after-before)
				}

				before = childAdminCallCount(key, p0RefusalProof, p0ResultRefused)
				if _, reached := serveStubbed(route, "Bearer "+tokens.clientToken, nil); !reached {
					t.Fatalf("%s: an ordinary client token was refused", key)
				}
				if after := childAdminCallCount(key, p0RefusalProof, p0ResultRefused); after != before {
					t.Fatalf("%s: an ordinary client token was counted", key)
				}
			}
		}()
	}
}

// M0 has no bound keys, so the real lookup answers none and a proof, even a
// well-formed one, cannot pass: the check fails closed until MG1 supplies keys.
func TestClientProofKeyFailsClosedBeforeMg1(t *testing.T) {
	boundKey, err := model.ClientProofPublicKey(context.Background(), server.NewId())
	if boundKey != nil || err == nil {
		t.Fatalf("key = %v, err = %v; want none bound", boundKey, err)
	}
}
