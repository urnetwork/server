package main

// The block's production internal-gate source talks to the registered API router
// entirely in process. Only the authoritative facts model is replaced in this test.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	connectprotocol "github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// Runs a request through the router without a network listener.
type listenFactsTransport struct{ http.Handler }

func (self listenFactsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	self.ServeHTTP(response, r)
	return response.Result(), nil
}

// A synthetic credential for each class, checked by the real internal route.
func listenFactsClient(t testing.TB, source socketsservice.TlsListenFactsSource) (*http.Client, string, string) {
	t.Helper()
	socketsSecret := sha256.Sum256([]byte("synthetic sockets credential"))
	otherSecret := sha256.Sum256([]byte("synthetic connect credential"))
	socketsBearer := base64.RawURLEncoding.EncodeToString(socketsSecret[:])
	otherBearer := base64.RawURLEncoding.EncodeToString(otherSecret[:])
	verifier, err := server.NewInternalGateVerifier([]byte(fmt.Sprintf("sockets: {class: sockets, sha256: [%x]}\nconnect: {class: connect, sha256: [%x]}\n", sha256.Sum256([]byte(socketsBearer)), sha256.Sum256([]byte(otherBearer)))))
	if err != nil {
		t.Fatal(err)
	}
	gate := handlers.NewInternalGateProjectionHandlers(&handlers.InternalGateProjectionDeps{Verifier: verifier, ListenFacts: source})
	return &http.Client{Transport: listenFactsTransport{Handler: router.NewRouter(t.Context(), gate.Routes())}}, socketsBearer, otherBearer
}

// The API verifies the class before reading network facts. Missing rows cannot yield a cached positive answer.
func TestSocketBlockLoadsAuthoritativeListenFacts(t *testing.T) {
	networkId, deviceId := connect.NewId(), connect.NewId()
	expires := time.Unix(2000000000, 0).UTC()
	reads := 0
	source := socketsservice.TlsListenFactsFunc(func(ctx context.Context, id connect.Id) (*socketsservice.TlsListenFacts, error) {
		reads++
		if id != networkId {
			return nil, model.ErrTlsListenFactsNetworkNotFound
		}
		return &socketsservice.TlsListenFacts{MailNetwork: true, Verified: true, Certs: []*socketsservice.TlsListenCert{{SidecarId: deviceId, NetworkPair: true, NotAfter: expires}}}, nil
	})
	client, socketsBearer, otherBearer := listenFactsClient(t, source)
	deps := &hostDeps{}
	for _, bearer := range []string{otherBearer, socketsBearer} {
		if err := deps.setGateApiSources("https://api.example", bearer, client, defaultHostSettings().ListenFactsMaxByteCount); err != nil {
			t.Fatal(err)
		}
		facts, err := deps.TlsListenFacts.ListenFacts(t.Context(), networkId)
		if bearer != socketsBearer {
			if err == nil || facts != nil || reads != 0 {
				t.Fatalf("other service read facts %v, %v", facts, err)
			}
		} else if err != nil || facts == nil || !facts.MailNetwork || !facts.Verified || !facts.Certified(deviceId, "@", expires.Add(-time.Second)) {
			t.Fatalf("sockets facts = %v, %v", facts, err)
		}
	}
	if facts, err := deps.TlsListenFacts.ListenFacts(t.Context(), connect.NewId()); err == nil || facts != nil {
		t.Fatalf("unknown network returned %v, %v", facts, err)
	}
}

