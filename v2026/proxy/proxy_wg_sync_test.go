package proxy

// Focused tests for the wg server's client validation and reconcile (sync)
// behavior. These do not drive traffic; they assert how the peer table is
// maintained: adds are validated and counted, the sync removes peers whose
// proxy client no longer exists, and peers applied after the sync start time
// are kept (grace for clients warmed up concurrently with the db snapshot).

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/proxy/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func newTestProxyClient(t testing.TB, clientIpv4 netip.Addr) *model.ProxyClient {
	serverConfig := model.LoadServerProxyConfig()

	_, clientPublicKey, err := proxy.WgGenKeyPairStrings()
	if err != nil {
		t.Fatalf("generate client keypair: %v", err)
	}

	proxyId := server.NewId()
	return &model.ProxyClient{
		CreateTime: server.NowUtc(),
		ProxyId:    proxyId,
		ClientId:   server.NewId(),
		InstanceId: server.NewId(),
		AuthToken:  model.SignProxyId(proxyId),
		WgConfig: &model.WgConfig{
			ClientPublicKey: clientPublicKey,
			ProxyPublicKey:  serverConfig.Wg.PublicKey,
			ClientIpv4:      clientIpv4,
		},
	}
}

func TestWgServerSyncProxyClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		setProxyTestEnv()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		proxyDeviceManager := NewProxyDeviceManagerWithDefaults(ctx)
		defer proxyDeviceManager.Close()

		wgCtx, wgCancel := context.WithCancel(ctx)
		defer wgCancel()
		wg := NewWgServer(wgCtx, wgCancel, proxyDeviceManager, DefaultProxySettings())

		proxyClientA := newTestProxyClient(t, netip.MustParseAddr("10.10.0.1"))
		proxyClientB := newTestProxyClient(t, netip.MustParseAddr("10.10.0.2"))

		err := wg.AddProxyClients(proxyClientA, proxyClientB)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, wg.wgProxy.ClientCount(), 2)

		// a client that fails validation (auth token signed for a different
		// proxy id) is skipped, not an error
		badProxyClient := newTestProxyClient(t, netip.MustParseAddr("10.10.0.3"))
		badProxyClient.AuthToken = model.SignProxyId(server.NewId())
		err = wg.AddProxyClients(badProxyClient)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, wg.wgProxy.ClientCount(), 2)

		// sync with only A in the full set: B was applied before the sync
		// start time and must be removed
		err = wg.SyncProxyClients([]*model.ProxyClient{proxyClientA}, server.NowUtc())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, wg.wgProxy.ClientCount(), 1)
		clients := wg.wgProxy.Clients()
		_, ok := clients[proxyClientA.WgConfig.ClientIpv4]
		connect.AssertEqual(t, ok, true)

		// grace: a peer applied after the sync start time is kept even when it
		// is not in the (older) full set
		proxyClientC := newTestProxyClient(t, netip.MustParseAddr("10.10.0.4"))
		syncStartTime := server.NowUtc()
		time.Sleep(10 * time.Millisecond)
		err = wg.AddProxyClients(proxyClientC)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, wg.wgProxy.ClientCount(), 2)

		err = wg.SyncProxyClients([]*model.ProxyClient{proxyClientA}, syncStartTime)
		connect.AssertEqual(t, err, nil)
		clients = wg.wgProxy.Clients()
		_, ok = clients[proxyClientA.WgConfig.ClientIpv4]
		connect.AssertEqual(t, ok, true)
		_, ok = clients[proxyClientC.WgConfig.ClientIpv4]
		connect.AssertEqual(t, ok, true)

		// the graced peer is removed by a later sync that still does not
		// include it
		err = wg.SyncProxyClients([]*model.ProxyClient{proxyClientA}, server.NowUtc())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, wg.wgProxy.ClientCount(), 1)
	})
}
