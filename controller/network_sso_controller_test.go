//go:build sidecar_pending

package controller

// §7.6's sign-in through a team's IdP (TEAM-M6-05, §22.5's §7.6 rows, §22.7's
// JIT) against a fake OIDC provider over TLS, on Postgres with MG6: api's own
// HTTP client discovers it, redeems the code with PKCE and verifies the ID
// token under its JWKS.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// One code the fake IdP will redeem: the challenge and nonce its authorization
// request carried, and the identity it asserts.
type testIdpGrant struct {
	challenge string
	nonce     string
	sub       string
	email     string
}

// A fake OIDC provider: discovery, an RSA JWKS and a token endpoint that
// redeems each code once, checking the client secret and PKCE.
type testOidcProvider struct {
	server       *httptest.Server
	key          *rsa.PrivateKey
	clientId     string
	clientSecret string

	stateLock sync.Mutex
	grants    map[string]*testIdpGrant
	// set to sign the next ID token with another nonce
	forgeNonce bool
}

func newTestOidcProvider(t testing.TB) *testOidcProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	self := &testOidcProvider{key: key, clientId: "synthetic-client", clientSecret: "synthetic-secret", grants: map[string]*testIdpGrant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 self.server.URL,
			"authorization_endpoint": self.server.URL + "/authorize",
			"token_endpoint":         self.server.URL + "/token",
			"jwks_uri":               self.server.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.PostForm.Get("client_id") != self.clientId || r.PostForm.Get("client_secret") != self.clientSecret {
			http.Error(w, "bad client", http.StatusUnauthorized)
			return
		}
		grant := func() *testIdpGrant {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			grant := self.grants[r.PostForm.Get("code")]
			delete(self.grants, r.PostForm.Get("code"))
			return grant
		}()
		if grant == nil || NetworkSsoCodeChallenge(r.PostForm.Get("code_verifier")) != grant.challenge {
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		nonce := grant.nonce
		if self.forgeNonce {
			nonce = "another-flows-nonce"
		}
		now := time.Now()
		token := gojwt.NewWithClaims(gojwt.SigningMethodRS256, gojwt.MapClaims{
			"iss":            self.server.URL,
			"aud":            self.clientId,
			"sub":            grant.sub,
			"email":          grant.email,
			"email_verified": true,
			"nonce":          nonce,
			"iat":            now.Unix(),
			"exp":            now.Add(5 * time.Minute).Unix(),
		})
		token.Header["kid"] = "k1"
		signed, err := token.SignedString(key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id_token": signed, "token_type": "Bearer"})
	})
	self.server = httptest.NewTLSServer(mux)
	t.Cleanup(self.server.Close)
	return self
}

// What a browser does at the IdP: the authorization request's challenge, nonce
// and state, and a code for the identity.
func (self *testOidcProvider) authorize(t testing.TB, authorizationUrl string, sub string, email string) (state string, code string) {
	t.Helper()
	parsed, err := url.Parse(authorizationUrl)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("code_challenge_method") != "S256" || query.Get("client_id") != self.clientId || query.Get("nonce") == "" {
		t.Fatalf("the authorization request: %s", authorizationUrl)
	}
	code = server.NewId().String()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.grants[code] = &testIdpGrant{challenge: query.Get("code_challenge"), nonce: query.Get("nonce"), sub: sub, email: email}
	return query.Get("state"), code
}

// An in-memory flow store, taking each flow once.
type testSsoFlowStore struct {
	stateLock sync.Mutex
	flows     map[string]*model.NetworkSsoFlow
}

func (self *testSsoFlowStore) Put(ctx context.Context, flow *model.NetworkSsoFlow, ttl time.Duration) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.flows[flow.State] = flow
	return nil
}

func (self *testSsoFlowStore) Take(ctx context.Context, state string) (*model.NetworkSsoFlow, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	flow := self.flows[state]
	delete(self.flows, state)
	return flow, nil
}

// A team with the fake provider as its IdP, a flow store and api's HTTP client
// trusting the provider's test certificate on loopback.
type testSsoTeam struct {
	ctx      context.Context
	teamId   server.Id
	idpId    server.Id
	provider *testOidcProvider
}

func newTestSsoTeam(t testing.TB) *testSsoTeam {
	t.Helper()
	ctx := t.Context()
	provider := newTestOidcProvider(t)
	teamId := server.NewId()
	model.Testing_CreateNetwork(ctx, teamId, "team-"+teamId.String(), server.NewId())
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team(network_id) VALUES($1)`, teamId))
	})
	t.Cleanup(model.Testing_SetNetworkIdpSecretKey(bytes.Repeat([]byte{9}, 32)))
	t.Cleanup(model.Testing_SetNetworkIdpRequireTeamTier(func(ctx context.Context, networkId server.Id) error { return nil }))
	// the provider listens on loopback, which CreateNetworkIdp's issuer check
	// refuses, so its row is written as the route would write it
	idpId := server.NewId()
	testWriteIdpRow(t, ctx, teamId, idpId, provider)

	store := &testSsoFlowStore{flows: map[string]*model.NetworkSsoFlow{}}
	model.SetNetworkSsoFlowStore(store)
	t.Cleanup(func() { model.SetNetworkSsoFlowStore(nil) })
	settings := DefaultHttpNetworkOidcClientSettings()
	settings.DialControl = nil
	transport := provider.server.Client().Transport.(*http.Transport).Clone()
	settings.Transport = transport
	SetNetworkOidcClient(NewHttpNetworkOidcClient(settings))
	t.Cleanup(func() { SetNetworkOidcClient(nil) })
	previousOrigin := networkSsoRedirectOrigin
	networkSsoRedirectOrigin = func() (string, error) { return "https://api.example", nil }
	t.Cleanup(func() { networkSsoRedirectOrigin = previousOrigin })
	return &testSsoTeam{ctx: ctx, teamId: teamId, idpId: idpId, provider: provider}
}

// The IdP row the provider's loopback issuer needs, its secret sealed as
// CreateNetworkIdp seals it.
func testWriteIdpRow(t testing.TB, ctx context.Context, teamId server.Id, idpId server.Id, provider *testOidcProvider) {
	t.Helper()
	sealed, err := model.Testing_SealNetworkIdpSecret(idpId, provider.clientSecret)
	if err != nil {
		t.Fatal(err)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_idp (idp_id, network_id, issuer, client_id, client_auth, client_secret_ciphertext) VALUES ($1, $2, $3, $4, $5, $6)`,
			idpId, teamId, provider.server.URL, provider.clientId, model.NetworkIdpClientAuthClientSecret, sealed,
		))
	})
}

