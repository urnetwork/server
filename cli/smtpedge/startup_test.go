package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"
)

type startupSmtpClients struct {
	client *sidecar.SocketEdgeClient
}

func (self startupSmtpClients) Clients() []*sidecar.SocketEdgeClient {
	return []*sidecar.SocketEdgeClient{self.client}
}
func (self startupSmtpClients) ClientIds() []sidecar.Id { return []sidecar.Id{self.client.ClientId} }
func (self startupSmtpClients) Ready() bool             { return true }
func (self startupSmtpClients) Close()                  {}

// The registrations endpoint of this test. It receives ordinary requests entirely in memory.
type startupSmtpRegistrations struct {
	registered chan struct{}
	edge       connect.Id
}

func (self *startupSmtpRegistrations) RoundTrip(request *http.Request) (*http.Response, error) {
	defer request.Body.Close()
	page := &sidecar.SocketRegistrationsArgs{}
	if err := json.NewDecoder(request.Body).Decode(page); err != nil {
		return nil, err
	}
	reply := &sidecar.SocketRegistrationsResult{SetVersion: page.SetVersion, UrnetworkEdge: &self.edge}
	for _, entry := range page.Registrations {
		reply.Statuses = append(reply.Statuses, sidecar.SocketRegistrationStatusOk)
		if entry.Name == smtpedgeservice.SmtpEdgeSocketName {
			select {
			case self.registered <- struct{}{}:
			default:
			}
		}
	}
	body, err := json.Marshal(reply)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
}

// Stock block composition registers its platform service and reaches readiness after validating signed PTR and A.
func TestHostGateAndMxResolverReachBlockReadiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fakes := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	edge := connect.NewId()
	if err := fakes.Gates.Set("sidecar:\n  urnetwork_edge: " + edge.String() + "\n"); err != nil {
		t.Fatal(err)
	}
	config, err := smtpedgeservice.LoadSmtpEdgeConfig([]byte("root: root.example\nreport_host: rpt.report.example\nhelo_domain: out.report.example\nsending_addresses:\n  - {address: 192.0.2.10, helo: o1.out.report.example}\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	config.Settings.XUrRefKeys = []*smtpedgeservice.SmtpXUrRefKey{{KeyId: 1, Key: bytes.Repeat([]byte{1}, 32)}}
	worldSettings := sidecartest.DefaultMxWorldSettings()
	worldSettings.Clock = clock
	world, err := sidecartest.NewMxWorld(worldSettings)
	if err != nil {
		t.Fatal(err)
	}
	address := netip.MustParseAddr("192.0.2.10")
	const helo = "o1.out.report.example"
	if err := world.AddMx(helo, address, nil); err != nil {
		t.Fatal(err)
	}
	world.SetPtr(address, helo)
	anchors, err := sidecar.NewSmtpDnssecAnchors(world.TrustAnchorDs())
	if err != nil {
		t.Fatal(err)
	}
	resolver := sidecar.NewSmtpMxDnsResolver(world, anchors, nil, clock)
	id := connect.NewId()
	clientSettings := connect.DefaultClientSettings()
	sidecar.ApplySidecarClientSettings(clientSettings)
	client := connect.NewClient(ctx, id, connect.NewNoContractClientOob(), clientSettings)
	defer client.Cancel()
	protocolSettings := sidecar.DefaultSidecarProtocolSettings()
	dispatcher, err := sidecar.NewSubprotocol(ctx, &sidecar.SubprotocolEnv{Client: client, Role: sidecar.SubprotocolRoleEdge,
		Inbound: sidecar.SubprotocolInboundWithoutPolicy, Settings: protocolSettings,
		Budget: sidecar.NewSubprotocolBudget(protocolSettings, 128*1024*1024), Clock: clock, Log: sidecar.NewNoopLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer dispatcher.Close()
	pool := startupSmtpClients{client: &sidecar.SocketEdgeClient{ClientId: sidecar.Id(id), Subprotocol: dispatcher}}
	registrations := &startupSmtpRegistrations{registered: make(chan struct{}, 1), edge: edge}
	block, err := newSmtpEdgeHost(ctx, defaultHostSettings(config), &hostDeps{
		Quota: fakes.Quota, Abuse: fakes.Abuse, Store: sidecartest.NewFakeSmtpEdgeStore(clock, nil),
		Projection: sidecar.NewAclGateDirectSource(fakes.Redis, fakes.Blobs, nil),
		Facts:      &testGateSource{loads: map[sidecar.Id]int{}, breaker: &smtpedgeservice.SmtpBreakerState{}},
		Resolver:   resolver, Ptr: resolver, Dialer: world, Verifier: sidecartest.NewFakeSmtpProofVerifier(nil),
		NewClients: func(context.Context) (hostClients, error) { return pool, nil },
		Registrations: &sidecar.SocketPlatformRegistrationsArgs{SocketsUrl: "https://sockets.example", Transport: registrations,
			Bearer: func(context.Context, sidecar.Id) (string, error) { return "fixture", nil }},
		Gates: fakes.Gates, Redis: fakes.Redis, Metrics: fakes.Metrics, Clock: clock, Log: &testLog{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer block.Close()
	if missing := block.Missing(); len(missing) != 0 {
		t.Fatalf("missing startup dependencies %v", missing)
	}
	select {
	case <-registrations.registered:
	case <-ctx.Done():
		t.Fatal("the block did not register its SMTP service")
	}
	for {
		_, changed := clock.WatchTimers()
		if block.Ready() {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("the registered block did not become ready")
		}
	}
	block.Drain()
	if block.Ready() {
		t.Fatal("the drained block remained ready")
	}
}
