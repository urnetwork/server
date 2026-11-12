package oauth

// The token exchange (exchange.go; §7.8, §22.5's §7 row). The issuer and the
// signer keys are the test's own; the client token is signed with the local
// vault key; the exchange's reads of the database are replaced, so what is
// tested is the exchange's own rules. Tests that spend a proof's jti read
// Redis.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A sidecar's client, its token and its ClientKey, with the exchange's reads
// answering for it.
type exchangeFixture struct {
	networkId    server.Id
	userId       server.Id
	clientId     server.Id
	sidecarId    server.Id
	token        string
	privateKey   ed25519.PrivateKey
	jti          int
	logStateLock sync.Mutex
	logged       []string
}

func newExchangeFixture(t testing.TB) *exchangeFixture {
	t.Helper()
	installTestSignerKeys(t)
	t.Setenv("WARP_DOMAIN", "exchange.example")
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	network := session.NewByJwt(server.NewId(), server.NewId(), "exchange-test", false, false)
	clientId := server.NewId()
	client := network.Client(server.NewId(), clientId)
	client.SetStrictExp(true)
	self := &exchangeFixture{
		networkId:  network.NetworkId,
		userId:     network.UserId,
		clientId:   clientId,
		sidecarId:  server.NewId(),
		token:      client.Testing_Sign(),
		privateKey: privateKey,
	}

	previousValidate := exchangeValidateClient
	previousProofKey := exchangeProofKey
	previousSidecarId := exchangeClientSidecarId
	previousSubject := exchangeClientSubject
	previousLogf := exchangeLogf
	exchangeValidateClient = func(ctx context.Context, byJwt *session.ByJwt) error {
		return nil
	}
	exchangeProofKey = func(ctx context.Context, clientId server.Id) (ed25519.PublicKey, error) {
		if clientId != self.clientId {
			return nil, errors.New("no key")
		}
		return publicKey, nil
	}
	exchangeClientSidecarId = func(ctx context.Context, networkId server.Id, clientId server.Id) *server.Id {
		return &self.sidecarId
	}
	exchangeClientSubject = func(ctx context.Context, networkId server.Id, clientId server.Id) (server.Id, error) {
		return clientId, nil
	}
	exchangeLogf = func(format string, args ...any) {
		self.logStateLock.Lock()
		defer self.logStateLock.Unlock()
		self.logged = append(self.logged, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() {
		exchangeValidateClient = previousValidate
		exchangeProofKey = previousProofKey
		exchangeClientSidecarId = previousSidecarId
		exchangeClientSubject = previousSubject
		exchangeLogf = previousLogf
	})
	return self
}

// A fresh proof of the exchange by key, over token.
func (self *exchangeFixture) proof(key ed25519.PrivateKey, token string) string {
	self.jti += 1
	return session.Testing_SignDpopProof(key, "POST", TokenEndpoint(), token, fmt.Sprintf("exchange-jti-%d-%s", self.jti, server.NewId()), server.NowUtc())
}

func (self *exchangeFixture) resource(audience string) string {
	return "https://" + audience + ".exchange.example"
}

// The exchange's args for audience with a fresh proof.
func (self *exchangeFixture) args(audience string, scope string) *ExchangeArgs {
	return &ExchangeArgs{
		SubjectToken:     self.token,
		SubjectTokenType: ExchangeSubjectTokenType,
		Resources:        []string{self.resource(audience)},
		Scope:            scope,
		Proofs:           []string{self.proof(self.privateKey, self.token)},
	}
}

func (self *exchangeFixture) logLines() []string {
	self.logStateLock.Lock()
	defer self.logStateLock.Unlock()
	return append([]string{}, self.logged...)
}

func requireExchangeRefused(t testing.TB, name string, err error, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want %v", name, err, want)
	}
}

