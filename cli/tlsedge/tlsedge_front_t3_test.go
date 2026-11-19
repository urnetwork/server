//go:build sidecar_pending && sidecar_testhooks

package main

// FS-34 on T3 (TLS-M5-26; FS-34, FI-11, D-TLS-13, §13.7): tlsedge_t3_test.go's block on the private stack's Postgres
// and Redis in front of §13.7's production front, one for each published host, each holding the device's own
// certificate and judging the audience from a policy the test fixes, as the device's own evaluation of its config
// would answer. The stream the block opens is handed to the front through sidecar.TlsFrontTestListener, which stands in
// for §9.6's admission on the ticket the dial names, so only the sidecar_testhooks build has this file. Behind each front
// a service counts every connection the front makes to it.
//
// FS-34: for a members, members-device or link host, http or raw, whatever ALPN the visitor offers (none, an unknown
// one, http/1.1, h2) and whatever it writes, no connection and no byte reaches the service, while the front did take the
// visitor's stream; the public raw host's service is reached beside them, so a path nothing reaches does not pass for
// nothing. TLS-ALPN-01's half of FS-34 runs on T2 (the front's acme responder) and on T3 through the real splitter
// (TLS-M6-07).

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
)

// The edge network every ticket names, which the fronts' test listener takes as urnetwork-edge.
var t3FrontEdgeNetworkId = sidecar.NewId()

// A policy the test fixes: each tls/ name's grant, with no member, device or link signer known.
type t3FrontPolicy struct {
	grants map[string]*sidecar.TlsPublishGrant
}

func (self *t3FrontPolicy) PublishGrant(name string, host string, now time.Time) *sidecar.TlsPublishGrant {
	return self.grants[name]
}

func (self *t3FrontPolicy) PublishGrantOf(signerId sidecar.Id, grantId []byte, name string, host string, now time.Time) *sidecar.TlsPublishGrant {
	return nil
}

func (self *t3FrontPolicy) MemberActive(memberId sidecar.Id, now time.Time) bool { return false }
func (self *t3FrontPolicy) IdentityKey(sidecarId sidecar.Id) []byte              { return nil }
func (self *t3FrontPolicy) DeviceConsented(sidecarId sidecar.Id, memberId sidecar.Id, now time.Time) bool {
	return false
}

// The keychain namespace a front seals sessions under, in memory.
type t3FrontSecrets struct {
	stateLock sync.Mutex
	values    map[string][]byte
}

func (self *t3FrontSecrets) Get(name string) ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	value, ok := self.values[name]
	if !ok {
		return nil, sidecar.ErrSidecarStoreAbsent
	}
	return append([]byte{}, value...), nil
}

func (self *t3FrontSecrets) Put(name string, value []byte) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.values[name] = append([]byte{}, value...)
	return nil
}

func (self *t3FrontSecrets) Delete(name string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.values, name)
	return nil
}

func (self *t3FrontSecrets) List(prefix string) ([]string, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	names := []string{}
	for name := range self.values {
		if len(prefix) <= len(name) && name[:len(prefix)] == prefix {
			names = append(names, name)
		}
	}
	return names, nil
}

// The device's end of a stream as a front reads it: the device conn and a reset that ends both directions.
type t3FrontStream struct {
	*t3EdgeDeviceConn
}

func (self *t3FrontStream) Reset(code protocol.StreamResetCode) error {
	return self.t3EdgeDeviceConn.Close()
}

// One published host: its front and the count of connections the front made to its service.
type t3FrontEndpoint struct {
	listener *sidecar.TlsFrontTestListener
	dials    atomic.Int64
	accepted atomic.Int64
}

// What one published host is.
type t3FrontPublish struct {
	host     string
	audience protocol.AclAudience
	appMode  protocol.AclAppMode
}

