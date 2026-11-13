//go:build sidecar_pending

package controller

// §7.6's sign-in through a team's IdP (TEAM-M6-05, §22.5's §7.6 rows, §22.7's
// JIT) against a fake OIDC provider over TLS, on Postgres with MG6: api's own
// HTTP client discovers it, redeems the code with PKCE and verifies the ID
// token under its JWKS. Flows wait in the test stack's Redis, in production's
// store (QD-720).

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

// A team with the fake provider as its IdP, the Redis flow store and api's
// HTTP client trusting the provider's test certificate on loopback.
type testSsoTeam struct {
	ctx      context.Context
	teamId   server.Id
	idpId    server.Id
	provider *testOidcProvider
	// the store flows wait in, whose clock a test may set
	flowStore *model.RedisNetworkSsoFlowStore
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

	// a store of the test's own, as production's is made, so that a test's
	// clock stays its own
	previousStore := model.CurrentNetworkSsoFlowStore()
	if _, ok := previousStore.(*model.RedisNetworkSsoFlowStore); !ok {
		t.Fatalf("the process's flow store is %T", previousStore)
	}
	store := model.NewRedisNetworkSsoFlowStore(model.DefaultNetworkIdpSettings())
	model.SetNetworkSsoFlowStore(store)
	t.Cleanup(func() { model.SetNetworkSsoFlowStore(previousStore) })
	settings := DefaultHttpNetworkOidcClientSettings()
	settings.DialControl = nil
	transport := provider.server.Client().Transport.(*http.Transport).Clone()
	settings.Transport = transport
	SetNetworkOidcClient(NewHttpNetworkOidcClient(settings))
	t.Cleanup(func() { SetNetworkOidcClient(nil) })
	previousOrigin := networkSsoRedirectOrigin
	networkSsoRedirectOrigin = func() (string, error) { return "https://api.example", nil }
	t.Cleanup(func() { networkSsoRedirectOrigin = previousOrigin })
	return &testSsoTeam{ctx: ctx, teamId: teamId, idpId: idpId, provider: provider, flowStore: store}
}

// Whether Redis holds a flow under the state, and the key's remaining life.
func (self *testSsoTeam) flowWaits(t testing.TB, state string) (waits bool, ttl time.Duration) {
	t.Helper()
	server.Redis(self.ctx, func(r server.RedisClient) {
		key := "{sso_" + state + "}"
		count, err := r.Exists(self.ctx, key).Result()
		server.Raise(err)
		waits = 0 < count
		if waits {
			ttl, err = r.PTTL(self.ctx, key).Result()
			server.Raise(err)
		}
	})
	return
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

// A whole sign-in on the Redis store: between its start and its callback the
// flow waits under {sso_<state>} with at most the 10 minute ttl, the callback's
// take removes it, and a callback for a state no flow began finds nothing.
func TestSsoFlowWaitsInRedisUntilItsCallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTestSsoTeam(t)
		userId, personalNetworkId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(self.ctx, personalNetworkId, "user", userId)
		userSession := session.NewLocalClientSession(self.ctx, "192.0.2.24:1", session.NewByJwt(personalNetworkId, userId, "personal", false, false))
		defer userSession.Cancel()

		begin, err := BeginNetworkSso(self.ctx, self.idpId, userSession)
		if err != nil {
			t.Fatal(err)
		}
		state, code := self.provider.authorize(t, begin.AuthorizationUrl, "employee-11", "employee@team.example")
		waits, ttl := self.flowWaits(t, state)
		if !waits || ttl <= 0 || model.DefaultNetworkIdpSettings().FlowTtl < ttl {
			t.Fatalf("the flow waits in Redis: %t, with %s to live", waits, ttl)
		}

		if unknown := self.callback(t, "AAAAAAAAAAAAAAAAAAAAAA", code); unknown.Error == nil {
			t.Fatal("a state no flow began finished a sign-in")
		}
		if waits, _ := self.flowWaits(t, state); !waits {
			t.Fatal("another state's callback took the flow")
		}

		result := self.callback(t, state, code)
		if result.Error != nil || !result.Linked || !result.Pending {
			t.Fatalf("the callback: %+v", result)
		}
		if waits, _ := self.flowWaits(t, state); waits {
			t.Fatal("the callback left the flow in Redis")
		}
		if model.GetNetworkMemberOfUser(self.ctx, self.teamId, userId) == nil {
			t.Fatal("the sign-in made no member")
		}
	})
}