// A sidecar's client token and the ClientKey's proof buy a 15-minute service
// token for the audience the resource names, with the client's and its
// sidecar's ids, no scope without a projection, and one log line.
func TestExchangeIssuesAServiceToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		self := newExchangeFixture(t)

		result, err := Exchange(t.Context(), self.args("gkm", ""))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.IssuedTokenType, ExchangeIssuedTokenType)
		connect.AssertEqual(t, result.TokenType, "Bearer")
		if result.ExpiresIn <= int64((ServiceTokenDuration-time.Minute).Seconds()) || int64(ServiceTokenDuration.Seconds()) < result.ExpiresIn {
			t.Fatalf("expires_in = %d", result.ExpiresIn)
		}
		connect.AssertEqual(t, result.Scope, "")

		claims, err := newTestServiceTokenVerifier(t).Verify(result.AccessToken, "gkm")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.NetworkId, self.networkId)
		connect.AssertEqual(t, claims.ClientId, self.clientId)
		connect.AssertEqual(t, *claims.SidecarId, self.sidecarId)
		// its own subject is no claim
		connect.AssertEqual(t, claims.SubjectClientId == nil, true)
		connect.AssertEqual(t, claims.ProjVersion, uint64(0))
		connect.AssertEqual(t, len(claims.Scopes()), 0)
		// and the token is no service token for another audience
		_, err = newTestServiceTokenVerifier(t).Verify(result.AccessToken, "evs")
		connect.AssertEqual(t, err != nil, true)
	})
}

// §22.5's §7 row: a copied client token buys nothing, a proof by another key is
// refused, and a replayed proof is refused, each as invalid_dpop_proof.
func TestExchangeRefusesACopiedTokenAndAReplayedProof(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		self := newExchangeFixture(t)

		copied := self.args("evs", "")
		copied.Proofs = nil
		_, err := Exchange(t.Context(), copied)
		requireExchangeRefused(t, "a copy alone", err, ErrInvalidDpopProof)

		twice := self.args("evs", "")
		twice.Proofs = append(twice.Proofs, twice.Proofs[0])
		_, err = Exchange(t.Context(), twice)
		requireExchangeRefused(t, "two proofs", err, ErrInvalidDpopProof)

		_, otherKey, err := ed25519.GenerateKey(rand.Reader)
		connect.AssertEqual(t, err, nil)
		other := self.args("evs", "")
		other.Proofs = []string{self.proof(otherKey, self.token)}
		_, err = Exchange(t.Context(), other)
		requireExchangeRefused(t, "another key", err, ErrInvalidDpopProof)

		// a proof over another token, as a proof made for the bearer of
		// another request would be
		wrongToken := self.args("evs", "")
		wrongToken.Proofs = []string{self.proof(self.privateKey, "another.client.token")}
		_, err = Exchange(t.Context(), wrongToken)
		requireExchangeRefused(t, "another token's proof", err, ErrInvalidDpopProof)

		// a proof for api's routes, not the issuer's token endpoint
		apiProof := self.args("evs", "")
		apiProof.Proofs = []string{session.Testing_SignDpopProof(self.privateKey, "POST", "https://api.exchange.example/oauth/token", self.token, "api-jti-"+server.NewId().String(), server.NowUtc())}
		_, err = Exchange(t.Context(), apiProof)
		requireExchangeRefused(t, "an api proof", err, ErrInvalidDpopProof)

		replayed := self.args("evs", "")
		_, err = Exchange(t.Context(), replayed)
		connect.AssertEqual(t, err, nil)
		_, err = Exchange(t.Context(), replayed)
		requireExchangeRefused(t, "a replayed proof", err, ErrInvalidDpopProof)
	})
}

