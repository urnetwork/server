package router

import (
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The lag.dev guest gate (lag_guest_credential.go). Pure: tokens are signed
// with the local vault key and nothing reads the database or redis.

// A lag.dev guest's client token and network token, and an ordinary client
// token of another network.
type lagGuestGateFixture struct {
	guestClientToken  string
	guestNetworkToken string
	clientToken       string
	networkToken      string
}

func newLagGuestGateFixture() *lagGuestGateFixture {
	guest := session.NewByJwt(server.NewId(), server.NewId(), "lag-gate-test", false, false)
	guest.LagGuest = true
	network := session.NewByJwt(server.NewId(), server.NewId(), "gate-test", false, false)
	return &lagGuestGateFixture{
		guestClientToken:  guest.Client(server.NewId(), server.NewId()).Testing_Sign(),
		guestNetworkToken: guest.Testing_Sign(),
		clientToken:       network.Client(server.NewId(), server.NewId()).Testing_Sign(),
		networkToken:      network.Testing_Sign(),
	}
}

// A route with the gate answers a guest's token, client or network, with 403
// and the refusal message before its handler runs. Every other credential,
// and none, reaches the handler unchanged.
func TestRefuseLagGuestCredentialsGate(t *testing.T) {
	fixture := newLagGuestGateFixture()
	route := RefuseLagGuestCredentials(NewRoute("GET", "/anything", gateTestHandler))
	if !route.RefusesLagGuestCredentials() {
		t.Fatal("the route does not report its gate")
	}

	for name, token := range map[string]string{
		"guest client token":  fixture.guestClientToken,
		"guest network token": fixture.guestNetworkToken,
	} {
		w, reached := serveClientCredentialGate(t, route, "/anything", "Bearer "+token)
		if w.Code != http.StatusForbidden || reached {
			t.Fatalf("%s: status = %d, handler ran = %t; want 403 before the handler", name, w.Code, reached)
		}
		if body := strings.TrimSpace(w.Body.String()); body != LagGuestCredentialRefusedMessage {
			t.Fatalf("%s: body = %q", name, body)
		}
	}

	// a guest's token with a broken signature is no guest's token: the
	// handler's authentication refuses it as any bad token
	broken := fixture.guestClientToken[:len(fixture.guestClientToken)-4] + "AAAA"
	for name, authorization := range map[string]string{
		"client token":  "Bearer " + fixture.clientToken,
		"network token": "Bearer " + fixture.networkToken,
		"api key":       "Bearer urn_" + strings.Repeat("k", 52),
		"broken guest":  "Bearer " + broken,
		"no credential": "",
	} {
		w, reached := serveClientCredentialGate(t, route, "/anything", authorization)
		if w.Code != http.StatusOK || !reached {
			t.Fatalf("%s: status = %d, handler ran = %t; want the handler", name, w.Code, reached)
		}
	}
}

// A route without the gate serves a guest's token; the gate composes with the
// client token gate, and a test handler keeps both.
func TestLagGuestGateComposesWithTheClientTokenGate(t *testing.T) {
	fixture := newLagGuestGateFixture()

	open := NewRoute("GET", "/lag", gateTestHandler)
	w, reached := serveClientCredentialGate(t, open, "/lag", "Bearer "+fixture.guestClientToken)
	if w.Code != http.StatusOK || !reached {
		t.Fatalf("an ungated route: status = %d, handler ran = %t; want the handler", w.Code, reached)
	}

	// the client token gate refuses first, with its own message; the guest
	// gate refuses what the client token gate lets through
	both := RefuseLagGuestCredentials(RefuseClientCredentials(NewRoute("POST", "/admin", gateTestHandler), RefuseEveryClientCredential))
	w, reached = serveClientCredentialGate(t, both, "/admin", "Bearer "+fixture.guestClientToken)
	if w.Code != http.StatusForbidden || reached {
		t.Fatalf("guest client token: status = %d, handler ran = %t; want 403", w.Code, reached)
	}
	w, reached = serveClientCredentialGate(t, both, "/admin", "Bearer "+fixture.guestNetworkToken)
	if w.Code != http.StatusForbidden || reached || strings.TrimSpace(w.Body.String()) != LagGuestCredentialRefusedMessage {
		t.Fatalf("guest network token: status = %d, handler ran = %t, body %q; want the guest refusal", w.Code, reached, w.Body.String())
	}
	w, reached = serveClientCredentialGate(t, both, "/admin", "Bearer "+fixture.networkToken)
	if w.Code != http.StatusOK || !reached {
		t.Fatalf("network token: status = %d, handler ran = %t; want the handler", w.Code, reached)
	}
}
