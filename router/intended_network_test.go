package router

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The intended network gate (intended_network.go). Pure: tokens are signed
// with the fixture vault's key, and no request carries an API key past the
// header check, so nothing reads the database or redis.

// serveIntendedNetworkGate sends one request for path through a router holding
// only the route, with the intended network headers given, and reports the
// response and whether the route's handler ran.
func serveIntendedNetworkGate(t *testing.T, route *Route, path string, authorization string, intended ...string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	reached := false
	route = Testing_WithHandler(route, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	router := NewRouter(ctx, []*Route{route})
	r := httptest.NewRequest(route.Method(), path, strings.NewReader(`{}`))
	r.RemoteAddr = "198.51.100.43:4000"
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	for _, value := range intended {
		r.Header.Add("X-UR-Intended-Network-Id", value)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w, reached
}

// requireIntendedNetworkRefused requires the coded JSON refusal and that the
// handler never ran.
func requireIntendedNetworkRefused(t *testing.T, name string, w *httptest.ResponseRecorder, reached bool, status int, code string) {
	t.Helper()
	if w.Code != status || reached {
		t.Fatalf("%s: status = %d, handler ran = %t; want %d before the handler", name, w.Code, reached, status)
	}
	if contentType := w.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("%s: content type = %q", name, contentType)
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: body %q: %s", name, w.Body.String(), err)
	}
	if body.Code != code || body.Error == "" {
		t.Fatalf("%s: body = %+v, want code %s", name, body, code)
	}
}

// The defect: a stale screen still showing network A sends network B's
// credential. The gate answers 412 intended_network_mismatch before the
// handler runs, for B's network token and B's client token alike, with
// "Nothing was changed." in the message.
func TestIntendedNetworkGateRefusesAnotherNetworksCredential(t *testing.T) {
	fixture := newClientCredentialGateFixture()
	route := RefuseIntendedNetworkMismatch(NewRoute("POST", "/auth/network-delete", gateTestHandler))
	staleNetworkId := server.NewId()

	for name, token := range map[string]string{
		"network token": fixture.networkToken,
		"client token":  fixture.clientToken,
	} {
		w, reached := serveIntendedNetworkGate(t, route, "/auth/network-delete", "Bearer "+token, staleNetworkId.String())
		requireIntendedNetworkRefused(t, name, w, reached, http.StatusPreconditionFailed, "intended_network_mismatch")
		if !strings.Contains(w.Body.String(), "Nothing was changed.") {
			t.Fatalf("%s: body = %q", name, w.Body.String())
		}
	}
}

// The credential of the intended network reaches the handler.
func TestIntendedNetworkGateServesTheIntendedNetwork(t *testing.T) {
	fixture := newClientCredentialGateFixture()
	route := RefuseIntendedNetworkMismatch(NewRoute("POST", "/auth/network-delete", gateTestHandler))
	for name, token := range map[string]string{
		"network token": fixture.networkToken,
		"client token":  fixture.clientToken,
	} {
		if w, reached := serveIntendedNetworkGate(t, route, "/auth/network-delete", "Bearer "+token, fixture.networkId.String()); !reached || w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, handler ran = %t; want the handler", name, w.Code, reached)
		}
	}
}

// Backward compatible: without the header every request reaches the handler
// unchanged, whatever its credential, so a client that does not send the
// header is served exactly as before.
func TestIntendedNetworkGateServesEveryRequestWithoutTheHeader(t *testing.T) {
	fixture := newClientCredentialGateFixture()
	route := RefuseIntendedNetworkMismatch(NewRoute("POST", "/auth/network-delete", gateTestHandler))
	for name, authorization := range map[string]string{
		"network token": "Bearer " + fixture.networkToken,
		"client token":  "Bearer " + fixture.clientToken,
		"api key":       "Bearer urn_" + strings.Repeat("a", 52),
		"no credential": "",
		"other scheme":  "Basic " + base64.StdEncoding.EncodeToString([]byte("a:b")),
		"not a token":   "Bearer not.a.jwt",
	} {
		if w, reached := serveIntendedNetworkGate(t, route, "/auth/network-delete", authorization); !reached || w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, handler ran = %t; want the handler", name, w.Code, reached)
		}
	}
}

// A header that is not one network id is refused with 400: the client asked
// for a check that cannot be made.
func TestIntendedNetworkGateRefusesAMalformedHeader(t *testing.T) {
	fixture := newClientCredentialGateFixture()
	route := RefuseIntendedNetworkMismatch(NewRoute("POST", "/auth/network-delete", gateTestHandler))
	for name, values := range map[string][]string{
		"empty":       {""},
		"not an id":   {"network-a"},
		"zero id":     {server.Id{}.String()},
		"two headers": {fixture.networkId.String(), server.NewId().String()},
	} {
		w, reached := serveIntendedNetworkGate(t, route, "/auth/network-delete", "Bearer "+fixture.networkToken, values...)
		requireIntendedNetworkRefused(t, name, w, reached, http.StatusBadRequest, "invalid_intended_network_id")
	}
}

