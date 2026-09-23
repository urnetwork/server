package connect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	connectlib "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Exercise real JWT/state/membership admission, resident exchange, encrypted
// Transfer handshakes, reconnect generations and payload checks on both new and
// old providers. The fixture asserts actual custom carrier/fallback counters.
func TestConnectH1PlusEncrypted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testConnect(t, contractTestNone, &testConnectConfig{
			transportMode: connectlib.TransportModeH1, enableH1Plus: true, enableEncryption: true, enableTransportReform: true, enableNack: true,
		})
	})
}

func TestConnectH1PlusOldProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testConnect(t, contractTestNone, &testConnectConfig{
			transportMode: connectlib.TransportModeH1, enableH1Plus: true, oldH1Provider: true, enableEncryption: true, enableTransportReform: true, enableNack: true,
		})
	})
}

// The client rollout gate is independent of server capability. With it off,
// a server that supports H1+ must still receive an ordinary RFC WebSocket
// session and carry the full encrypted Connect exchange.
func TestConnectH1ClientH1PlusDisabledUsesWebSocket(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testConnect(t, contractTestNone, &testConnectConfig{
			transportMode:         connectlib.TransportModeH1,
			enableH1Plus:          false,
			expectH1WebSocket:     true,
			enableEncryption:      true,
			enableTransportReform: true,
			enableNack:            true,
		})
	})
}

func TestConnectH1PlusEncryptedWithExtender(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testConnect(t, contractTestNone, &testConnectConfig{
			transportMode: connectlib.TransportModeH1, enableH1Plus: true, enableExtender: true, enableEncryption: true, enableTransportReform: true, enableNack: true,
		})
	})
}

func TestConnectH1PlusAuthenticationBefore101(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, nil, func(settings *ConnectHandlerSettings) { settings.EnableH1Plus = true })
		defer env.Close()
		clientId, token := env.authClient(&model.AuthNetworkClientArgs{Description: "h1plus admission", DeviceSpec: "test"})
		address := fmt.Sprintf("ws://127.0.0.1:%d/", env.port)
		headers := func(token string) http.Header {
			return http.Header{
				"Authorization": []string{"Bearer " + token}, "X-Ur-Instanceid": []string{server.NewId().String()}, "X-Ur-Transportversion": []string{"2"},
			}
		}
		dialer := &websocket.Dialer{HandshakeTimeout: 3 * time.Second}
		for _, header := range []http.Header{nil, headers("malformed"), headers(env.userSession.ByJwt.User().Sign())} {
			conn, err := connectlib.DialFramedUpgrade(ctx, address, header, dialer, connectlib.H1FramerProtocol)
			if conn != nil {
				conn.Close()
				t.Fatal("unauthorized request received101")
			}
			var rejected *connectlib.HTTPUpgradeError
			if !errors.As(err, &rejected) || (rejected.StatusCode != 401 && rejected.StatusCode != 403) || !rejected.Terminal {
				t.Fatalf("auth rejection=%v", err)
			}
		}
		conn, err := connectlib.DialFramedUpgrade(ctx, address, headers(token), dialer, connectlib.H1FramerProtocol)
		if err != nil {
			t.Fatalf("authorized custom upgrade: %v", err)
		}
		conn.Close()
		result, err := model.RemoveNetworkClient(&model.RemoveNetworkClientArgs{ClientId: clientId}, env.userSession)
		if err != nil || result.Error != nil {
			t.Fatalf("remove client=%v %v", result, err)
		}
		conn, err = connectlib.DialFramedUpgrade(ctx, address, headers(token), dialer, connectlib.H1FramerProtocol)
		if conn != nil {
			conn.Close()
			t.Fatal("inactive client received101")
		}
		var rejected *connectlib.HTTPUpgradeError
		if !errors.As(err, &rejected) || (rejected.StatusCode != 401 && rejected.StatusCode != 403) {
			t.Fatalf("inactive client rejection=%v", err)
		}
	})
}