// Fronts for publishes behind the T3 block: the dial hands each stream to the front of its ticket's name.
func newT3FrontWorld(t testing.TB, publishes ...t3FrontPublish) (*t3EdgeWorld, map[string]*t3FrontEndpoint) {
	t.Helper()
	// one visitor opens every connection inside one second of the fake clock, which §13.9's visitor caps would refuse
	none := int64(-1)
	world := newT3EdgeWorld(t, func(settings *hostSettings) {
		settings.TlsEdge.ColdLimits = &tlsedgeservice.TlsEndpointLimits{
			EndpointConcurrent: none, EndpointNewPerS: none, EndpointMbitPerS: none,
			VisitorConcurrent: none, VisitorNewPerS: none, VisitorAggregateMultiple: none,
		}
	})
	world.fakes.Quota.SetLimits(connect.Id(world.networkId), &sidecar.QuotaLimits{Tier: "team", Limits: map[string]int64{
		"tls_endpoint_concurrent":        none,
		"tls_endpoint_new_per_s":         none,
		"tls_endpoint_mbit_per_s":        none,
		"tls_visitor_concurrent":         none,
		"tls_visitor_new_per_s":          none,
		"tls_visitor_aggregate_multiple": none,
	}})
	policy := &t3FrontPolicy{grants: map[string]*sidecar.TlsPublishGrant{}}
	endpoints := map[string]*t3FrontEndpoint{}
	for _, publish := range publishes {
		name := "tls/443/" + publish.host
		policy.grants[name] = &sidecar.TlsPublishGrant{
			GrantIds: [][]byte{[]byte(publish.host)},
			Audience: publish.audience,
			AppMode:  publish.appMode,
			Expires:  world.start.Add(24 * time.Hour),
		}
		endpoint := &t3FrontEndpoint{listener: sidecar.NewTlsFrontTestListener(t3FrontEdgeNetworkId)}
		appMode := "http"
		// the service is DialTarget's, so the address is one nothing dials
		target := &sidecar.TlsFrontTarget{Name: name, ServerNames: []string{world.serverName(publish.host)}, AppMode: appMode, LocalAddress: "127.0.0.1:9"}
		if publish.appMode == protocol.AclAppMode_ACL_APP_MODE_RAW {
			target.AppMode = "raw"
			target.Alpn = []string{"h2", "http/1.1"}
		}
		settings := sidecar.DefaultTlsFrontSettings()
		// the probe would dial the service, which here must see a visitor's bytes alone
		settings.TargetProbeInterval = 0
		front, err := sidecar.NewTlsFront(world.ctx, &sidecar.TlsFrontArgs{
			Settings:  settings,
			Target:    target,
			Listener:  endpoint.listener,
			NetworkId: world.networkId,
			SidecarId: sidecar.NewId(),
			Policy:    policy,
			Certificate: func(serverName string) (*tls.Certificate, error) {
				return &world.device.certificate, nil
			},
			Secrets:   &t3FrontSecrets{values: map[string][]byte{}},
			IssuerUrl: "https://issuer.example",
			Limits: func(ctx context.Context) (*sidecar.QuotaLimits, error) {
				return &sidecar.QuotaLimits{Tier: "pro"}, nil
			},
			DialTarget: func(ctx context.Context) (net.Conn, error) {
				endpoint.dials.Add(1)
				service, peer := net.Pipe()
				go func() {
					defer service.Close()
					io.Copy(service, service)
				}()
				return peer, nil
			},
			Clock: world.clock,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { front.Close() })
		endpoints[name] = endpoint
	}
	world.dial.serve = func(conn *t3EdgeDeviceConn, opened *t3EdgeOpened) {
		endpoint, ok := endpoints[opened.request.Name]
		if !ok {
			conn.Close()
			return
		}
		ticket := &protocol.SocketTicket{
			DialerNetworkId:       t3FrontEdgeNetworkId.Bytes(),
			GrantExpireTimeMillis: world.dial.expireTimeMillis,
		}
		if endpoint.listener.Deliver(&t3FrontStream{t3EdgeDeviceConn: conn}, ticket, opened.metadata) {
			endpoint.accepted.Add(1)
		}
	}
	return world, endpoints
}

// FS-34 on T3: no connection and no byte reaches a private host's service through the block, whatever ALPN the visitor
// offers or writes, the front having taken each stream, while the public raw host's service is reached.
func TestT3Fs34NoByteReachesAPrivateAudiencesService(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		privates := []t3FrontPublish{
			{host: "members", audience: protocol.AclAudience_ACL_AUDIENCE_MEMBERS, appMode: protocol.AclAppMode_ACL_APP_MODE_HTTP},
			{host: "devices", audience: protocol.AclAudience_ACL_AUDIENCE_MEMBERS_DEVICE, appMode: protocol.AclAppMode_ACL_APP_MODE_HTTP},
			{host: "linked", audience: protocol.AclAudience_ACL_AUDIENCE_LINK, appMode: protocol.AclAppMode_ACL_APP_MODE_HTTP},
			{host: "linkraw", audience: protocol.AclAudience_ACL_AUDIENCE_LINK, appMode: protocol.AclAppMode_ACL_APP_MODE_RAW},
		}
		public := t3FrontPublish{host: "open", audience: protocol.AclAudience_ACL_AUDIENCE_PUBLIC, appMode: protocol.AclAppMode_ACL_APP_MODE_RAW}
		world, endpoints := newT3FrontWorld(t, append(privates, public)...)
		visits := 0
		for _, publish := range privates {
			for _, alpnIds := range [][]string{nil, {"unknown-protocol"}, {"http/1.1"}, {"h2"}, {"h2", "http/1.1"}} {
				visits += 1
				conn := world.dialVisitor()
				client := tls.Client(conn, &tls.Config{ServerName: world.serverName(publish.host), RootCAs: world.device.roots, NextProtos: alpnIds, MinVersion: tls.VersionTLS13})
				if err := client.Handshake(); err == nil {
					// a visitor that got through the handshake writes what a service would answer to
					client.Write([]byte("GET / HTTP/1.1\r\nHost: " + world.serverName(publish.host) + "\r\n\r\nPRI * HTTP/2.0\r\n"))
					client.CloseWrite()
					io.ReadAll(client)
				}
				conn.Close()
			}
		}
		// the public raw host's service is reached and echoes
		conn := world.dialVisitor()
		client := tls.Client(conn, &tls.Config{ServerName: world.serverName(public.host), RootCAs: world.device.roots, NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS13})
		if err := client.Handshake(); err != nil {
			t.Fatalf("the public host's handshake: %v", err)
		}
		payload := []byte("public bytes")
		if _, err := client.Write(payload); err != nil {
			t.Fatal(err)
		}
		echo := make([]byte, len(payload))
		if _, err := io.ReadFull(client, echo); err != nil || string(echo) != string(payload) {
			t.Fatalf("the public host's echo: %q, %v", echo, err)
		}
		conn.Close()
		visits += 1
		world.awaitEnded(uint64(visits))

		for _, publish := range privates {
			endpoint := endpoints["tls/443/"+publish.host]
			if endpoint.accepted.Load() == 0 {
				t.Errorf("the %s host's front took no stream, so the row proves nothing", publish.host)
			}
			if dials := endpoint.dials.Load(); dials != 0 {
				t.Errorf("the %s host's service was reached %d times before any authentication", publish.host, dials)
			}
		}
		if dials := endpoints["tls/443/"+public.host].dials.Load(); dials != 1 {
			t.Fatalf("the public host's service was reached %d times", dials)
		}
	})
}