// A subject token past its exp is refused with reject_expired off, the state
// before the fleet flips it, a sidecar's and an ordinary client's alike.
func TestExchangeRefusesAnExpiredSubjectToken(t *testing.T) {
	self := newExchangeFixture(t)
	defer session.Testing_SetRejectExpired(false)()

	network := session.NewByJwt(self.networkId, server.NewId(), "exchange-test", false, false)
	for _, strictExp := range []bool{true, false} {
		client := network.Client(server.NewId(), self.clientId)
		client.SetStrictExp(strictExp)
		client.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-time.Minute))
		args := self.args("evs", "")
		args.SubjectToken = client.Testing_Sign()
		args.Proofs = []string{self.proof(self.privateKey, args.SubjectToken)}
		_, err := Exchange(t.Context(), args)
		requireExchangeRefused(t, fmt.Sprintf("strict_exp %t", strictExp), err, ErrInvalidGrant)
	}

	// and a network token, which names no client, is no subject
	args := self.args("evs", "")
	args.SubjectToken = network.Testing_Sign()
	_, err := Exchange(t.Context(), args)
	requireExchangeRefused(t, "a network token", err, ErrInvalidGrant)

	args = self.args("evs", "")
	args.SubjectToken = "not a token"
	_, err = Exchange(t.Context(), args)
	requireExchangeRefused(t, "not a token", err, ErrInvalidGrant)

	args = self.args("evs", "")
	args.SubjectTokenType = "urn:ietf:params:oauth:token-type:access_token"
	_, err = Exchange(t.Context(), args)
	requireExchangeRefused(t, "another subject type", err, ErrInvalidRequest)
}

// An inactive or reaped client, or one whose subject cannot be told, is
// refused, and a store that cannot answer is the server's error, not a refusal
// of the client.
func TestExchangeRefusesAnInactiveClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		self := newExchangeFixture(t)

		exchangeValidateClient = func(ctx context.Context, byJwt *session.ByJwt) error {
			return errors.New("client is not active")
		}
		_, err := Exchange(t.Context(), self.args("evs", ""))
		requireExchangeRefused(t, "inactive", err, ErrInvalidGrant)

		exchangeValidateClient = func(ctx context.Context, byJwt *session.ByJwt) error {
			return errors.Join(session.ErrAuthUnavailable, errors.New("store down"))
		}
		_, err = Exchange(t.Context(), self.args("evs", ""))
		if err == nil || errors.Is(err, ErrInvalidGrant) {
			t.Fatalf("an unavailable store: err = %v", err)
		}

		exchangeValidateClient = func(ctx context.Context, byJwt *session.ByJwt) error { return nil }
		exchangeClientSubject = func(ctx context.Context, networkId server.Id, clientId server.Id) (server.Id, error) {
			return server.Id{}, errors.New("unresolved")
		}
		_, err = Exchange(t.Context(), self.args("evs", ""))
		requireExchangeRefused(t, "no subject", err, ErrInvalidGrant)
	})
}

// The exchange never issues the api audience, nor one the resource does not name
// exactly.
func TestExchangeNeverIssuesTheApiAudience(t *testing.T) {
	self := newExchangeFixture(t)
	for name, resources := range map[string][]string{
		"the api":          {"https://api.exchange.example"},
		"the api's path":   {"https://api.exchange.example/"},
		"the issuer":       {testIssuer},
		"another domain":   {"https://evs.other.example"},
		"http":             {"http://evs.exchange.example"},
		"a path":           {"https://evs.exchange.example/evs/v1"},
		"a query":          {"https://evs.exchange.example?x=1"},
		"a fragment":       {"https://evs.exchange.example#x"},
		"credentials":      {"https://user@evs.exchange.example"},
		"no such service":  {"https://web.exchange.example"},
		"two resources":    {self.resource("evs"), self.resource("gkm")},
		"none":             nil,
		"a bare audience":  {"evs"},
		"a deeper subhost": {"https://x.evs.exchange.example"},
	} {
		args := self.args("evs", "")
		args.Resources = resources
		_, err := Exchange(t.Context(), args)
		requireExchangeRefused(t, name, err, ErrInvalidTarget)
	}
	for _, audience := range []string{"evs", "gkm", "dns", "certs", "mail"} {
		got, err := serviceAudienceForResource(self.resource(audience))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, got, audience)
		got, err = serviceAudienceForResource(self.resource(audience) + "/")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, got, audience)
	}
}