// A callback at or after the flow's ttl is refused, links nothing, and the
// flow is gone for a callback that follows it on an earlier clock.
func TestSsoCallbackRefusesAnExpiredFlow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTestSsoTeam(t)
		userId, personalNetworkId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(self.ctx, personalNetworkId, "user", userId)
		userSession := session.NewLocalClientSession(self.ctx, "192.0.2.25:1", session.NewByJwt(personalNetworkId, userId, "personal", false, false))
		defer userSession.Cancel()
		flowTtl := model.DefaultNetworkIdpSettings().FlowTtl
		// Redis's own clock is the real one, so the fake starts there
		start := server.NowUtc()
		now := start
		self.flowStore.Testing_SetNow(func() time.Time { return now })

		begin, err := BeginNetworkSso(self.ctx, self.idpId, userSession)
		if err != nil {
			t.Fatal(err)
		}
		state, code := self.provider.authorize(t, begin.AuthorizationUrl, "employee-12", "employee@team.example")
		now = start.Add(flowTtl)
		if expired := self.callback(t, state, code); expired.Error == nil || expired.Linked || expired.ByJwt != "" {
			t.Fatalf("an expired flow finished a sign-in: %+v", expired)
		}
		if model.GetNetworkMemberOfUser(self.ctx, self.teamId, userId) != nil {
			t.Fatal("an expired flow made a member")
		}
		now = start
		if replayed := self.callback(t, state, code); replayed.Error == nil {
			t.Fatal("an expired flow came back")
		}

		// a flow one tick short of its ttl still finishes
		begin, err = BeginNetworkSso(self.ctx, self.idpId, userSession)
		if err != nil {
			t.Fatal(err)
		}
		state, code = self.provider.authorize(t, begin.AuthorizationUrl, "employee-12", "employee@team.example")
		now = start.Add(flowTtl - time.Microsecond)
		if live := self.callback(t, state, code); live.Error != nil || !live.Linked {
			t.Fatalf("a flow inside its ttl: %+v", live)
		}
	})
}

// Callbacks carrying one state and code, released together, finish one
// sign-in: the take answers one of them and every other finds no flow.
func TestSsoConcurrentCallbacksFinishOneSignIn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTestSsoTeam(t)
		userId, personalNetworkId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(self.ctx, personalNetworkId, "user", userId)
		userSession := session.NewLocalClientSession(self.ctx, "192.0.2.26:1", session.NewByJwt(personalNetworkId, userId, "personal", false, false))
		defer userSession.Cancel()

		begin, err := BeginNetworkSso(self.ctx, self.idpId, userSession)
		if err != nil {
			t.Fatal(err)
		}
		state, code := self.provider.authorize(t, begin.AuthorizationUrl, "employee-13", "employee@team.example")

		callbackCount := 12
		results := make([]*NetworkSsoCallbackResult, callbackCount)
		errs := make([]error, callbackCount)
		release := make(chan struct{})
		var ready sync.WaitGroup
		var done sync.WaitGroup
		for i := range callbackCount {
			ready.Add(1)
			done.Add(1)
			go func() {
				defer done.Done()
				clientSession := session.NewLocalClientSession(self.ctx, "192.0.2.27:1", nil)
				defer clientSession.Cancel()
				ready.Done()
				<-release
				results[i], errs[i] = NetworkSsoCallback(&NetworkSsoCallbackArgs{State: state, Code: code}, clientSession)
			}()
		}
		ready.Wait()
		close(release)
		done.Wait()

		finishedCount := 0
		for i := range callbackCount {
			if errs[i] != nil {
				t.Fatalf("callback %d: %v", i, errs[i])
			}
			if results[i].Error == nil {
				finishedCount += 1
				if !results[i].Linked || !results[i].Pending {
					t.Fatalf("the callback that finished: %+v", results[i])
				}
			}
		}
		if finishedCount != 1 {
			t.Fatalf("%d of %d concurrent callbacks finished the sign-in", finishedCount, callbackCount)
		}
		if waits, _ := self.flowWaits(t, state); waits {
			t.Fatal("the callbacks left the flow in Redis")
		}
	})
}
