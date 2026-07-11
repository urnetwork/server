package proxy

// End-to-end test for the device rpc path: a DeviceRemote (as a browser would)
// controls a hosted proxy DeviceLocal by connecting directly to the proxy host:
//
//   DeviceRemote --ws /device-rpc (signed proxy id)--> proxy host device rpc
//     handler --> hosted DeviceLocal rpc
//
// The DeviceLocal lives in the proxy process, so there is no connect-service or
// resident hop. It reuses the proxy integration harness (provider + proxy
// device manager) and stands up the device rpc endpoint over a plain http
// listener (the way server/connect exposes its handler for tests); the
// DeviceRemote uses the real platform dialer against it. No JS is involved —
// this exercises the Go path a JS wasm client drives.
//
// Requires the standard local test environment (WARP_ENV=local + local
// postgres/redis/vault) and real outbound internet, like the rest of this
// package. Skipped under -short.

import (
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type e2eOfflineListener struct {
	offline chan bool
}

func (self *e2eOfflineListener) OfflineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	select {
	case self.offline <- offline:
	default:
	}
}

type e2eRecreatedListener struct {
	recreated chan struct{}
}

func (self *e2eRecreatedListener) DeviceRecreated() {
	select {
	case self.recreated <- struct{}{}:
	default:
	}
}

func TestProxyDeviceRpcE2E(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		opts := defaultProxyTestOptions()
		opts.enableDeviceRpc = true
		opts.disableSecurityPolicies = true
		h := setupProxyTestWithOptions(t, opts)
		defer h.cancel()

		// build a DeviceRemote as a browser would: connect directly to the
		// proxy host's device rpc endpoint, authenticating with the device's
		// signed proxy id (not a jwt). byJwt is the network member jwt for the
		// network space api.
		instanceId := sdk.RequireIdFromBytes(h.pdInstanceId.Bytes())
		device, err := sdk.NewPlatformDeviceRemote(
			h.networkSpace,
			h.pdByClientJwt,
			h.deviceRpcUrl,
			h.signedProxyId,
			instanceId,
		)
		assert.Equal(t, err, nil)
		defer device.Close()

		// the rpc syncs and connects
		waitFor(t, 60*time.Second, "device remote connected", func() bool {
			return device.GetRemoteConnected()
		})

		// the connect location synced from the hosted device (set to the
		// provider at creation)
		waitFor(t, 30*time.Second, "connect location synced", func() bool {
			return device.GetConnectLocation() != nil
		})

		// hosted-incompatible: a remote SetRouteLocal is ignored by the hosted
		// device. The hosted proxy device runs with route local false.
		routeLocalBefore := device.GetRouteLocal()
		device.SetRouteLocal(!routeLocalBefore)
		// give the (no-op) rpc call time to round-trip
		select {
		case <-time.After(2 * time.Second):
		}
		assert.Equal(t, device.GetRouteLocal(), routeLocalBefore)

		// an allowed setter (SetOffline) applies and fires the reverse-channel
		// listener. Toggle relative to the current state so it is a real change
		// regardless of the hosted device's default.
		offlineListener := &e2eOfflineListener{offline: make(chan bool, 16)}
		offlineSub := device.AddOfflineChangeListener(offlineListener)
		defer offlineSub.Close()

		target := !device.GetOffline()
		device.SetOffline(target)
		waitFor(t, 30*time.Second, "offline change reverse event", func() bool {
			select {
			case offline := <-offlineListener.offline:
				return offline == target
			default:
				return false
			}
		})
		waitFor(t, 10*time.Second, "offline applied", func() bool {
			return device.GetOffline() == target
		})

		// the connection survives past the mux keepalive window (30s): the
		// relay must carry keepalive across all legs. After the window, a fresh
		// round-trip (setter -> hosted device -> reverse event) must still work,
		// proving the connection did not silently drop.
		select {
		case <-time.After(35 * time.Second):
		}
		survivorListener := &e2eOfflineListener{offline: make(chan bool, 16)}
		survivorSub := device.AddOfflineChangeListener(survivorListener)
		defer survivorSub.Close()
		device.SetOffline(!target)
		waitFor(t, 30*time.Second, "round-trip after keepalive window", func() bool {
			select {
			case offline := <-survivorListener.offline:
				return offline == !target
			default:
				return false
			}
		})

		// peers: the proxy client is counted but never appears in the peer list
		userSession := session.Testing_CreateClientSession(h.ctx, &jwt.ByJwt{
			NetworkId: h.pdNetworkId,
			UserId:    h.pdUserId,
		})
		peersResult, err := model.GetNetworkPeersForSession(userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, peersResult.Error, nil)
		// the proxy client never appears as a visible peer. (The proxy
		// device has no resident here, so the connected-count registration —
		// unit-tested in model.TestNetworkProxyPeer — is not exercised.)
		for _, peer := range peersResult.Peers {
			assert.NotEqual(t, peer.ClientId, h.pdClientId)
		}

		// device recreate: kill the hosted device; the DeviceRemote reconnects,
		// the handler reopens a fresh device (new generation), and the
		// DeviceRecreated listener fires
		recreatedListener := &e2eRecreatedListener{recreated: make(chan struct{}, 1)}
		recreatedSub := device.AddDeviceRecreatedListener(recreatedListener)
		defer recreatedSub.Close()

		pd, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		assert.Equal(t, err, nil)
		pd.Cancel()

		select {
		case <-recreatedListener.recreated:
		case <-time.After(60 * time.Second):
			t.Fatal("timeout waiting for device recreated")
		}

		// after recreate the remote is connected again
		waitFor(t, 30*time.Second, "device remote reconnected", func() bool {
			return device.GetRemoteConnected()
		})

		// close conventions: closing the remote tears down the rpc
		device.Close()
		waitFor(t, 30*time.Second, "device remote disconnected", func() bool {
			return !device.GetRemoteConnected()
		})
	})
}
