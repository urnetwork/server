// QD-1054 at the sockets block, on the private test Redis that test-env.sh attests: urnetwork-edge's platform
// clients reach sockets on the service token api's edge client route answers beside each jwt. A stand-in api
// answers each ask as the route does, a client jwt and the client's sockets token minted by the route's own
// oauth.MintEdgeClientSocketsToken under the test's svc- key; server.PlatformClientSource keeps them; an
// EdgeClientPool of its clients on a stand-in platform gives each a dispatcher in the edge role; and the block,
// served over HTTP, verifies with its own Authenticator against the test's jwks and the rev/ objects on fake blobs.
// Through the source's SocketFindHttpArgs and SocketPlatformRegistrationsArgs, what tlsedge's and mxedge's dialers
// and smtpedge's platform listener are given, a Find is answered as an urnetwork-edge caller's and each client's
// ur/smtpedge lease is granted; the client's jwt in the token's place is refused 401; and once the client's rev/
// object stands, its token is refused 401 at both routes.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/session"
)

// One answer sockets gave an edge call: its route, status and body.
type edgeTokenTestAnswer struct {
	path   string
	status int
	body   []byte
}

// A transport to the test's sockets server that keeps every answer, its body read whole and handed on.
type edgeTokenTestTransport struct {
	transport http.RoundTripper

	stateLock sync.Mutex
	answers   []*edgeTokenTestAnswer
}

func (self *edgeTokenTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := self.transport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.answers = append(self.answers, &edgeTokenTestAnswer{path: request.URL.Path, status: response.StatusCode, body: body})
	return response, nil
}

// The answers to path, in order.
func (self *edgeTokenTestTransport) answersTo(path string) []*edgeTokenTestAnswer {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	answers := []*edgeTokenTestAnswer{}
	for _, answer := range self.answers {
		if answer.path == path {
			answers = append(answers, answer)
		}
	}
	return answers
}

// A platform that opens each client in process, connected at once, as server's source tests do.
type edgeTokenTestPlatform struct{}

func (edgeTokenTestPlatform) OpenClient(ctx context.Context, args *sidecar.SidecarPlatformClientArgs) (*sidecar.SidecarPlatformClient, error) {
	client := connect.NewClient(ctx, args.ClientId, connect.NewNoContractClientOob(), args.ClientSettings)
	return &sidecar.SidecarPlatformClient{
		Client:    client,
		SetAuth:   func(auth *connect.ClientAuth) {},
		Connected: func() (bool, <-chan struct{}) { return true, nil },
		CloseAndWait: func(ctx context.Context) error {
			client.Cancel()
			return nil
		},
	}, nil
}

