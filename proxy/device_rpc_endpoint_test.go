package proxy

// Endpoint-level tests for the device rpc path that the e2e (device_rpc_e2e_test.go)
// does not cover:
//
//   - the real proxy api TLS listener route + auth (the e2e drives the handler
//     over a plain http test listener, so the production wiring — the GET
//     /device-rpc route on the api TLS listener and its per-proxy SNI TLS — is
//     otherwise unexercised)
//   - concurrent DeviceRemotes controlling one hosted device
//   - an attached rpc session keeping the device non-idle, and releasing it on
//     detach
//
// These reuse the proxy integration harness and need the standard local test
// environment plus outbound internet, like the rest of this package. Skipped
// under -short.

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
	"github.com/gorilla/websocket"

	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
)

func TestProxyDeviceRpcEndpoint(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		opts := defaultProxyTestOptions()
		opts.enableDeviceRpc = true
		opts.disableSecurityPolicies = true
		h := setupProxyTestWithOptions(t, opts)
		defer h.cancel()

		// ---- the real proxy api TLS listener route + auth --------------------
		// dial the actual api listener (InternalApiPort, TLS), not the plain test
		// endpoint, so the production route and per-proxy SNI TLS are exercised.
		tlsDialer := &websocket.Dialer{
			// the api listener presents a self-signed cert (as the https leg test
			// also relaxes); the signed proxy id is the security boundary
			TLSClientConfig:  &tls.Config{InsecureSkipVerify: true},
			HandshakeTimeout: 15 * time.Second,
		}
		apiDial := func(query string, header http.Header) (*websocket.Conn, *http.Response, error) {
			u := url.URL{
				Scheme:   "wss",
				Host:     fmt.Sprintf("127.0.0.1:%d", InternalApiPort),
				Path:     "/device-rpc",
				RawQuery: query,
			}
			return tlsDialer.Dial(u.String(), header)
		}

		// valid signed proxy id via the query parameter: the upgrade succeeds and
		// the hosted mux is live — a zero-length binary keepalive arrives, proving
		// the request reached PushDeviceRpc over the real listener.
		ws, resp, err := apiDial("proxy="+url.QueryEscape(h.signedProxyId), nil)
		if err != nil {
			t.Fatalf("api listener valid dial: %v (resp=%v)", err, resp)
		}
		assert.Equal(t, resp.StatusCode, http.StatusSwitchingProtocols)
		ws.SetReadDeadline(time.Now().Add(15 * time.Second))
		messageType, data, err := ws.ReadMessage()
		assert.Equal(t, err, nil)
		assert.Equal(t, messageType, websocket.BinaryMessage)
		assert.Equal(t, len(data), 0)
		ws.Close()

		// valid signed proxy id via the Authorization Bearer header (non-browser
		// form) also upgrades
		bearer := http.Header{}
		bearer.Set("Authorization", "Bearer "+h.signedProxyId)
		wsBearer, respBearer, err := apiDial("", bearer)
		if err != nil {
			t.Fatalf("api listener bearer dial: %v (resp=%v)", err, respBearer)
		}
		assert.Equal(t, respBearer.StatusCode, http.StatusSwitchingProtocols)
		wsBearer.Close()

		// missing signed proxy id -> 401
		_, respMissing, err := apiDial("", nil)
		assert.NotEqual(t, err, nil)
		if respMissing == nil {
			t.Fatal("api listener missing-token dial: no response")
		}
		assert.Equal(t, respMissing.StatusCode, http.StatusUnauthorized)

		// malformed signed proxy id -> 401
		_, respBad, err := apiDial("proxy=garbage", nil)
		assert.NotEqual(t, err, nil)
		if respBad == nil {
			t.Fatal("api listener malformed-token dial: no response")
		}
		assert.Equal(t, respBad.StatusCode, http.StatusUnauthorized)

		// ---- concurrent DeviceRemotes controlling one hosted device ----------
		instanceId := sdk.RequireIdFromBytes(h.pdInstanceId.Bytes())
		newRemote := func() *sdk.DeviceRemote {
			d, err := sdk.NewPlatformDeviceRemote(
				h.networkSpace,
				h.pdByClientJwt,
				h.deviceRpcUrl,
				h.signedProxyId,
				instanceId,
			)
			assert.Equal(t, err, nil)
			return d
		}
		remoteA := newRemote()
		defer remoteA.Close()
		remoteB := newRemote()
		defer remoteB.Close()

		// both remotes connect and sync against the one hosted device
		for i, d := range []*sdk.DeviceRemote{remoteA, remoteB} {
			label := fmt.Sprintf("remote %d", i)
			waitFor(t, 60*time.Second, label+" connected", d.GetRemoteConnected)
			waitFor(t, 30*time.Second, label+" location synced", func() bool {
				return d.GetConnectLocation() != nil
			})
		}

		// a change driven by one remote applies to the hosted device and is
		// observed by the other remote via its own reverse channel
		target := !remoteA.GetOffline()
		remoteA.SetOffline(target)
		waitFor(t, 30*time.Second, "remote B observes remote A's change", func() bool {
			return remoteB.GetOffline() == target
		})
		waitFor(t, 10*time.Second, "remote A applied", func() bool {
			return remoteA.GetOffline() == target
		})
	})
}

// TestProxyDeviceRpcSessionIdle asserts that an attached device rpc session keeps
// the hosted device non-idle for its duration (PushDeviceRpc's keepalive ticker),
// and that detaching stops holding it open so it can be idle-reaped.
func TestProxyDeviceRpcSessionIdle(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		opts := defaultProxyTestOptions()
		opts.enableDeviceRpc = true
		opts.disableSecurityPolicies = true
		h := setupProxyTestWithOptions(t, opts)
		defer h.cancel()

		pd, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		assert.Equal(t, err, nil)

		// shrink the idle timeout so the keepalive ticker (idleTimeout/2) and the
		// idle threshold are observable in-test. Set before attaching:
		// PushDeviceRpc reads it when it starts the ticker. The per-device idle
		// checker runs on the manager's 1-minute cadence, so it does not reap the
		// device during this short window; the activity timestamp is read directly.
		const idleTimeout = 3 * time.Second
		pd.settings.ProxyDeviceIdleTimeout = idleTimeout

		idleFor := func() time.Duration {
			return time.Since(time.Unix(0, pd.lastActivityNanos.Load()))
		}

		// attach a device rpc session over the (plain) test endpoint; the handler
		// calls pd.PushDeviceRpc, which bumps activity and keeps bumping it
		u := h.deviceRpcUrl + "/device-rpc?proxy=" + url.QueryEscape(h.signedProxyId)
		ws, _, err := websocket.DefaultDialer.Dial(u, nil)
		assert.Equal(t, err, nil)
		// drain server frames (mux keepalives) so a server write never stalls
		go func() {
			for {
				if _, _, err := ws.ReadMessage(); err != nil {
					return
				}
			}
		}()

		// while attached, the ticker keeps the device fresh even though the idle
		// timeout has elapsed several times over
		select {
		case <-time.After(2 * idleTimeout):
		}
		if d := idleFor(); d >= idleTimeout {
			t.Fatalf("attached session did not keep device non-idle (idle for %s)", d)
		}

		// detach; the ticker stops, so the device goes idle and stays idle
		ws.Close()
		select {
		case <-time.After(2 * idleTimeout):
		}
		if d := idleFor(); d < idleTimeout {
			t.Fatalf("device did not go idle after detach (idle for %s)", d)
		}
	})
}
