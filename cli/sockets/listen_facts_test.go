package main

// The block's production internal-gate source talks to the registered API router
// entirely in process. Only the authoritative facts model is replaced in this test.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	"github.com/urnetwork/sidecar/tlsedgeservice"

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
	// Both clients install the sidecar Service subprotocol on their Required transport.
	clientSettings := connect.DefaultClientSettings()
	sidecar.ApplySidecarClientSettings(clientSettings)
	deviceClient := world.net.AddClient("registered listener", clientSettings)
	settings := sidecar.DefaultEdgeClientPoolSettings()
	deviceDispatch, err := sidecar.NewSubprotocol(t.Context(), &sidecar.SubprotocolEnv{
		Client: deviceClient.Client, Store: sidecartest.NewFakeStore().Root(), Log: sidecar.NewNoopLogger(),
		Settings: settings.Protocol, Budget: sidecar.NewSubprotocolBudget(settings.Protocol, settings.MemoryBudgetByteCount),
		Inbound: sidecar.SubprotocolInboundWithoutPolicy, Role: sidecar.SubprotocolRoleSidecar,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceDispatch.Close)
	poolArgs, err := world.net.EdgeClientPoolArgs(sidecar.Id(world.edgeNetworkId))
	if err != nil {
		t.Fatal(err)
	}
	settings.ClientCount = 1
	poolArgs.Settings = settings
	pool, err := sidecar.NewEdgeClientPool(t.Context(), poolArgs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	edgeClient := pool.Clients()[0]
	edgeId := connect.Id(edgeClient.ClientId)
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
	tlsRegistration := custom.blogRegistration(world.listener, nil)
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
	world.registerOk(world.listener, 1, tlsRegistration, smtp)
	edgeToken := world.world.authenticator.Issue(&sidecar.Principal{
		NetworkId: world.edgeNetworkId, ClientId: edgeId, Audience: socketsservice.SocketsAudience, IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	findTransport := &edgeTokenTestTransport{transport: listenFactsTransport{Handler: world.host.Handler()}}
	edge, err := sidecar.NewSocketEdgeDialer(&sidecar.SocketEdgeDialerArgs{
		Clients: pool.Clients(),
		Find: &sidecar.SocketFindHttpArgs{SocketsUrl: "https://sockets.example",
			Transport: findTransport, Bearer: func(context.Context) (string, error) { return edgeToken, nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, entry := range []*sidecar.SocketRegistration{tlsRegistration, smtp} {
		incoming := make(chan *sidecar.StreamIncoming, 1)
		stop, err := deviceDispatch.Listen("abcdefghij:"+entry.Name, func(ctx context.Context, open *sidecar.StreamIncoming) error {
			ticket, err := sidecar.VerifySocketTicket(open.Ticket, metadata.TicketKeys, uint64(now.UnixMilli()))
			if err != nil || !open.Edge || open.SourceId != edgeClient.ClientId || ticket.GetName() != entry.Name ||
				!slices.Equal(ticket.GetNetworkId(), world.networkId.Bytes()) || !slices.Equal(ticket.GetDialerNetworkId(), world.edgeNetworkId.Bytes()) ||
				!slices.Equal(ticket.GetDialerClientId(), edgeId.Bytes()) ||
				!slices.Equal(ticket.GetListenId(), entry.ListenId.Bytes()) || !slices.Equal(ticket.GetListenerClientId(), deviceClient.ClientId.Bytes()) {
				return fmt.Errorf("the device received no matching signed edge ticket")
			}
			select {
			case incoming <- open:
				return nil
			default:
				return errors.New("the device received a second open before the first was taken")
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		// Both fronts forward the visitor's original TLS records, after STARTTLS for SMTP.
		visitor, front := net.Pipe()
		handshakeDone := make(chan struct{})
		go func() {
			defer close(handshakeDone)
			defer visitor.Close()
			tls.Client(visitor, &tls.Config{ServerName: "visitor.example", NextProtos: entry.Alpn}).HandshakeContext(ctx)
		}()
		hello, err := tlsedgeservice.ReadClientHello(front, tlsedgeservice.DefaultTlsEdgeSettings().ClientHelloMaxByteCount)
		front.Close()
		<-handshakeDone
		if err != nil {
			t.Fatal(err)
		}
		dialed, err := edge.Dial(ctx, &sidecar.SocketEdgeDialRequest{
			NetworkId: sidecar.Id(world.networkId), NetworkLabel: "abcdefghij", Name: entry.Name, Protocol: entry.Protocol, Alpns: entry.Alpn,
			EarlyData: hello.RawBytes,
		})
		if err != nil {
			t.Fatalf("opening %s from the edge: %v (listener Service drops %d, edge inbound refusals %d)", entry.Name, err,
				deviceClient.Client.SubprotocolStats().DroppedServiceSubprotocol, edgeClient.Subprotocol.Stats().InboundRefusedCount)
		}
		defer dialed.Close()
		var accepted *sidecar.StreamIncoming
		select {
		case accepted = <-incoming:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		defer accepted.Stream.Close()
		finds := findTransport.answersTo("/socket/find")
		if len(finds) != index+1 {
			t.Fatalf("opening %s took %d Finds", entry.Name, len(finds)-index)
		}
		result := finds[index]
		if clients := custom.ticketedListeners(result.status, result.body); !slices.Equal(clients, []connect.Id{world.listener.clientId}) {
			t.Fatalf("edge ticket for %s did not identify the registered device", entry.Protocol)
		}
		var answer sidecar.SocketFindResult
		if err := json.Unmarshal(result.body, &answer); err != nil || answer.Primary == nil ||
			!slices.Equal(answer.Primary.Claim, entry.Claim) || !slices.Equal(answer.Primary.Ticket, accepted.Ticket) {
			t.Fatal("the registration claim or the open's ticket differed from the Find reply")
		}
		deadline, _ := ctx.Deadline()
		accepted.Stream.SetReadDeadline(deadline)
		data := make([]byte, len(hello.RawBytes))
		if _, err := io.ReadFull(accepted.Stream, data); err != nil || !slices.Equal(data, hello.RawBytes) {
			t.Fatalf("the device did not receive the original %s ClientHello: %v", entry.Protocol, err)
		}
		dialed.SetReadDeadline(deadline)
		if _, err := accepted.Stream.Write([]byte(entry.Name)); err != nil {
			t.Fatal(err)
		}
		reply := make([]byte, len(entry.Name))
		if _, err := io.ReadFull(dialed, reply); err != nil || string(reply) != entry.Name {
			t.Fatalf("the edge received %q as the device's first bytes: %v", reply, err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err = deviceDispatch.OpenStream(ctx, edgeClient.ClientId, &sidecar.StreamOpenRequest{Target: "ur/smtpedge", Edge: true})
	var rejection *sidecar.StreamRejectError
	if !errors.As(err, &rejection) || rejection.Code != protocol.StreamRejectCode_SR_DENIED {
		t.Fatalf("the dial-only edge answered a new inbound stream with %v", err)
	}
}
