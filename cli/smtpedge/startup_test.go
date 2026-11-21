package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	connectprotocol "github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"
)

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
	clientsNet := sidecartest.NewNet(ctx, nil)
	defer clientsNet.Close()
	poolArgs, err := clientsNet.EdgeClientPoolArgs(sidecar.Id(edge))
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultHostSettings(config)
	poolArgs.Settings = settings.Clients
	pool, err := newSmtpEdgeClients(ctx, *poolArgs)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	senders, _, err := clientsNet.AddSubprotocolSidecars("sender")
	if err != nil {
		t.Fatal(err)
	}
	sender := senders[0]
	edgeId := pool.ClientIds()[0]
	clientsNet.SetContractProvideMode(sender.ClientId(), connect.Id(edgeId), connectprotocol.ProvideMode_Service)
	clientsNet.SetContractProvideMode(connect.Id(edgeId), sender.ClientId(), connectprotocol.ProvideMode_Service)
	release, err := sender.Subprotocol().HoldProvideMode(connectprotocol.ProvideMode_Service)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	registrations := &startupSmtpRegistrations{registered: make(chan struct{}, 1), edge: edge}
	block, err := newSmtpEdgeHost(ctx, settings, &hostDeps{
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
	// Registration and RPC handler installation run concurrently. The live engine answers a handshake
	// naming no active send with its typed result once its handler has been installed.
	for {
		body, err := sender.Subprotocol().Call(ctx, edgeId, protocol.MessageType_SMTP_HANDSHAKE, nil, &sidecar.CallOptions{Edge: true})
		var callError *sidecar.SubprotocolError
		if errors.As(err, &callError) && callError.Code == protocol.ErrorCode_ERR_UNIMPLEMENTED && ctx.Err() == nil {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var reply protocol.SmtpHandshakeResult
		if err := proto.Unmarshal(body, &reply); err != nil || reply.GetCode() != uint32(protocol.SmtpHandshakeCode_SMTP_HANDSHAKE_INVALID) {
			t.Fatalf("the registered SMTP handler answered %v, %v", &reply, err)
		}
		break
	}
	_, err = sender.Subprotocol().OpenStream(ctx, edgeId, &sidecar.StreamOpenRequest{Target: smtpedgeservice.SmtpEdgeSocketName, Edge: true})
	var rejection *sidecar.StreamRejectError
	if !errors.As(err, &rejection) || rejection.Code != protocol.StreamRejectCode_SR_INVALID {
		t.Fatalf("the SMTP listener's ticket check answered %v", err)
	}
	block.Drain()
	if block.Ready() {
		t.Fatal("the drained block remained ready")
	}
}
