package connect

import (
	"context"
	"github.com/urnetwork/connect/v2026"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// TestExchangeProxyPeerHidden drives a proxy-configured client and a regular
// client of the same network through a real exchange and asserts the proxy
// client is counted toward the network's connected total but never appears as a
// visible network peer.
//
// This exercises the resident.peerCategory == NetworkPeerCategoryProxy path end
// to end: a client with a proxy_device_config connects, its resident registers
// via AddNetworkProxyPeer (a separate connected_proxy zset, no peer
// subscription), so GetNetworkConnectedCount counts it while
// GetNetworkPeersForSession filters it out. The model unit test
// (TestNetworkProxyPeer) covers only the registry level; this covers the
// resident wiring that reads the category and chooses the proxy path.
//
// Requires the test DB env (WARP_ENV=local + postgres/redis/vault), like the
// rest of this package; skipped under -short.
func TestExchangeProxyPeerHidden(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := testing_newPeerDiscoveryEnv(ctx, t, 8095, 9015)
		defer env.Close()

		// a regular top-level client — the control: it must be a visible peer
		clientIdReg, byClientJwtReg := env.authClient(&model.AuthNetworkClientArgs{
			Description: "regular device",
			DeviceSpec:  "regular spec",
		})
		// a proxy client: same network, but marked with a proxy_device_config so
		// GetNetworkPeerProfile categorizes it as a proxy peer. The config must
		// exist before the client connects — the resident reads the category once
		// at creation.
		clientIdProxy, byClientJwtProxy := env.authClient(&model.AuthNetworkClientArgs{
			Description: "proxy device",
			DeviceSpec:  "proxy spec",
		})
		err := model.CreateProxyDeviceConfig(ctx, &model.ProxyDeviceConfig{
			ProxyDeviceConnection: model.ProxyDeviceConnection{ClientId: clientIdProxy},
			ProxyDeviceMode:       model.ProxyDeviceModeDevice,
		})
		connect.AssertEqual(t, err, nil)

		// connect both to the exchange
		clientReg := env.newClient(clientIdReg)
		defer clientReg.Close()
		clientProxy := env.newClient(clientIdProxy)
		defer clientProxy.Close()

		transportReg := env.newTransport(byClientJwtReg, server.NewId(), clientReg.RouteManager())
		defer transportReg.Close()
		transportProxy := env.newTransport(byClientJwtProxy, server.NewId(), clientProxy.RouteManager())
		defer transportProxy.Close()

		// wait until both residents have registered: the regular client in the
		// visible (client) zset and the proxy client in the proxy zset, so the
		// combined connected count reaches 2
		waitForConnectedCount := func(want int) bool {
			endTime := time.Now().Add(60 * time.Second)
			for {
				if model.GetNetworkConnectedCount(ctx, env.networkId) == want {
					return true
				}
				if endTime.Before(time.Now()) {
					return false
				}
				select {
				case <-ctx.Done():
					return false
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
		connect.AssertEqual(t, waitForConnectedCount(2), true)

		// the proxy client is counted (above) but never a visible peer: the
		// network session sees only the regular client
		peersResult, err := model.GetNetworkPeersForSession(env.userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, peersResult.Error, nil)
		connect.AssertEqual(t, len(peersResult.Peers), 1)
		connect.AssertEqual(t, peersResult.Peers[0].ClientId, clientIdReg)
		for _, peer := range peersResult.Peers {
			connect.AssertNotEqual(t, peer.ClientId, clientIdProxy)
		}
	})
}