// Without a projection no scope is granted: a repo: scope, a recovery: scope and
// any other are refused as invalid_scope. A 65th repo: scope, and what is no
// scope-token, are refused before anything is read, whatever a source would
// grant.
func TestExchangeGrantsNoScopeWithoutAProjection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		self := newExchangeFixture(t)
		repoId := server.NewId()
		memberId := server.NewId()
		for _, scope := range []string{
			"repo:" + repoId.String() + ":r",
			"repo:" + repoId.String() + ":admin",
			"recovery:" + memberId.String(),
			"gkm:keypackages",
		} {
			_, err := Exchange(t.Context(), self.args("gkm", scope))
			requireExchangeRefused(t, scope, err, ErrInvalidScope)
		}
	})
}

func TestExchangeRefusesA65thRepoScope(t *testing.T) {
	self := newExchangeFixture(t)
	defer SetExchangeScopeSource(&grantEverythingSource{})()

	repoScopes := []string{}
	for i := 0; i <= ServiceTokenMaxRepoScopeCount; i += 1 {
		repoScopes = append(repoScopes, "repo:"+server.NewId().String()+":r")
	}
	_, err := Exchange(t.Context(), self.args("evs", strings.Join(repoScopes, " ")))
	requireExchangeRefused(t, "65 repo: scopes", err, ErrInvalidScope)

	_, err = Exchange(t.Context(), self.args("evs", "repo:\""))
	requireExchangeRefused(t, "not a scope", err, ErrInvalidScope)
}

// A projection's source grants what its rows give: at most what was asked, with
// its position, member and actor in the claims, 64 repo: scopes included.
type grantEverythingSource struct {
	projVersion uint64
	memberId    server.Id
	requests    []*ExchangeScopeRequest
}

func (self *grantEverythingSource) Grant(ctx context.Context, request *ExchangeScopeRequest) (*ExchangeScopeGrant, error) {
	self.requests = append(self.requests, request)
	return &ExchangeScopeGrant{
		Scopes:      request.Scopes,
		ProjVersion: self.projVersion,
		MemberId:    &self.memberId,
		Act:         &ServiceTokenActor{Subject: "actor"},
	}, nil
}

// A source that grants a scope nobody asked for is the server's error.
type grantExtraSource struct{}

func (self grantExtraSource) Grant(ctx context.Context, request *ExchangeScopeRequest) (*ExchangeScopeGrant, error) {
	return &ExchangeScopeGrant{Scopes: append(request.Scopes, "repo:extra:admin")}, nil
}

func TestExchangeTakesItsScopesFromTheInstalledSource(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		self := newExchangeFixture(t)
		source := &grantEverythingSource{projVersion: 4242, memberId: server.NewId()}
		defer SetExchangeScopeSource(source)()

		repoScopes := []string{}
		for i := 0; i < ServiceTokenMaxRepoScopeCount; i += 1 {
			repoScopes = append(repoScopes, "repo:"+server.NewId().String()+":r")
		}
		// a repeat is asked once
		scope := strings.Join(repoScopes, " ") + " " + repoScopes[0]
		result, err := Exchange(t.Context(), self.args("evs", scope))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(source.requests), 1)
		request := source.requests[0]
		connect.AssertEqual(t, request.NetworkId, self.networkId)
		connect.AssertEqual(t, request.ClientId, self.clientId)
		connect.AssertEqual(t, request.SubjectClientId, self.clientId)
		connect.AssertEqual(t, *request.SidecarId, self.sidecarId)
		connect.AssertEqual(t, request.Audience, "evs")
		connect.AssertEqual(t, request.Scopes, repoScopes)

		claims, err := newTestServiceTokenVerifier(t).Verify(result.AccessToken, "evs")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.Scopes(), repoScopes)
		connect.AssertEqual(t, claims.ProjVersion, uint64(4242))
		connect.AssertEqual(t, *claims.MemberId, source.memberId)
		connect.AssertEqual(t, claims.Act.Subject, "actor")
		connect.AssertEqual(t, result.Scope, strings.Join(repoScopes, " "))

		defer SetExchangeScopeSource(grantExtraSource{})()
		_, err = Exchange(t.Context(), self.args("evs", repoScopes[0]))
		if err == nil || errors.Is(err, ErrInvalidScope) {
			t.Fatalf("an over-granting source: err = %v", err)
		}
	})
}

