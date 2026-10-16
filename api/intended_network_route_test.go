package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// The intended network gate on the route table (route_authz.go, AUTHZ1.md §10).
// Pure: tokens are signed with the fixture vault's key, every request reaches
// a stub handler at most, and no request carries an API key past the gate, so
// nothing reads the database or redis.

// the routes the owner's report (2026-10-16, a stale ur.io tab deleted another
// network) names: account deletion, and the money, credential and
// account-changing actions. Each must refuse a request meant for another
// network, whatever class it has; a route that moves to the public class
// fails here.
var intendedNetworkReportedRoutes = []string{
	// account deletion
	"POST /auth/network-delete",
	// subscription checkout, cancel and resume, the Stripe portal
	"POST /stripe/create-checkout-session",
	"POST /subscription/stripe/payment-sheet",
	"POST /stripe/payment-intent",
	"POST /subscription/cancel",
	"POST /subscription/resume",
	"POST /stripe/customer-portal",
	// USDC/Solana payment intents, balance codes, store and x402 purchases
	"POST /solana/payment-intent",
	"POST /solana/payment-transaction",
	"POST /subscription/redeem-balance-code",
	"POST /subscription/create-payment-id",
	"POST /subscription/verify-play-purchase",
	"POST /subscription/verify-apple-transaction",
	"POST /x402/purchase",
	"POST /wallet/circle-init",
	"POST /wallet/circle-transfer-out",
	// payout wallets
	"POST /account/payout-wallet",
	"POST /account/wallet",
	"POST /account/wallets/remove",
	"POST /account/wallets/verify-seeker",
	"POST /sn/wallet",
	"POST /sn/wallet/consent",
	"POST /sn/wallet/network-consent",
	"POST /sn/wallet/hotkey-consent",
	"POST /sn/wallet/hotkey-delegation",
	"POST /sn/head/binding",
	// sign-in methods, auth codes, API keys, network and client tokens, sessions
	"POST /auth/code-create",
	"POST /auth/add-auth",
	"POST /auth/remove-auth",
	"POST /auth/generate-seedphrase",
	"POST /auth/regenerate-seedphrase",
	"POST /auth/network-refresh",
	"POST /account/api-key",
	"POST /account/api-key/remove",
	"POST /network/revoke-session",
	"POST /network/revoke-other-sessions",
	// OAuth authorize and consent
	"POST /oauth/authorize",
	"POST /oauth/consent",
	// clients (proxy create is auth-client with a proxy config), referral,
	// blocked locations, the network's name and settings, devices
	"POST /network/auth-client",
	"POST /network/remove-client",
	"POST /network/remove-clients",
	"POST /account/set-referral",
	"GET /account/unlink-referral-network",
	"POST /network/block-location",
	"POST /network/unblock-location",
	"POST /network/user/update",
	"POST /account/change-name",
	"POST /account/claim-name",
	"POST /preferences/set-preferences",
	"POST /device/add",
	"POST /device/remove-association",
	"POST /device/set-name",
}

// serveIntendedNetwork serves one request through the route with a stub
// handler and reports the response and whether the handler ran.
func serveIntendedNetwork(route *router.Route, authorization string, intendedNetworkId *server.Id) (*httptest.ResponseRecorder, bool) {
	reached := false
	stubbed := router.Testing_WithHandler(route, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(route.Method(), routeAccessTestPath(route.Pattern()), strings.NewReader(`{}`))
	r.RemoteAddr = "198.51.100.44:4000"
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	if intendedNetworkId != nil {
		r.Header.Set("X-UR-Intended-Network-Id", intendedNetworkId.String())
	}
	w := httptest.NewRecorder()
	router.NewRouter(ctx, []*router.Route{stubbed}).ServeHTTP(w, r)
	return w, reached
}

func intendedNetworkRefusalCode(w *httptest.ResponseRecorder) string {
	var body struct {
		Code string `json:"code"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	return body.Code
}

// requireStaleScreenRefused requires the route to refuse the credentials of
// one network on a request meant for another, before its handler runs, and to
// serve that network's credential on a request meant for it and on a request
// that does not say (an older client).
func requireStaleScreenRefused(t *testing.T, route *router.Route, tokens *routeAccessTokens) {
	t.Helper()
	key := routeAccessKey(route)
	staleNetworkId := server.NewId()
	for name, token := range map[string]string{
		"network token": tokens.networkToken,
		"client token":  tokens.clientToken,
	} {
		w, reached := serveIntendedNetwork(route, "Bearer "+token, &staleNetworkId)
		if reached || w.Code != http.StatusPreconditionFailed || intendedNetworkRefusalCode(w) != "intended_network_mismatch" {
			t.Fatalf("%s: %s meant for another network: status = %d body = %q, handler ran = %t; want 412 intended_network_mismatch before the handler", key, name, w.Code, w.Body.String(), reached)
		}
	}
	if w, reached := serveIntendedNetwork(route, "Bearer "+tokens.networkToken, &tokens.networkId); !reached || w.Code != http.StatusOK {
		t.Fatalf("%s: network token meant for its network: status = %d, handler ran = %t; want the handler", key, w.Code, reached)
	}
	if w, reached := serveIntendedNetwork(route, "Bearer "+tokens.networkToken, nil); !reached || w.Code != http.StatusOK {
		t.Fatalf("%s: network token without the header: status = %d, handler ran = %t; want the handler", key, w.Code, reached)
	}
}

// Through every route that takes a URnetwork credential, a stale screen's
// request is refused before the handler runs, while the intended network's
// credential and a client that does not send the header are served. A public
// route does not act for the bearer's network, and ignores the header.
func TestAStaleScreenIsRefusedOnEveryCredentialRoute(t *testing.T) {
	tokens := newRouteAccessTokens()
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return false
	})
	checked := 0
	for _, route := range Routes() {
		if routeAccessFor(route) == routeAccessPublic {
			staleNetworkId := server.NewId()
			if w, reached := serveIntendedNetwork(route, "Bearer "+tokens.networkToken, &staleNetworkId); !reached || w.Code != http.StatusOK {
				t.Fatalf("%s (public): status = %d, handler ran = %t; want the handler", routeAccessKey(route), w.Code, reached)
			}
			continue
		}
		requireStaleScreenRefused(t, route, tokens)
		checked += 1
	}
	if checked == 0 {
		t.Fatal("no credential routes")
	}
}

// Each route the report names refuses a stale screen's request.
func TestTheReportedRoutesRefuseAStaleScreen(t *testing.T) {
	tokens := newRouteAccessTokens()
	setNetworkRefusesClientAdmin(t, func(ctx context.Context, networkId server.Id) bool {
		return false
	})
	routes := map[string]*router.Route{}
	for _, route := range Routes() {
		routes[routeAccessKey(route)] = route
	}
	for _, key := range intendedNetworkReportedRoutes {
		route := routes[key]
		if route == nil {
			t.Fatalf("%s is not a route", key)
		}
		if access := routeAccessFor(route); access == routeAccessPublic {
			t.Fatalf("%s is public; it acts for the bearer's network", key)
		}
		requireStaleScreenRefused(t, route, tokens)
	}
}