// Runs one flow to its callback: begun from linkingSession, the browser at the
// provider as sub, then the callback.
func (self *testSsoTeam) signIn(t testing.TB, linkingSession *session.ClientSession, sub string) (*NetworkSsoCallbackResult, string, string) {
	t.Helper()
	begin, err := BeginNetworkSso(self.ctx, self.idpId, linkingSession)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	state, code := self.provider.authorize(t, begin.AuthorizationUrl, sub, "employee@team.example")
	result := self.callback(t, state, code)
	return result, state, code
}

func (self *testSsoTeam) callback(t testing.TB, state string, code string) *NetworkSsoCallbackResult {
	t.Helper()
	clientSession := session.NewLocalClientSession(self.ctx, "192.0.2.21:1", nil)
	defer clientSession.Cancel()
	result, err := NetworkSsoCallback(&NetworkSsoCallbackArgs{State: state, Code: code}, clientSession)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	return result
}

// §22.7's JIT end to end: a password account is never linked by an IdP sign-in
// alone, links once its own sign-in begins the flow and becomes a pending
// member with no credential; admitted, it signs in through the IdP to its team
// alone, with a credential naming its membership.
func TestSsoFlowLinksOnlyAfterTheUsersOwnSignIn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTestSsoTeam(t)
		passwordUserId, personalNetworkId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(self.ctx, personalNetworkId, "password-account", passwordUserId)

		result, _, _ := self.signIn(t, nil, "employee-7")
		if result.Error == nil || result.ByJwt != "" {
			t.Fatalf("an unlinked identity signed in: %+v", result)
		}
		if model.GetNetworkMemberOfUser(self.ctx, self.teamId, passwordUserId) != nil {
			t.Fatal("an IdP sign-in alone made a member")
		}

		passwordSession := session.NewLocalClientSession(self.ctx, "192.0.2.22:1", session.NewByJwt(personalNetworkId, passwordUserId, "personal", false, false))
		defer passwordSession.Cancel()
		result, _, _ = self.signIn(t, passwordSession, "employee-7")
		if result.Error != nil || !result.Linked || !result.Pending || result.ByJwt != "" {
			t.Fatalf("the user's own sign-in: %+v", result)
		}

		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(self.ctx, `UPDATE network_member SET pending = false WHERE network_id = $1 AND user_id = $2`, self.teamId, passwordUserId))
		})
		result, _, _ = self.signIn(t, nil, "employee-7")
		if result.Error != nil || result.ByJwt == "" || result.NetworkId == nil || *result.NetworkId != self.teamId {
			t.Fatalf("an admitted member's sign-in: %+v", result.Error)
		}
		byJwt, err := session.ParseByJwt(self.ctx, result.ByJwt)
		if err != nil || byJwt.NetworkId != self.teamId || byJwt.MemberId == nil || byJwt.UserId != passwordUserId {
			t.Fatalf("the credential: %v", err)
		}
		connect.AssertEqual(t, session.ValidateByJwtState(self.ctx, byJwt, false), nil)
	})
}

// A replayed state finds no flow, and an ID token with another flow's nonce
// does not verify.
func TestSsoStateAndNonceReplaysFail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTestSsoTeam(t)
		userId, personalNetworkId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(self.ctx, personalNetworkId, "user", userId)
		userSession := session.NewLocalClientSession(self.ctx, "192.0.2.23:1", session.NewByJwt(personalNetworkId, userId, "personal", false, false))
		defer userSession.Cancel()

		result, state, code := self.signIn(t, userSession, "employee-8")
		if result.Error != nil {
			t.Fatalf("the first callback: %+v", result.Error)
		}
		replayed := self.callback(t, state, code)
		if replayed.Error == nil {
			t.Fatal("a replayed state finished a second sign-in")
		}

		self.provider.forgeNonce = true
		begin, err := BeginNetworkSso(self.ctx, self.idpId, userSession)
		if err != nil {
			t.Fatal(err)
		}
		state, code = self.provider.authorize(t, begin.AuthorizationUrl, "employee-8", "employee@team.example")
		forged := self.callback(t, state, code)
		if forged.Error == nil {
			t.Fatal("an ID token with another flow's nonce signed in")
		}
	})
}
