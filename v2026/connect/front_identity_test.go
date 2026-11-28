package connect

// Tests of the identification connect gives the nginx front's identity limit
// (frontRequestIdentifier, LBALTOPT2 R15): which requests it reads a bearer
// token from, the audience it verifies each with, and the identity a
// verified token is charged as.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/nginx"
	"github.com/urnetwork/server/v2026/session"
)

// One verification the fake verifier was asked for.
type testFrontParseCall struct {
	token    string
	audience string
}

// A verifier that knows a fixed set of tokens and records what it is asked.
type testFrontParse struct {
	byJwts map[string]*session.ByJwt

	stateLock  sync.Mutex
	parseCalls []testFrontParseCall
}

// See frontRequestIdentifier.parseByJwt.
func (self *testFrontParse) parse(ctx context.Context, token string, audience string) (*session.ByJwt, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.parseCalls = append(self.parseCalls, testFrontParseCall{token: token, audience: audience})
	}()
	byJwt, ok := self.byJwts[token]
	if !ok {
		return nil, errors.New("Could not verify signed token.")
	}
	return byJwt, nil
}

// The verifications so far.
func (self *testFrontParse) calls() []testFrontParseCall {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.parseCalls)
}

// A request with an Authorization, when set.
func newTestFrontIdentityRequest(authorization string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "https://api.service.example/v1", nil)
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	return r
}

// A client token is its client, a network token its network, each verified
// with the audience of the upstream: the api's for the api, connect's for
// connect. The bearer scheme matches in any case. Nothing else is
// identified, and what isn't a bearer jwt is never verified: no
// Authorization, another scheme, an empty token, an api key, a token for
// another upstream, one that doesn't verify, and one naming neither a client
// nor a network.
func TestFrontRequestIdentifierReadsBearerTokens(t *testing.T) {
	clientId := server.NewId()
	networkId := server.NewId()
	otherNetworkId := server.NewId()
	parse := &testFrontParse{
		byJwts: map[string]*session.ByJwt{
			"client.token.sig": {
				NetworkId: networkId,
				ClientId:  &clientId,
			},
			"network.token.sig": {
				NetworkId: otherNetworkId,
			},
			"empty.token.sig": {},
		},
	}
	identifier := &frontRequestIdentifier{
		parseByJwt: parse.parse,
	}

	for _, c := range []struct {
		name          string
		authorization string
		upstream      nginx.NameClass
		identity      nginx.LimitIdentity
		identified    bool
		verified      *testFrontParseCall
	}{
		{name: "a client token to the api", authorization: "Bearer client.token.sig", upstream: nginx.NameClassApi, identity: nginx.LimitIdentity{Id: clientId}, identified: true, verified: &testFrontParseCall{token: "client.token.sig", audience: session.ByJwtAudienceApi}},
		{name: "a client token to connect", authorization: "Bearer client.token.sig", upstream: nginx.NameClassConnect, identity: nginx.LimitIdentity{Id: clientId}, identified: true, verified: &testFrontParseCall{token: "client.token.sig", audience: session.ByJwtAudienceConnect}},
		{name: "a lowercase scheme, as connect's transport reads it", authorization: "bearer client.token.sig", upstream: nginx.NameClassConnect, identity: nginx.LimitIdentity{Id: clientId}, identified: true, verified: &testFrontParseCall{token: "client.token.sig", audience: session.ByJwtAudienceConnect}},
		{name: "a network token", authorization: "Bearer network.token.sig", upstream: nginx.NameClassApi, identity: nginx.LimitIdentity{Id: otherNetworkId, Network: true}, identified: true, verified: &testFrontParseCall{token: "network.token.sig", audience: session.ByJwtAudienceApi}},
		{name: "a token naming neither", authorization: "Bearer empty.token.sig", upstream: nginx.NameClassApi, verified: &testFrontParseCall{token: "empty.token.sig", audience: session.ByJwtAudienceApi}},
		{name: "a token that doesn't verify", authorization: "Bearer forged.token.sig", upstream: nginx.NameClassApi, verified: &testFrontParseCall{token: "forged.token.sig", audience: session.ByJwtAudienceApi}},
		{name: "no Authorization", authorization: "", upstream: nginx.NameClassApi},
		{name: "another scheme", authorization: "Basic dXNlcjpwYXNz", upstream: nginx.NameClassApi},
		{name: "an empty bearer", authorization: "Bearer ", upstream: nginx.NameClassApi},
		{name: "an api key", authorization: "Bearer urn_" + strings.Repeat("a", 52), upstream: nginx.NameClassApi},
		{name: "a forwarded service", authorization: "Bearer client.token.sig", upstream: nginx.NameClassForwarded},
		{name: "an lb name", authorization: "Bearer client.token.sig", upstream: nginx.NameClassLb},
	} {
		before := len(parse.calls())
		identity, identified := identifier.identify(newTestFrontIdentityRequest(c.authorization), c.upstream)
		if identified != c.identified || identity != c.identity {
			t.Errorf("%s: identity %+v %t, want %+v %t", c.name, identity, identified, c.identity, c.identified)
		}
		calls := parse.calls()[before:]
		switch {
		case c.verified == nil && len(calls) != 0:
			t.Errorf("%s: verified %+v, want no verification", c.name, calls)
		case c.verified != nil && !slices.Equal(calls, []testFrontParseCall{*c.verified}):
			t.Errorf("%s: verified %+v, want %+v", c.name, calls, *c.verified)
		}
	}
}