func TestHostTakesEdgeClientsSocketsTokens(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		installServiceTokenTestKeys(t)
		edgeNetworkId := server.NewId()
		adminUserId := server.NewId()
		world := newHostTestWorld(t)
		if err := world.gates.Set("sidecar:\n  sockets: {enabled: true, percent: 100}\n  urnetwork_edge: " + edgeNetworkId.String() + "\n"); err != nil {
			t.Fatal(err)
		}
		blobs := sidecartest.NewFakeBlobs(world.clock, sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
		host := world.newHost(t, func(deps *hostDeps) {
			deps.Authenticator = nil
			deps.Blobs = blobs
		})
		socketsServer := httptest.NewServer(host.Handler())
		t.Cleanup(socketsServer.Close)
		httpTransport := &http.Transport{}
		t.Cleanup(httpTransport.CloseIdleConnections)
		transport := &edgeTokenTestTransport{transport: httpTransport}

		// api's edge client route as the test stands it up: each ask's derived client, a real client jwt, and the
		// sockets token the route mints
		edgeBearer := "edge-bearer-secret"
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			args := &server.EdgeClientMintArgs{}
			if r.URL.Path != server.EdgeClientMintPath || r.Header.Get("Authorization") != "Bearer "+edgeBearer || json.NewDecoder(r.Body).Decode(args) != nil {
				http.Error(w, "Forbidden.", http.StatusForbidden)
				return
			}
			clientId := server.DeriveEdgeClientId("test", args.Host, args.Block, args.Slot, args.Index)
			deviceId := server.DeriveEdgeDeviceId("test", args.Host, args.Block, args.Slot, args.Index)
			byJwt := session.NewByJwt(edgeNetworkId, adminUserId, "urnetwork-edge-test", false, false).Client(deviceId, clientId)
			socketsToken, socketsTokenExpireTime, err := oauth.MintEdgeClientSocketsToken(edgeNetworkId, clientId)
			if err != nil {
				http.Error(w, "Unavailable.", http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(&server.EdgeClientMintResult{
				ClientId:                     clientId.String(),
				ByClientJwt:                  byJwt.Testing_Sign(),
				ExpireTimeMillis:             server.NowUtc().Add(time.Hour).UnixMilli(),
				PlatformUrl:                  "wss://connect.edge.example",
				SocketsToken:                 socketsToken,
				SocketsTokenExpireTimeMillis: socketsTokenExpireTime.UnixMilli(),
			})
		}))
		t.Cleanup(api.Close)

		clientCount := 2
		sourceSettings := server.DefaultPlatformClientSourceSettings()
		sourceSettings.ClientCount = clientCount
		source, err := server.NewPlatformClientSource(ctx, &server.PlatformClientSourceArgs{
			ApiUrl:   api.URL,
			Block:    "smtpedge",
			Host:     "lb-a.example",
			Bearer:   edgeBearer,
			Settings: sourceSettings,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(source.Close)
		poolSettings := sidecar.DefaultEdgeClientPoolSettings()
		poolSettings.ClientCount = clientCount
		poolSettings.AppVersion = "0.0.0-test"
		pool, err := sidecar.NewEdgeClientPool(ctx, &sidecar.EdgeClientPoolArgs{
			Source:   source,
			Platform: edgeTokenTestPlatform{},
			Settings: poolSettings,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)

		// smtpedge's ur/smtpedge: each client's lease, on its own sockets token
		registrationsArgs := source.SocketPlatformRegistrationsArgs(socketsServer.URL)
		registrationsArgs.Transport = transport
		listener, err := sidecar.NewSocketPlatformListener(ctx, &sidecar.SocketPlatformListenerArgs{
			Name:          "ur/smtpedge",
			UrnetworkEdge: sidecar.Id(edgeNetworkId),
			Clients:       pool.Clients(),
			Registrations: registrationsArgs,
			Clock:         world.clock,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := listener.AwaitRegistered(ctx); err != nil {
			t.Fatalf("an edge client's ur/smtpedge registration was not granted: %v", err)
		}
		listener.Close()
		registrations := transport.answersTo("/socket/registrations")
		if len(registrations) < clientCount {
			t.Fatalf("%d registrations answered for %d clients", len(registrations), clientCount)
		}
		for _, answer := range registrations {
			if answer.status != http.StatusOK {
				t.Fatalf("a registration answered %d", answer.status)
			}
		}

		// tlsedge's and mxedge's Find: the first client's sockets token, answered as an urnetwork-edge caller's. The
		// name's network has no view here, so it is unavailable; a caller sockets did not take as an edge would be
		// denied before the view is read
		findArgs := source.SocketFindHttpArgs(socketsServer.URL)
		findArgs.Transport = transport
		dialer, err := sidecar.NewSocketEdgeDialer(&sidecar.SocketEdgeDialerArgs{
			Clients: pool.Clients(),
			Find:    findArgs,
			Clock:   world.clock,
		})
		if err != nil {
			t.Fatal(err)
		}
		dialRequest := &sidecar.SocketEdgeDialRequest{
			NetworkId:    sidecar.Id(server.NewId()),
			NetworkLabel: "edgetest01",
			Name:         "tls/443/blog",
			Protocol:     "tls",
		}
		if _, err := dialer.Dial(ctx, dialRequest); err == nil {
			t.Fatal("a Find into a network with no view opened a stream")
		}
		finds := transport.answersTo("/socket/find")
		if len(finds) != 1 {
			t.Fatalf("%d Finds answered", len(finds))
		}
		findResult := &sidecar.SocketFindResult{}
		if finds[0].status != http.StatusOK || json.Unmarshal(finds[0].body, findResult) != nil || findResult.Error == nil ||
			findResult.Error.Code != sidecar.SocketErrorCodeUnavailable {
			t.Fatalf("the edge's Find answered %d %s", finds[0].status, finds[0].body)
		}

		// the client's jwt, the bearer an edge presented before QD-1029, is no token sockets takes
		jwtAuth, err := source.ServiceClient(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		findBody := &sidecar.SocketFindArgs{Name: "tls/443/blog", NetworkId: (*connect.Id)(&dialRequest.NetworkId)}
		if recorder := hostTestServe(host, http.MethodPost, "/socket/find", jwtAuth.ByJwt, findBody, ""); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("a Find on the client's jwt answered %d", recorder.Code)
		}
		if recorder := hostTestServe(host, http.MethodPost, "/socket/registrations", jwtAuth.ByJwt, &sidecar.SocketRegistrationsArgs{SetVersion: 1}, ""); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("a registration on the client's jwt answered %d", recorder.Code)
		}

		// the first client removed: its client rev/ object, as api's edge client removal effect owes it, voids the
		// token it holds once the block's 5 s answer for it lapses
		firstClientId := pool.ClientIds()[0]
		revokeServiceTokenTestSubject(t, blobs, edgeNetworkId, sidecar.RevocationKindClient, server.Id(firstClientId), server.NowUtc().Add(time.Minute))
		world.clock.Advance(defaultHostSettings().Revocation.CacheTimeout)
		if _, err := dialer.Dial(ctx, dialRequest); err == nil {
			t.Fatal("a revoked client's Find opened a stream")
		}
		finds = transport.answersTo("/socket/find")
		if len(finds) != 2 || finds[1].status != http.StatusUnauthorized {
			t.Fatalf("a revoked client's Find answered %v", finds)
		}
		revokedToken, err := registrationsArgs.Bearer(ctx, firstClientId)
		if err != nil {
			t.Fatal(err)
		}
		if recorder := hostTestServe(host, http.MethodPost, "/socket/registrations", revokedToken, &sidecar.SocketRegistrationsArgs{SetVersion: 1}, ""); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("a revoked client's registration answered %d", recorder.Code)
		}
		// the other client, not removed, still registers on its own token
		otherToken, err := registrationsArgs.Bearer(ctx, pool.ClientIds()[1])
		if err != nil {
			t.Fatal(err)
		}
		if recorder := hostTestServe(host, http.MethodPost, "/socket/registrations", otherToken, &sidecar.SocketRegistrationsArgs{SetVersion: 1}, ""); recorder.Code != http.StatusOK {
			t.Fatalf("the other client's registration answered %d: %s", recorder.Code, recorder.Body.String())
		}
	})
}
