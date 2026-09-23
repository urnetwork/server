package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
)

// The real proxy endpoint validates the existing signed-id/device admission
// before101, accepts both carriers, and serves the actual forward/reverse RPC
// session over XL. The SDK's independent tests cover3MiB and budget boundaries.
func TestProxyDeviceRpcH1Plus(t *testing.T) {
	if testing.Short() {
		t.Skip("local proxy environment required")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		opts := defaultProxyTestOptions()
		opts.enableDeviceRpc = true
		opts.deviceRpcH1PlusStats = &connect.H1PlusStats{}
		opts.disableSecurityPolicies = true
		h := setupProxyTestWithOptions(t, opts)
		defer h.close(t)
		dialer := &websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, HandshakeTimeout: 5 * time.Second}
		address := fmt.Sprintf("wss://127.0.0.1:%d/device-rpc", h.apiPort)
		conn, err := connect.DialFramedUpgrade(h.ctx, address, http.Header{"Authorization": []string{"Bearer invalid"}}, dialer, connect.H1FramerXlProtocol)
		if conn != nil {
			conn.Close()
			t.Fatal("invalid signed proxy id upgraded")
		}
		var rejection *connect.HTTPUpgradeError
		if !errors.As(err, &rejection) || rejection.StatusCode != 401 || !rejection.Terminal {
			t.Fatalf("signed-id rejection=%v", err)
		}
		conn, err = connect.DialFramedUpgrade(h.ctx, address, http.Header{"Authorization": []string{"Bearer " + h.signedProxyId}}, dialer, connect.H1FramerXlProtocol)
		if err != nil {
			t.Fatalf("authenticated TLS XL upgrade=%v", err)
		}
		conn.Close()

		// A native session offers XL by default (H1+ is opt-out).
		remote, err := sdk.NewPlatformDeviceRemote(h.networkSpace, h.pdByClientJwt, h.deviceRpcUrl, h.signedProxyId, sdk.RequireIdFromBytes(h.pdInstanceId.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		defer remote.Close()
		waitFor(t, 30*time.Second, "XL remote connected", remote.GetRemoteConnected)
		listener := &e2eOfflineListener{offline: make(chan bool, 4)}
		sub := remote.AddOfflineChangeListener(listener)
		defer sub.Close()
		target := !remote.GetOffline()
		remote.SetOffline(target)
		waitFor(t, 15*time.Second, "XL reverse event", func() bool {
			select {
			case state := <-listener.offline:
				return state == target
			default:
				return false
			}
		})
		stats := opts.deviceRpcH1PlusStats.Snapshot()
		if stats.Accepted < 2 || stats.Messages == 0 {
			t.Fatalf("actual proxy did not carry XL RPC: %+v", stats)
		}
		// Existing browser/old-client carrier remains available on the same URL.
		ws, _, err := dialer.DialContext(h.ctx, address, http.Header{"Authorization": []string{"Bearer " + h.signedProxyId}})
		if err != nil {
			t.Fatalf("WebSocket compatibility=%v", err)
		}
		ws.Close()
	})
}

// H1+ is opt-out: the default proxy accepts XL device RPC.
func TestDefaultProxySettingsAcceptDeviceRpcH1Plus(t *testing.T) {
	if !DefaultProxySettings().EnableDeviceRpcH1Plus {
		t.Fatal("default proxy opted out of device RPC H1+")
	}
}