// connect's front settings identify requests by default, so an interface
// whose rate_limit turns the identity limit on has it, and the identifier
// verifies with session.
func TestDefaultFrontServerSettingsIdentifyRequests(t *testing.T) {
	settings := DefaultFrontServerSettings()
	if settings.Front.Limit.IdentifyRequest == nil {
		t.Fatal("connect's front settings don't identify requests")
	}
	if identity, ok := settings.Front.Limit.IdentifyRequest(newTestFrontIdentityRequest(""), nginx.NameClassApi); ok || identity != (nginx.LimitIdentity{}) {
		t.Fatalf("a request without a token identified as %+v", identity)
	}
	if identifier := newFrontRequestIdentifier(); identifier.parseByJwt == nil {
		t.Fatal("the identifier has no verifier")
	}
}

// Through session.ParseByJwtForAudience: a signed client token is its client
// for the api and connect alike, a signed network token its network, and a
// token whose signature was changed is no one. It signs with the fixture
// vault's key, which a test binary reads under ./test-env.sh, or with
// WARP_ENV=local WARP_VAULT_HOME=<server>/local/testdata/vault
// WARP_CONFIG_HOME=<server>/local/testdata/config.
func TestFrontRequestIdentifierVerifiesWithSession(t *testing.T) {
	if _, err := server.Vault.SimpleResource("jwt.yml"); err != nil {
		t.Skipf("requires the fixture vault's jwt key: %v", err)
	}
	networkId := server.NewId()
	clientId := server.NewId()
	networkByJwt := session.NewByJwt(networkId, server.NewId(), "front-identity", false, false)
	networkToken := networkByJwt.Testing_Sign()
	clientToken := networkByJwt.Client(server.NewId(), clientId).Testing_Sign()
	identifier := newFrontRequestIdentifier()

	for _, upstream := range []nginx.NameClass{nginx.NameClassApi, nginx.NameClassConnect} {
		identity, ok := identifier.identify(newTestFrontIdentityRequest("Bearer "+clientToken), upstream)
		if !ok || identity != (nginx.LimitIdentity{Id: clientId}) {
			t.Fatalf("%s: the client token identified as %+v %t, want its client", upstream, identity, ok)
		}
	}
	identity, ok := identifier.identify(newTestFrontIdentityRequest("Bearer "+networkToken), nginx.NameClassApi)
	if !ok || identity != (nginx.LimitIdentity{Id: networkId, Network: true}) {
		t.Fatalf("the network token identified as %+v %t, want its network", identity, ok)
	}

	// the same claims under another signature
	lastDot := strings.LastIndex(clientToken, ".")
	signature := []byte(clientToken[lastDot+1:])
	signature[len(signature)/2] ^= 'A' ^ 'B'
	if signature[len(signature)/2] == clientToken[lastDot+1+len(signature)/2] {
		t.Fatal("the signature didn't change")
	}
	forged := clientToken[:lastDot+1] + string(signature)
	if identity, ok := identifier.identify(newTestFrontIdentityRequest("Bearer "+forged), nginx.NameClassConnect); ok {
		t.Fatalf("a forged signature identified as %+v", identity)
	}
}