// While a restore runs, the exchange holds every request (§19.10).
func TestExchangeRefusesWhileAReseedRuns(t *testing.T) {
	self := newExchangeFixture(t)
	defer server.Testing_PushSidecarConfig("sidecar:\n  reseed: true\n")()
	_, err := Exchange(t.Context(), self.args("evs", ""))
	requireExchangeRefused(t, "reseed", err, ErrRestoreRequired)
}

// No token or proof reaches api's service log: the exchange's own line carries
// §2.8 api-11's ids and the DPoP key's thumbprint, and refusals carry their
// reason.
func TestExchangeLogsNoToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		self := newExchangeFixture(t)

		args := self.args("evs", "")
		result, err := Exchange(t.Context(), args)
		connect.AssertEqual(t, err, nil)
		refused := self.args("evs", "")
		refused.Proofs = []string{self.proof(self.privateKey, "another.client.token")}
		w := httptest.NewRecorder()
		writeExchangeError(w, func() error { _, err := Exchange(t.Context(), refused); return err }())
		connect.AssertEqual(t, w.Code, http.StatusBadRequest)

		lines := self.logLines()
		if len(lines) != 2 {
			t.Fatalf("logged %q", lines)
		}
		joined := strings.Join(lines, "\n")
		for name, secret := range map[string]string{
			"the subject token": self.token,
			"the access token":  result.AccessToken,
			"a proof":           args.Proofs[0],
			"a refused proof":   refused.Proofs[0],
		} {
			for _, part := range strings.Split(secret, ".") {
				if 16 <= len(part) && strings.Contains(joined, part) {
					t.Fatalf("the log holds %s", name)
				}
			}
		}
		if !strings.Contains(lines[0], self.clientId.String()) || !strings.Contains(lines[0], self.sidecarId.String()) || !strings.Contains(lines[0], "aud evs") || !strings.Contains(lines[0], "dpop_jkt ") {
			t.Fatalf("the exchange's line = %q", lines[0])
		}
	})
}

// The token route reads the grant type first: an exchange names no client_id,
// and is answered by the exchange, not refused as invalid_client.
func TestTokenHandlerRoutesTheExchange(t *testing.T) {
	self := newExchangeFixture(t)
	form := url.Values{
		"grant_type":         {ExchangeGrantType},
		"subject_token":      {self.token},
		"subject_token_type": {ExchangeSubjectTokenType},
		"resource":           {"https://api.exchange.example"},
	}
	r := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	TokenHandler(w, r)
	connect.AssertEqual(t, w.Code, http.StatusBadRequest)
	answer := map[string]string{}
	connect.AssertEqual(t, json.Unmarshal(w.Body.Bytes(), &answer), nil)
	connect.AssertEqual(t, answer["error"], "invalid_target")
	connect.AssertEqual(t, strings.Contains(w.Body.String(), self.token), false)

	// and the discovery document says so
	metadata := ServerMetadata()
	found := false
	for _, grantType := range metadata.GrantTypesSupported {
		found = found || grantType == ExchangeGrantType
	}
	connect.AssertEqual(t, found, true)
}
