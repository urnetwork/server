package handlers

// api's edge client route (TLS-M5-11, QD-812): only the edge class's bearer reaches it; a block, host or index
// edge.yml does not name is refused 403 with nothing created; with no edge.yml or no urnetwork-edge network it
// answers 503; and through Postgres an admitted ask answers the derived client's jwt, the same client at each ask.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
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
	result := &server.EdgeClientMintResult{}
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(result); err != nil {
			self.t.Fatal(err)
		}
	}
	return response.StatusCode, result
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
	}
	for _, c := range cases {
		if code, _ := world.post(c.bearer, c.body); code != c.code {
			t.Errorf("%s: %d, want %d", c.name, code, c.code)
		}
	}
	// no urnetwork_edge in sidecar.yml: unavailable, before anything is read from Postgres
	world.noNetwork = true
	if code, _ := world.post(world.edge, admitted); code != http.StatusServiceUnavailable {
		t.Errorf("no urnetwork_edge: %d", code)
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
		wantClientId := server.DeriveEdgeClientId("test", "tlsedge", "lb-a.example", 1)
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
		// a restart's ask: the same client
		code, second := world.post(world.edge, admitted)
		if code != http.StatusOK || second.ClientId != first.ClientId {
			t.Fatalf("a second ask: %d %s", code, second.ClientId)
		}
		// the environment is api's own: another environment's api derives another client for the same ask
		world.environment = "other"
		code, other := world.post(world.edge, admitted)
		if code != http.StatusOK || other.ClientId == first.ClientId {
			t.Fatalf("another environment: %d %s", code, other.ClientId)
		}
	})
}
