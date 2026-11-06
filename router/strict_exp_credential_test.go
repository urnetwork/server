package router

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The strict_exp gates (strict_exp_credential.go). Pure: tokens are signed with
// the local vault key and nothing reads the database or redis.

type strictExpGateFixture struct {
	clientId    server.Id
	strictToken string
	clientToken string
	networkId   server.Id
}

func newStrictExpGateFixture() *strictExpGateFixture {
	networkId := server.NewId()
	clientId := server.NewId()
	network := session.NewByJwt(networkId, server.NewId(), "strict-gate-test", false, false)
	strict := network.Client(server.NewId(), clientId)
	strict.SetStrictExp(true)
	return &strictExpGateFixture{
		clientId:    clientId,
		strictToken: strict.Testing_Sign(),
		clientToken: network.Client(server.NewId(), server.NewId()).Testing_Sign(),
		networkId:   networkId,
	}
}

func refuseEveryStrictExpCredential(ctx context.Context, byJwt *session.ByJwt) bool {
	return true
}

// A route off the allowlist answers a strict_exp token with 403 and its own
// message, 403 so the sdk does not sign out, and its handler never runs. Every
// other credential reaches the handler unchanged.
func TestRefuseStrictExpCredentialsGate(t *testing.T) {
	fixture := newStrictExpGateFixture()
	route := RefuseStrictExpCredentials(NewRoute("POST", "/account/points", gateTestHandler), refuseEveryStrictExpCredential)
	if !route.RefusesStrictExpCredentials() {
		t.Fatal("the gate is not reported")
	}

	w, reached := serveClientCredentialGate(t, route, "/account/points", "Bearer "+fixture.strictToken)
	if w.Code != http.StatusForbidden || reached {
		t.Fatalf("strict_exp token: status = %d, handler ran = %t; want 403 before the handler", w.Code, reached)
	}
	if body := strings.TrimSpace(w.Body.String()); body != StrictExpCredentialRefusedMessage {
		t.Fatalf("strict_exp token: body = %q", body)
	}

	for name, authorization := range map[string]string{
		"client token":  "Bearer " + fixture.clientToken,
		"api key":       "Bearer urn_" + strings.Repeat("a", 52),
		"no credential": "",
		"other scheme":  "Basic " + base64.StdEncoding.EncodeToString([]byte("a:b")),
		"not a token":   "Bearer not.a.jwt",
	} {
		w, reached := serveClientCredentialGate(t, route, "/account/points", authorization)
		if !reached || w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, handler ran = %t; want the handler", name, w.Code, reached)
		}
	}
}

// The gate asks: while the refusal says no, as it does through stage 1's log-only
// days, a strict_exp token reaches the handler, and the refusal saw the verified
// token.
func TestRefuseStrictExpCredentialsGateAsksTheRefusal(t *testing.T) {
	fixture := newStrictExpGateFixture()
	var seen *session.ByJwt
	route := RefuseStrictExpCredentials(NewRoute("POST", "/account/points", gateTestHandler), func(ctx context.Context, byJwt *session.ByJwt) bool {
		seen = byJwt
		return false
	})
	w, reached := serveClientCredentialGate(t, route, "/account/points", "Bearer "+fixture.strictToken)
	if !reached || w.Code != http.StatusOK {
		t.Fatalf("status = %d, handler ran = %t; want the handler", w.Code, reached)
	}
	if seen == nil || seen.ClientId == nil || *seen.ClientId != fixture.clientId || !seen.StrictExp {
		t.Fatalf("the refusal saw %+v, want the verified strict_exp token", seen)
	}
}

// A token that says strict_exp and does not verify is the handler's to refuse.
func TestRefuseStrictExpCredentialsGateLeavesAnUnverifiedTokenToTheHandler(t *testing.T) {
	fixture := newStrictExpGateFixture()
	route := RefuseStrictExpCredentials(NewRoute("POST", "/account/points", gateTestHandler), refuseEveryStrictExpCredential)

	parts := strings.Split(fixture.strictToken, ".")
	if len(parts) != 3 {
		t.Fatalf("strict_exp token has %d parts", len(parts))
	}
	forged := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 64)))
	if !session.ByJwtStrictExpUnverified(forged) {
		t.Fatal("the forged token's claims should say strict_exp")
	}
	w, reached := serveClientCredentialGate(t, route, "/account/points", "Bearer "+forged)
	if !reached || w.Code != http.StatusOK {
		t.Fatalf("forged token: status = %d, handler ran = %t; want the handler", w.Code, reached)
	}
}

// The proof gate hands the request and the bearer to its refusal, for a
// strict_exp token alone, and answers 403 for what it refuses.
func TestRequireStrictExpProofGate(t *testing.T) {
	fixture := newStrictExpGateFixture()
	var seenToken string
	var seenByJwt *session.ByJwt
	refuse := true
	route := RequireStrictExpProof(NewRoute("GET", "/auth/refresh", gateTestHandler), func(r *http.Request, bearerToken string, byJwt *session.ByJwt) bool {
		seenToken = bearerToken
		seenByJwt = byJwt
		return refuse
	})
	if !route.RequiresStrictExpProof() {
		t.Fatal("the gate is not reported")
	}

	w, reached := serveClientCredentialGate(t, route, "/auth/refresh", "Bearer "+fixture.strictToken)
	if w.Code != http.StatusForbidden || reached {
		t.Fatalf("refused proof: status = %d, handler ran = %t; want 403 before the handler", w.Code, reached)
	}
	if body := strings.TrimSpace(w.Body.String()); body != StrictExpProofRefusedMessage {
		t.Fatalf("refused proof: body = %q", body)
	}
	if seenToken != fixture.strictToken || seenByJwt == nil || !seenByJwt.StrictExp {
		t.Fatalf("the refusal saw token %q and %+v", seenToken, seenByJwt)
	}

	refuse = false
	if w, reached := serveClientCredentialGate(t, route, "/auth/refresh", "Bearer "+fixture.strictToken); !reached || w.Code != http.StatusOK {
		t.Fatalf("good proof: status = %d, handler ran = %t; want the handler", w.Code, reached)
	}

	// any other credential is never asked for a proof
	seenByJwt = nil
	refuse = true
	if w, reached := serveClientCredentialGate(t, route, "/auth/refresh", "Bearer "+fixture.clientToken); !reached || w.Code != http.StatusOK || seenByJwt != nil {
		t.Fatalf("client token: status = %d, handler ran = %t, asked = %t", w.Code, reached, seenByJwt != nil)
	}
}

// Each gate keeps the route's identity and lives through a test handler.
func TestStrictExpGatesKeepTheRoute(t *testing.T) {
	original := NewRoute("POST", "/account/points", gateTestHandler)
	refused := RefuseStrictExpCredentials(original, refuseEveryStrictExpCredential)
	if original.RefusesStrictExpCredentials() || refused.Method() != "POST" || refused.Pattern() != "/account/points" || refused.String() != original.String() {
		t.Fatalf("refused route = %s, original gated = %t", refused.String(), original.RefusesStrictExpCredentials())
	}
	fixture := newStrictExpGateFixture()
	if w, reached := serveClientCredentialGate(t, refused, "/account/points", "Bearer "+fixture.strictToken); reached || w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, handler ran = %t; want 403", w.Code, reached)
	}
}