// A credential that does not verify is never refused by the gate, and neither
// is a request without one: the handler's authentication refuses them exactly
// as it would without the header.
func TestIntendedNetworkGateLeavesAnUnverifiedCredentialToTheHandler(t *testing.T) {
	fixture := newClientCredentialGateFixture()
	route := RefuseIntendedNetworkMismatch(NewRoute("POST", "/auth/network-delete", gateTestHandler))
	parts := strings.Split(fixture.networkToken, ".")
	forged := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 64)))
	for name, authorization := range map[string]string{
		"forged token":  "Bearer " + forged,
		"no credential": "",
		"not a token":   "Bearer not.a.jwt",
	} {
		if w, reached := serveIntendedNetworkGate(t, route, "/auth/network-delete", authorization, server.NewId().String()); !reached || w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, handler ran = %t; want the handler", name, w.Code, reached)
		}
	}
}

// In the order api.applyRouteAccess applies the gates, the intended network
// gate runs first: a request meant for another network is refused with 412,
// not with the client token gate's 403, which the apps take for a revoked
// sign-in. A request meant for the credential's network meets the other gates
// as before.
func TestIntendedNetworkGateRunsBeforeTheOtherGates(t *testing.T) {
	fixture := newClientCredentialGateFixture()
	route := RefuseIntendedNetworkMismatch(RefuseLagGuestCredentials(RefuseClientCredentials(NewRoute("POST", "/admin", gateTestHandler), RefuseEveryClientCredential)))

	w, reached := serveIntendedNetworkGate(t, route, "/admin", "Bearer "+fixture.clientToken, server.NewId().String())
	requireIntendedNetworkRefused(t, "client token, another network", w, reached, http.StatusPreconditionFailed, "intended_network_mismatch")

	w, reached = serveIntendedNetworkGate(t, route, "/admin", "Bearer "+fixture.clientToken, fixture.networkId.String())
	if w.Code != http.StatusForbidden || reached || strings.TrimSpace(w.Body.String()) != ClientCredentialRefusedMessage {
		t.Fatalf("client token, its own network: status = %d body = %q, handler ran = %t; want the client token refusal", w.Code, w.Body.String(), reached)
	}
	if w, reached := serveIntendedNetworkGate(t, route, "/admin", "Bearer "+fixture.networkToken, fixture.networkId.String()); !reached || w.Code != http.StatusOK {
		t.Fatalf("network token, its own network: status = %d, handler ran = %t; want the handler", w.Code, reached)
	}
}

// The gated copy keeps the route's identity, so the route table, the stats key
// and the streaming policy are unchanged, and the original route is not
// modified. A test handler keeps the gate.
func TestRefuseIntendedNetworkMismatchKeepsTheRoute(t *testing.T) {
	original := NewStreamingRoute("POST", "/upload/([^/]+)", gateTestHandler, StreamingBody{})
	refused := RefuseIntendedNetworkMismatch(original)
	if !refused.RefusesIntendedNetworkMismatch() || original.RefusesIntendedNetworkMismatch() {
		t.Fatalf("refused = %t, original = %t", refused.RefusesIntendedNetworkMismatch(), original.RefusesIntendedNetworkMismatch())
	}
	if refused.String() != original.String() || refused.Method() != "POST" || refused.Pattern() != "/upload/([^/]+)" {
		t.Fatalf("refused route = %s %s (%s), want the original's", refused.Method(), refused.Pattern(), refused.String())
	}
	if refused.streaming != original.streaming || !refused.captures {
		t.Fatal("the refused route lost the original's streaming policy or captures")
	}
	if refused.RefusesClientCredentials() || refused.RefusesLagGuestCredentials() {
		t.Fatal("the intended network gate added another gate")
	}

	fixture := newClientCredentialGateFixture()
	w, reached := serveIntendedNetworkGate(t, refused, "/upload/a", "Bearer "+fixture.networkToken, server.NewId().String())
	requireIntendedNetworkRefused(t, "test handler", w, reached, http.StatusPreconditionFailed, "intended_network_mismatch")
	if w, reached := serveIntendedNetworkGate(t, original, "/upload/a", "Bearer "+fixture.networkToken, server.NewId().String()); !reached || w.Code != http.StatusOK {
		t.Fatalf("original route: status = %d, handler ran = %t; want the handler", w.Code, reached)
	}
}

// The header name the gate reads is the session's.
func TestIntendedNetworkGateReadsTheSessionHeader(t *testing.T) {
	if session.IntendedNetworkIdHeader != "X-UR-Intended-Network-Id" {
		t.Fatalf("header = %q", session.IntendedNetworkIdHeader)
	}
}