// The stock sockets host receives signed device claims and issues edge-to-device
// tickets for both public namespaces, using its verified grant and live facts sources.
func TestSocketBlockPublishesTlsAndSmtpToEdge(t *testing.T) {
	world := newT3WorldWith(t, func(now time.Time) []string {
		return []string{
			fmt.Sprintf(`{"src":["autogroup:member"],"dst":["dns:blog","sock:tls/443/blog"],"allow":["publish","listen"],"audience":"public","expires":%q}`, now.Add(time.Hour).UTC().Format(time.RFC3339)),
			`{"src":["autogroup:member"],"dst":["sock:smtp/25/mx"],"allow":["listen"]}`,
		}
	}, func(world *t3World, deps *hostDeps) {
		deps.Redis = sidecartest.NewFakeRedis(t.Context(), world.world.clock, world.world.faults, nil, nil)
		deps.Store = sidecartest.NewFakeSocketStore(world.world.clock, nil, world.world.faults)
		facts := &socketsservice.TlsListenFacts{
			Verified: true, Standing: sidecar.AbuseStandingStandard, Tier: "team", MailNetwork: true,
			Certs: []*socketsservice.TlsListenCert{{SidecarId: connect.Id(world.listener.device.SidecarId), NotAfter: world.world.clock.Now().Add(time.Hour), NetworkPair: true}},
		}
		client, bearer, _ := listenFactsClient(t, socketsservice.TlsListenFactsFunc(func(ctx context.Context, id connect.Id) (*socketsservice.TlsListenFacts, error) {
			if id != world.networkId {
				return nil, model.ErrTlsListenFactsNetworkNotFound
			}
			return facts, nil
		}))
		apiDeps := &hostDeps{}
		if err := apiDeps.setGateApiSources("https://api.example", bearer, client, defaultHostSettings().ListenFactsMaxByteCount); err != nil {
			t.Fatal(err)
		}
		deps.TlsListenFacts = apiDeps.TlsListenFacts
	})
	if !world.host.Ready() {
		t.Fatal("the fully configured block is not ready")
	}
	metadata := world.register(world.dialer, 1, nil, nil, []*sidecar.SocketRegistration{})
	if metadata.Error != nil || metadata.UrnetworkEdge == nil || *metadata.UrnetworkEdge != world.edgeNetworkId || len(metadata.TicketKeys) == 0 {
		t.Fatal("an empty listener set did not bootstrap the edge id and ticket keys")
	}
	if err := world.host.tlsHooks.LoadFacts(t.Context(), world.networkId); err != nil {
		t.Fatal(err)
	}
	// Both clients run connect's Required transport on this world's in-process exchange.
	dispatcher := func(name string, role sidecar.SubprotocolRole) (*sidecartest.NetClient, *sidecar.Subprotocol) {
		t.Helper()
		client := world.net.AddClient(name, nil)
		settings := sidecar.DefaultEdgeClientPoolSettings()
		dispatch, err := sidecar.NewSubprotocol(t.Context(), &sidecar.SubprotocolEnv{
			Client: client.Client, Store: sidecartest.NewFakeStore().Root(), Log: sidecar.NewNoopLogger(),
			Settings: settings.Protocol, Budget: sidecar.NewSubprotocolBudget(settings.Protocol, settings.MemoryBudgetByteCount),
			Inbound: sidecar.SubprotocolInboundWithoutPolicy, Role: role,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(dispatch.Close)
		return client, dispatch
	}
	deviceClient, deviceDispatch := dispatcher("registered listener", sidecar.SubprotocolRoleSidecar)
	edgeClient, edgeDispatch := dispatcher("socket edge", sidecar.SubprotocolRoleEdge)
	edgeId := edgeClient.ClientId
	edgeClient.Client.ContractManager().SetProvideModes(map[connectprotocol.ProvideMode]bool{connectprotocol.ProvideMode_Service: true})
	release, err := deviceDispatch.HoldProvideMode(connectprotocol.ProvideMode_Service)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	world.net.SetContractProvideMode(edgeId, deviceClient.ClientId, connectprotocol.ProvideMode_Service)
	world.net.SetContractProvideMode(deviceClient.ClientId, edgeId, connectprotocol.ProvideMode_Service)
	now := world.world.clock.Now()
	deviceId := connect.Id(world.listener.device.SidecarId)
	world.listener.clientId = deviceClient.ClientId
	world.listener.token = world.world.authenticator.Issue(&sidecar.Principal{
		NetworkId: world.networkId, ClientId: deviceClient.ClientId, SidecarId: &deviceId,
		Audience: socketsservice.SocketsAudience, IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	custom := &t3CustomWorld{t3World: world}
	tls := custom.blogRegistration(world.listener, nil)
	smtpId := connect.NewId()
	smtp := &sidecar.SocketRegistration{Name: "smtp/25/mx", Protocol: "smtp-starttls", ListenId: &smtpId, Weight: 100, MaxStreams: 32, Serving: true}
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(&protocol.SocketRegistrationClaim{
		NetworkId: world.networkId.Bytes(), Name: smtp.Name, ListenId: smtpId.Bytes(), Protocol: smtp.Protocol,
	})
	if err != nil {
		t.Fatal(err)
	}
	smtp.Claim, err = proto.MarshalOptions{Deterministic: true}.Marshal(&bridge.Signed{
		Type: uint32(bridge.SignedType_SIGNED_SOCKET_REGISTRATION), Body: body,
		Signature: ed25519.Sign(world.listener.device.Key, bridge.SignedBytes(sidecar.SocketRegistrationPurpose, bridge.SignedType_SIGNED_SOCKET_REGISTRATION, body)),
	})
	if err != nil {
		t.Fatal(err)
	}
	world.registerOk(world.listener, 1, tls, smtp)
	edgeToken := world.world.authenticator.Issue(&sidecar.Principal{
		NetworkId: world.edgeNetworkId, ClientId: edgeId, Audience: socketsservice.SocketsAudience, IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	edge, err := sidecar.NewSocketEdgeDialer(&sidecar.SocketEdgeDialerArgs{
		Clients: []*sidecar.SocketEdgeClient{{ClientId: sidecar.Id(edgeId), Subprotocol: edgeDispatch}},
		Find: &sidecar.SocketFindHttpArgs{SocketsUrl: "https://sockets.example",
			Transport: listenFactsTransport{world.host.Handler()}, Bearer: func(context.Context) (string, error) { return edgeToken, nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []*sidecar.SocketRegistration{tls, smtp} {
		query := &sidecar.SocketFindArgs{Name: entry.Name, Protocol: entry.Protocol, Alpn: entry.Alpn, NetworkId: &world.networkId, ClientIds: []*connect.Id{&edgeId}}
		result := hostTestServe(world.host, http.MethodPost, "/socket/find", edgeToken, query, "")
		if clients := custom.ticketedListeners(result.Code, result.Body.Bytes()); !slices.Equal(clients, []connect.Id{world.listener.clientId}) {
			t.Fatalf("edge ticket for %s did not identify the registered device", entry.Protocol)
		}
		var answer sidecar.SocketFindResult
		if err := json.Unmarshal(result.Body.Bytes(), &answer); err != nil || !slices.Equal(answer.Primary.Claim, entry.Claim) {
			t.Fatal("the edge did not receive the device's registration claim")
		}
		incoming := make(chan *sidecar.SubprotocolStream, 1)
		stop, err := deviceDispatch.Listen("abcdefghij:"+entry.Name, func(ctx context.Context, open *sidecar.StreamIncoming) error {
			ticket, err := sidecar.VerifySocketTicket(open.Ticket, metadata.TicketKeys, uint64(now.UnixMilli()))
			if err != nil || !open.Edge || open.SourceId != sidecar.Id(edgeId) || ticket.GetName() != entry.Name ||
				!slices.Equal(ticket.GetListenId(), entry.ListenId.Bytes()) || !slices.Equal(ticket.GetListenerClientId(), deviceClient.ClientId.Bytes()) {
				return fmt.Errorf("the device received no matching signed edge ticket")
			}
			incoming <- open.Stream
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		dialed, err := edge.Dial(ctx, &sidecar.SocketEdgeDialRequest{
			NetworkId: sidecar.Id(world.networkId), NetworkLabel: "abcdefghij", Name: entry.Name, Protocol: entry.Protocol, Alpns: entry.Alpn,
			EarlyData: []byte(entry.Protocol),
		})
		if err != nil {
			t.Fatalf("opening %s from the edge: %v", entry.Name, err)
		}
		defer dialed.Close()
		var accepted *sidecar.SubprotocolStream
		select {
		case accepted = <-incoming:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		defer accepted.Close()
		deadline, _ := ctx.Deadline()
		accepted.SetReadDeadline(deadline)
		data := make([]byte, len(entry.Protocol))
		if _, err := io.ReadFull(accepted, data); err != nil || string(data) != entry.Protocol {
			t.Fatalf("the device received %q as the edge's first bytes: %v", data, err)
		}
	}
}
