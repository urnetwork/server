package router

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// The client token gate (client_credential.go). Pure: tokens are signed with
// the local vault key and nothing reads the database or redis.

type clientCredentialGateFixture struct {
	networkId    server.Id
	networkToken string
	clientId     server.Id
	clientToken  string
}

func newClientCredentialGateFixture() *clientCredentialGateFixture {
	networkId := server.NewId()
	userId := server.NewId()
	clientId := server.NewId()
	network := session.NewByJwt(networkId, userId, "gate-test", false, false)
	return &clientCredentialGateFixture{
		networkId:    networkId,
		networkToken: network.Testing_Sign(),
		clientId:     clientId,
		clientToken:  network.Client(server.NewId(), clientId).Testing_Sign(),
	}
}

// serveClientCredentialGate sends one request for path through a router
// holding only the route, and reports the response and whether the route's
// handler ran.
func serveClientCredentialGate(t *testing.T, route *Route, path string, authorization string) (*httptest.ResponseRecorder, bool) {
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
	r.RemoteAddr = "198.51.100.40:4000"
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w, reached
}

func gateTestHandler(w http.ResponseWriter, r *http.Request) {}

// A refused route answers a client token with 403 and the refusal message,
// and its handler never runs. Every other credential, and no credential,
// reaches the handler unchanged: the handler's authentication still decides
// those, so a missing token stays its 401.
func TestRefuseClientCredentialsGate(t *testing.T) {
	fixture := newClientCredentialGateFixture()
	route := RefuseClientCredentials(NewRoute("POST", "/admin", gateTestHandler), RefuseEveryClientCredential)

	w, reached := serveClientCredentialGate(t, route, "/admin", "Bearer "+fixture.clientToken)
	if w.Code != http.StatusForbidden || reached {
		t.Fatalf("client token: status = %d, handler ran = %t; want 403 before the handler", w.Code, reached)
	}
	if body := strings.TrimSpace(w.Body.String()); body != ClientCredentialRefusedMessage {
		t.Fatalf("client token: body = %q", body)
	}

	for name, authorization := range map[string]string{
		"network token": "Bearer " + fixture.networkToken,
		// an API key authenticates as the network; its lookup is the handler's
		"api key":       "Bearer urn_" + strings.Repeat("a", 52),
		"no credential": "",
		"other scheme":  "Basic " + base64.StdEncoding.EncodeToString([]byte("a:b")),
		"not a token":   "Bearer not.a.jwt",
	} {
		w, reached := serveClientCredentialGate(t, route, "/admin", authorization)
		if !reached || w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, handler ran = %t; want the handler", name, w.Code, reached)
		}
	}
}

// A token whose signature does not verify is never refused by the gate: the
// handler refuses it exactly as it would without the gate. The gate decides
// only on a verified client token.
func TestRefuseClientCredentialsGateLeavesAnUnverifiedTokenToTheHandler(t *testing.T) {
	fixture := newClientCredentialGateFixture()
	route := RefuseClientCredentials(NewRoute("POST", "/admin", gateTestHandler), RefuseEveryClientCredential)

	parts := strings.Split(fixture.clientToken, ".")
	if len(parts) != 3 {
		t.Fatalf("client token has %d parts", len(parts))
	}
	// the same claims under a different signature
	forged := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 64)))
	if !session.ByJwtNamesClientUnverified(forged) {
		t.Fatal("the forged token's claims should name a client")
	}
	w, reached := serveClientCredentialGate(t, route, "/admin", "Bearer "+forged)
	if !reached || w.Code != http.StatusOK {
		t.Fatalf("forged token: status = %d, handler ran = %t; want the handler", w.Code, reached)
	}
}

// A conditional refusal sees the verified token and refuses only what it
// decides to.
func TestRefuseClientCredentialsGateAsksTheRefusal(t *testing.T) {
	fixture := newClientCredentialGateFixture()
	var seen *session.ByJwt
	refusedNetworkId := fixture.networkId
	route := RefuseClientCredentials(NewRoute("POST", "/admin", gateTestHandler), func(ctx context.Context, byJwt *session.ByJwt) bool {
		seen = byJwt
		return byJwt.NetworkId == refusedNetworkId
	})

	w, reached := serveClientCredentialGate(t, route, "/admin", "Bearer "+fixture.clientToken)
	if w.Code != http.StatusForbidden || reached {
		t.Fatalf("refused network: status = %d, handler ran = %t; want 403", w.Code, reached)
	}
	if seen == nil || seen.ClientId == nil || *seen.ClientId != fixture.clientId || seen.NetworkId != fixture.networkId {
		t.Fatalf("the refusal saw %+v, want the verified client token", seen)
	}

	refusedNetworkId = server.NewId()
	w, reached = serveClientCredentialGate(t, route, "/admin", "Bearer "+fixture.clientToken)
	if !reached || w.Code != http.StatusOK {
		t.Fatalf("other network: status = %d, handler ran = %t; want the handler", w.Code, reached)
	}

	// a network credential never reaches the refusal
	seen = nil
	refusedNetworkId = fixture.networkId
	if _, reached := serveClientCredentialGate(t, route, "/admin", "Bearer "+fixture.networkToken); !reached || seen != nil {
		t.Fatalf("network token: handler ran = %t, refusal asked = %t", reached, seen != nil)
	}
}

// A route without the gate serves a client token like any other.
func TestRouteWithoutTheGateServesAClientToken(t *testing.T) {
	fixture := newClientCredentialGateFixture()
	route := NewRoute("POST", "/client", gateTestHandler)
	if route.RefusesClientCredentials() {
		t.Fatal("a new route refuses client tokens")
	}
	if w, reached := serveClientCredentialGate(t, route, "/client", "Bearer "+fixture.clientToken); !reached || w.Code != http.StatusOK {
		t.Fatalf("status = %d, handler ran = %t; want the handler", w.Code, reached)
	}
}

// The gated copy keeps the route's identity, so the route table, the stats
// key and the streaming policy are unchanged, and the original route is not
// modified.
func TestRefuseClientCredentialsKeepsTheRoute(t *testing.T) {
	original := NewStreamingRoute("POST", "/upload/([^/]+)", gateTestHandler, StreamingBody{})
	refused := RefuseClientCredentials(original, RefuseEveryClientCredential)
	if !refused.RefusesClientCredentials() || original.RefusesClientCredentials() {
		t.Fatalf("refused = %t, original = %t", refused.RefusesClientCredentials(), original.RefusesClientCredentials())
	}
	if refused.String() != original.String() || refused.Method() != "POST" || refused.Pattern() != "/upload/([^/]+)" {
		t.Fatalf("refused route = %s %s (%s), want the original's", refused.Method(), refused.Pattern(), refused.String())
	}
	if refused.streaming != original.streaming || !refused.captures {
		t.Fatal("the refused route lost the original's streaming policy or captures")
	}

	// a test handler keeps the gate
	fixture := newClientCredentialGateFixture()
	if w, reached := serveClientCredentialGate(t, refused, "/upload/a", "Bearer "+fixture.clientToken); reached || w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, handler ran = %t; want 403", w.Code, reached)
	}
}
