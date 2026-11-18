package handlers

// api's edge client route (TLS-M5-11, QD-812, QD-873, QD-1054): only the edge class's bearer reaches it; a block,
// host, slot or index edge.yml and the key's slot count do not admit is refused 403 with nothing created; with no
// edge.yml or no urnetwork-edge network it answers 503; and through Postgres an admitted ask answers the derived
// client's jwt and, beside it, the client's service token of audience sockets naming it in urnetwork-edge and
// nothing more, the same client at each ask of its slot, another client for the other slot, and 503 when no
// sockets token can be signed.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// The route served on a verify file of an edge bearer and a tlsedge bearer, with edge.yml as the test gives it.
type edgeClientTestWorld struct {
	t         testing.TB
	server    *httptest.Server
	edge      string
	tlsedge   string
	networkId server.Id
	// what the next reads of edge.yml and of sidecar.yml's urnetwork_edge answer
	configErr   error
	noNetwork   bool
	environment string
	// while set, every sockets token mint fails with it, as an issuer with no svc- key does
	socketsTokenErr error
	// the last answer's Retry-After
	lastRetryAfter string
}

func newEdgeClientTestWorld(t *testing.T, networkId server.Id) *edgeClientTestWorld {
	edge, edgeDigest := newInternalGateTestBearer(t)
	tlsedge, tlsedgeDigest := newInternalGateTestBearer(t)
	verifier, err := server.NewInternalGateVerifier([]byte(
		"edge: {class: edge, sha256: [" + edgeDigest + "]}\ntlsedge: {class: tlsedge, sha256: [" + tlsedgeDigest + "]}\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	config, err := server.ParseEdgeClientsConfig([]byte(
		"platform_url: wss://connect.edge.example\nblocks: {tlsedge: {clients: 2, hosts: [lb-a.example]}}\n",
	), nil)
	if err != nil {
		t.Fatal(err)
	}
	self := &edgeClientTestWorld{t: t, edge: edge, tlsedge: tlsedge, networkId: networkId, environment: "test"}
	settings := DefaultInternalGateEdgeClientSettings()
	// every call reads the file, so a test's change shows at once
	settings.ConfigRereadInterval = 0
	handlers := NewInternalGateEdgeClientHandlers(&InternalGateEdgeClientDeps{
		Verifier: verifier,
		Config: func() (*server.EdgeClientsConfig, error) {
			if self.configErr != nil {
				return nil, self.configErr
			}
			return config, nil
		},
		EdgeNetwork: func() (server.Id, bool) {
			return self.networkId, !self.noNetwork
		},
		Environment: func() (string, error) {
			return self.environment, nil
		},
		MintSocketsToken: func(networkId server.Id, clientId server.Id) (string, time.Time, error) {
			if self.socketsTokenErr != nil {
				return "", time.Time{}, self.socketsTokenErr
			}
			return oauth.MintEdgeClientSocketsToken(networkId, clientId)
		},
		Settings: settings,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	self.server = httptest.NewServer(router.NewRouter(ctx, handlers.Routes()))
	t.Cleanup(self.server.Close)
	return self
}

// One ask with a bearer and a body, and its status and answer.
func (self *edgeClientTestWorld) post(bearer string, body string) (int, *server.EdgeClientMintResult) {
	self.t.Helper()
	request, err := http.NewRequest(http.MethodPost, self.server.URL+server.EdgeClientMintPath, bytes.NewReader([]byte(body)))
	if err != nil {
		self.t.Fatal(err)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := self.server.Client().Do(request)
	if err != nil {
		self.t.Fatal(err)
	}
	defer response.Body.Close()
	self.lastRetryAfter = response.Header.Get("Retry-After")
	result := &server.EdgeClientMintResult{}
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(result); err != nil {
			self.t.Fatal(err)
		}
	}
	return response.StatusCode, result
}

// The test's issuer and svc- key in place of auth.yml's, and a verifier of that key, as sockets mounts it.
func installEdgeClientTestServiceKey(t testing.TB) *oauth.ServiceTokenVerifier {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := oauth.SignerKid(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oauth.Testing_SetIssuer("https://auth.edge.example"))
	t.Cleanup(oauth.Testing_SetSignerKeys(&oauth.SignerKey{
		Kid:        oauth.SignerKeySetService.KidPrefix() + thumbprint,
		Alg:        "ES256",
		Set:        oauth.SignerKeySetService,
		PrivateKey: privateKey,
	}))
	verifier, err := oauth.NewServiceTokenVerifier(&oauth.PublicJwksDocument{
		Issuer: "https://auth.edge.example",
		Keys:   oauth.PublicJwks(oauth.SignerKeySetService).Keys,
	})
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

// The answer's sockets token, verified as sockets verifies it: audience sockets, naming clientId in networkId and
// nothing more, ending when the answer says.
func checkEdgeClientTestSocketsToken(t testing.TB, verifier *oauth.ServiceTokenVerifier, answer *server.EdgeClientMintResult, networkId server.Id, clientId server.Id) {
	t.Helper()
	claims, err := verifier.Verify(answer.SocketsToken, "sockets")
	if err != nil {
		t.Fatalf("the sockets token does not verify: %v", err)
	}
	if claims.NetworkId != networkId || claims.ClientId != clientId {
		t.Fatalf("the sockets token names %s in %s, want %s in %s", claims.ClientId, claims.NetworkId, clientId, networkId)
	}
	if claims.SidecarId != nil || claims.MemberId != nil || claims.SubjectClientId != nil || claims.Act != nil || claims.Scope != "" {
		t.Fatalf("the sockets token claims more than its client: %+v", claims)
	}
	if claims.ExpiresAt == nil || claims.ExpiresAt.Time.UnixMilli() != answer.SocketsTokenExpireTimeMillis {
		t.Fatalf("the sockets token's exp %v, answered %d", claims.ExpiresAt, answer.SocketsTokenExpireTimeMillis)
	}
	if _, err := verifier.Verify(answer.SocketsToken, "evs"); err == nil {
		t.Fatal("evs takes the edge client's sockets token")
	}
}

func TestInternalGateEdgeClientRefusals(t *testing.T) {
	world := newEdgeClientTestWorld(t, server.NewId())
	admitted := `{"block":"tlsedge","host":"lb-a.example","index":0}`
	stranger, _ := newInternalGateTestBearer(t)
	cases := []struct {
		name   string
		bearer string
		body   string
		code   int
	}{
		{name: "no bearer", bearer: "", body: admitted, code: http.StatusForbidden},
		{name: "a bearer the file does not hold", bearer: stranger, body: admitted, code: http.StatusForbidden},
		// another class's bearer mints nothing, a gate host's among them
		{name: "tlsedge's projection bearer", bearer: world.tlsedge, body: admitted, code: http.StatusForbidden},
		{name: "no body", bearer: world.edge, body: "", code: http.StatusBadRequest},
		{name: "an unknown field", bearer: world.edge, body: `{"block":"tlsedge","host":"lb-a.example","index":0,"client_id":"x"}`, code: http.StatusBadRequest},
		{name: "a block not listed", bearer: world.edge, body: `{"block":"mxedge","host":"lb-a.example","index":0}`, code: http.StatusForbidden},
		{name: "a host not listed", bearer: world.edge, body: `{"block":"tlsedge","host":"lb-b.example","index":0}`, code: http.StatusForbidden},
		{name: "an index at K", bearer: world.edge, body: `{"block":"tlsedge","host":"lb-a.example","index":2}`, code: http.StatusForbidden},
		{name: "a negative index", bearer: world.edge, body: `{"block":"tlsedge","host":"lb-a.example","index":-1}`, code: http.StatusForbidden},
		// a slot at the slot count, 2, and past it: a third process of the block on the host mints nothing
		{name: "a slot at the slot count", bearer: world.edge, body: `{"block":"tlsedge","host":"lb-a.example","slot":2,"index":0}`, code: http.StatusForbidden},
		{name: "a slot past the slot count", bearer: world.edge, body: `{"block":"tlsedge","host":"lb-a.example","slot":7,"index":0}`, code: http.StatusForbidden},
		{name: "a negative slot", bearer: world.edge, body: `{"block":"tlsedge","host":"lb-a.example","slot":-1,"index":0}`, code: http.StatusForbidden},
		{name: "a slot that is not a number", bearer: world.edge, body: `{"block":"tlsedge","host":"lb-a.example","slot":"1","index":0}`, code: http.StatusBadRequest},
	}
	for _, c := range cases {
		if code, _ := world.post(c.bearer, c.body); code != c.code {
			t.Errorf("%s: %d, want %d", c.name, code, c.code)
		}
	}
	// no urnetwork_edge in sidecar.yml: unavailable, before anything is read from Postgres, with the retry the
	// settings name
	world.noNetwork = true
	if code, _ := world.post(world.edge, admitted); code != http.StatusServiceUnavailable {
		t.Errorf("no urnetwork_edge: %d", code)
	}
	if retryAfter := world.lastRetryAfter; retryAfter != "5" {
		t.Errorf("no urnetwork_edge: Retry-After %q", retryAfter)
	}
	world.noNetwork = false
	// no edge.yml that parses, on a process that has read none: unavailable
	fresh := newEdgeClientTestWorld(t, server.NewId())
	fresh.configErr = errors.New("no edge.yml")
	if code, _ := fresh.post(fresh.edge, admitted); code != http.StatusServiceUnavailable {
		t.Errorf("no edge.yml: %d", code)
	}
}

func TestInternalGateEdgeClientMintsTheDerivedClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(testing.TB) {
		ctx := context.Background()
		verifier := installEdgeClientTestServiceKey(t)
		networkId := server.NewId()
		world := newEdgeClientTestWorld(t, networkId)
		admitted := `{"block":"tlsedge","host":"lb-a.example","index":1}`

		// sidecar.yml names a network that has no row: unavailable, nothing created
		if code, _ := world.post(world.edge, admitted); code != http.StatusServiceUnavailable {
			t.Fatalf("no network row: %d", code)
		}
		model.Testing_CreateNetwork(ctx, networkId, "urnetwork-edge-handler-test", server.NewId())

		code, first := world.post(world.edge, admitted)
		if code != http.StatusOK {
			t.Fatalf("an admitted ask: %d", code)
		}
		// a body with no slot is slot 0's, as a block without WARP_SLOT is while warp's change rolls out
		wantClientId := server.DeriveEdgeClientId("test", "lb-a.example", "tlsedge", 0, 1)
		if first.ClientId != wantClientId.String() || first.PlatformUrl != "wss://connect.edge.example" || first.ByClientJwt == "" {
			t.Fatalf("answered client %s, want %s", first.ClientId, wantClientId)
		}
		byJwt, err := session.ParseByJwtForAudience(ctx, first.ByClientJwt, session.ByJwtAudienceConnect)
		if err != nil {
			t.Fatal(err)
		}
		if byJwt.ClientId == nil || *byJwt.ClientId != wantClientId || !byJwt.StrictExp || byJwt.ExpiresAt == nil ||
			byJwt.ExpiresAt.Time.UnixMilli() != first.ExpireTimeMillis {
			t.Fatalf("jwt %+v, answered exp %d", byJwt, first.ExpireTimeMillis)
		}
		// beside the jwt, the client's sockets token (QD-1054)
		checkEdgeClientTestSocketsToken(t, verifier, first, networkId, wantClientId)
		// a restart's ask: the same client
		code, second := world.post(world.edge, admitted)
		if code != http.StatusOK || second.ClientId != first.ClientId {
			t.Fatalf("a second ask: %d %s", code, second.ClientId)
		}
		code, slotZero := world.post(world.edge, `{"block":"tlsedge","host":"lb-a.example","slot":0,"index":1}`)
		if code != http.StatusOK || slotZero.ClientId != first.ClientId {
			t.Fatalf("slot 0 named: %d %s", code, slotZero.ClientId)
		}

		// a deploy's new process in slot 1 beside the old one in slot 0: another client, which connect's
		// audience takes as its own, and its restart repeats it
		code, other := world.post(world.edge, `{"block":"tlsedge","host":"lb-a.example","slot":1,"index":1}`)
		wantOtherClientId := server.DeriveEdgeClientId("test", "lb-a.example", "tlsedge", 1, 1)
		if code != http.StatusOK || other.ClientId != wantOtherClientId.String() || other.ClientId == first.ClientId {
			t.Fatalf("slot 1: %d %s, want %s", code, other.ClientId, wantOtherClientId)
		}
		checkEdgeClientTestSocketsToken(t, verifier, other, networkId, wantOtherClientId)
		otherByJwt, err := session.ParseByJwtForAudience(ctx, other.ByClientJwt, session.ByJwtAudienceConnect)
		if err != nil {
			t.Fatal(err)
		}
		if otherByJwt.ClientId == nil || *otherByJwt.ClientId != wantOtherClientId || !otherByJwt.StrictExp {
			t.Fatalf("slot 1's jwt %+v", otherByJwt)
		}
		if err := session.ValidateByJwtState(ctx, byJwt, true); err != nil {
			t.Fatalf("slot 0's client after slot 1's mint: %v", err)
		}
		if err := session.ValidateByJwtState(ctx, otherByJwt, true); err != nil {
			t.Fatalf("slot 1's client: %v", err)
		}
		code, otherAgain := world.post(world.edge, `{"block":"tlsedge","host":"lb-a.example","slot":1,"index":1}`)
		if code != http.StatusOK || otherAgain.ClientId != other.ClientId {
			t.Fatalf("slot 1's second ask: %d %s", code, otherAgain.ClientId)
		}
		// the environment is api's own: another environment's api derives another client for the same ask
		world.environment = "other"
		code, otherEnvironment := world.post(world.edge, admitted)
		if code != http.StatusOK || otherEnvironment.ClientId == first.ClientId {
			t.Fatalf("another environment: %d %s", code, otherEnvironment.ClientId)
		}
		world.environment = "test"

		// a process that cannot sign a sockets token answers unavailable, never a jwt alone
		world.socketsTokenErr = errors.New("auth.yml lists no svc signer key")
		if code, _ := world.post(world.edge, admitted); code != http.StatusServiceUnavailable || world.lastRetryAfter != "5" {
			t.Fatalf("no sockets token: %d, Retry-After %q", code, world.lastRetryAfter)
		}
		world.socketsTokenErr = nil

		// a removed client is refused, and is minted no sockets token
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, wantClientId))
		})
		if code, answer := world.post(world.edge, admitted); code != http.StatusForbidden || answer.SocketsToken != "" {
			t.Fatalf("a removed client: %d", code)
		}
	})
}
