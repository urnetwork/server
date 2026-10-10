package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go"
	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type authenticationDeadlineClient struct {
	close func()
	ping  func() error
}

func authenticationDeadlineHeaders(token string, instance server.Id) http.Header {
	return http.Header{
		"Authorization":         {"Bearer " + token},
		"X-Ur-Instanceid":       {instance.String()},
		"X-Ur-Transportversion": {"2"},
	}
}

// Uses each production Connect entry point, including JWT state and membership
// queries. The QUIC client uses only the fixture's fresh loopback certificate.
func dialAuthenticationDeadlineClient(ctx context.Context, env *peerDiscoveryEnv, carrier, token string) (*authenticationDeadlineClient, error) {
	instance := server.NewId()
	address := fmt.Sprintf("ws://127.0.0.1:%d/", env.port)
	headers := authenticationDeadlineHeaders(token, instance)
	authBytes, err := clientconnect.EncodeFrame(&protocol.Auth{ByJwt: token, InstanceId: instance.Bytes()}, clientconnect.DefaultProtocolVersion)
	if err != nil {
		return nil, err
	}
	defer clientconnect.MessagePoolReturn(authBytes)
	if carrier == "h3" || carrier == "h3_no_stream" {
		conn, err := quic.DialAddr(ctx, fmt.Sprintf("127.0.0.1:%d", env.h3Port), &tls.Config{
			InsecureSkipVerify: true, ServerName: "127.0.0.1", // Private fixture certificate only.
		}, &quic.Config{MaxIdleTimeout: 10 * time.Second})
		if err != nil {
			return nil, err
		}
		client := &authenticationDeadlineClient{close: func() { conn.CloseWithError(0, "") }}
		if carrier == "h3_no_stream" {
			return client, nil
		}
		stream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			client.close()
			return nil, err
		}
		framer := clientconnect.NewFramer(env.handler.settings.FramerSettings)
		if err := framer.Write(stream, authBytes); err != nil {
			client.close()
			return nil, err
		}
		client.ping = func() error { return framer.Write(stream, nil) }
		return client, nil
	}
	dialer := &websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	if carrier == "h1plus" {
		conn, err := clientconnect.DialFramedUpgrade(ctx, address, headers, dialer, clientconnect.H1FramerProtocol)
		if err != nil {
			return nil, err
		}
		ws, err := clientconnect.NewFramedMessageConn(conn, clientconnect.H1FramerProtocol, env.handler.settings.FramerSettings.MaxMessageLen, nil)
		if err != nil {
			conn.Close()
			return nil, err
		}
		return &authenticationDeadlineClient{close: func() { ws.Close() }, ping: func() error { return ws.WriteMessage(websocket.BinaryMessage, nil) }}, nil
	}
	if carrier == "h1_frame" {
		headers = nil
	}
	ws, _, err := dialer.DialContext(ctx, address, headers)
	if err != nil {
		return nil, err
	}
	client := &authenticationDeadlineClient{close: func() { ws.Close() }, ping: func() error { return ws.WriteMessage(websocket.BinaryMessage, nil) }}
	if carrier == "h1_frame" {
		if err := ws.WriteMessage(websocket.BinaryMessage, authBytes); err != nil {
			client.close()
			return nil, err
		}
	}
	return client, nil
}

func authenticationDeadlineEnvironment(ctx context.Context, t testing.TB, timeout time.Duration, ledger *clientconnect.TransferMemoryOwnerLedger) (*peerDiscoveryEnv, server.Id, string) {
	env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, func(settings *ExchangeSettings) {
		settings.EnableNetworkPeers = false
		settings.MemoryOwnerLedger = ledger
	}, func(settings *ConnectHandlerSettings) {
		settings.ReadTimeout = timeout
		settings.ConnectionAnnounceSettings.EnableNetworkPeers = false
	})
	clientId, token := env.authClient(&model.AuthNetworkClientArgs{Description: "authentication deadline fixture", DeviceSpec: "synthetic"})
	return env, clientId, token
}

// ConnectHandler includes persistent QUIC listener owners in activeCount.
// Only accepted connection owners should retire while those listeners stay live.
func authenticationDeadlineActiveConnections(handler *ConnectHandler) int {
	handler.activeLock.Lock()
	defer handler.activeLock.Unlock()
	return handler.activeCount - len(handler.packetEndpoints)
}

func waitForAuthenticationDeadlineConnections(ctx context.Context, handler *ConnectHandler, expected int, timeout time.Duration) bool {
	return TestingWaitForConnectCondition(ctx, timeout, time.Millisecond, func(context.Context) (bool, string) {
		active := authenticationDeadlineActiveConnections(handler)
		return active == expected, fmt.Sprintf("active connection owners=%d, want=%d", active, expected)
	}) == nil
}

// A real PostgreSQL table lock reproduces a valid JWT waiting for current
// authorization state. Close the WebSocket peer too: net/http no longer tracks
// its socket after hijack, so request cancellation cannot retire this owner.
func TestConnectAuthenticationDeadlineReleasesBlockedDatabase(t *testing.T) {
	for _, carrier := range []string{"h1_header", "h1_frame", "h1plus", "h3"} {
		t.Run(carrier, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				const authTimeout = 750 * time.Millisecond
				var ledger clientconnect.TransferMemoryOwnerLedger
				env, _, token := authenticationDeadlineEnvironment(ctx, t, authTimeout, &ledger)
				defer env.Close()
				release := holdResidentProfileTable(t, ctx)
				defer release()
				type result struct {
					client *authenticationDeadlineClient
					err    error
				}
				dialed := make(chan result, 1)
				go func() {
					client, err := dialAuthenticationDeadlineClient(ctx, env, carrier, token)
					dialed <- result{client, err}
				}()
				waitForBlockedResidentProfiles(t, ctx, 1)
				var client *authenticationDeadlineClient
				if carrier != "h1plus" {
					select {
					case result := <-dialed:
						if result.err != nil {
							t.Fatal(result.err)
						}
						client = result.client
					case <-ctx.Done():
						t.Fatal("fixture carrier did not enter its authentication query")
					}
					defer client.close()
					if carrier != "h3" {
						client.close()
					}
				}
				joined := waitForAuthenticationDeadlineConnections(ctx, env.handler, 0, 2*authTimeout)
				parentAlive := env.handler.ctx.Err() == nil && env.exchange.ctx.Err() == nil
				env.exchange.stateLock.Lock()
				residents, connections := len(env.exchange.residents), len(env.exchange.connections)
				env.exchange.stateLock.Unlock()
				owners := ledger.Snapshot()
				// Release only after recording whether the deadline independently
				// joined authentication. This also cleans up the pre-fix RED run.
				release()
				if carrier == "h1plus" {
					select {
					case result := <-dialed:
						if result.client != nil {
							result.client.close()
						}
						if joined {
							var unavailable *clientconnect.HTTPUpgradeError
							if !errors.As(result.err, &unavailable) || unavailable.StatusCode != http.StatusServiceUnavailable || unavailable.Terminal {
								t.Errorf("dependency timeout did not preserve retryable H1+ rejection: %v", result.err)
							}
						}
					case <-ctx.Done():
						t.Fatal("H1+ admission did not finish")
					}
				}
				t.Logf("carrier=%s blocked_auth_joined_before_lock_release=%t resident_count=%d exchange_registrations=%d sdk_admitted=%d parent_alive=%t", carrier, joined, residents, connections, owners.Send.AdmittedTotal+owners.Receive.AdmittedTotal+owners.Forward.AdmittedTotal, parentAlive)
				if !joined || !parentAlive || residents != 0 || connections != 0 || !owners.Complete || owners.Send.AdmittedTotal+owners.Receive.AdmittedTotal+owners.Forward.AdmittedTotal != 0 {
					t.Fatal("unadmitted authentication outlived its total deadline or constructed transfer work")
				}
			})
		})
	}
}

func TestConnectAuthenticationDeadlineBoundsQuicStreamAdmission(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		const authTimeout = 250 * time.Millisecond
		env, _, token := authenticationDeadlineEnvironment(ctx, t, authTimeout, nil)
		defer env.Close()
		client, err := dialAuthenticationDeadlineClient(ctx, env, "h3_no_stream", token)
		if err != nil {
			t.Fatal(err)
		}
		defer client.close()
		if !waitForAuthenticationDeadlineConnections(ctx, env.handler, 1, time.Second) {
			t.Fatal("QUIC connection did not enter its accepted handler")
		}
		if !waitForAuthenticationDeadlineConnections(ctx, env.handler, 0, 3*authTimeout) {
			t.Fatal("QUIC authentication waited indefinitely for its first stream")
		}
	})
}

// Successful authentication transfers lifetime to the serving handler. Keep
// real framed traffic moving across two former authentication deadlines.
func TestConnectAuthenticationDeadlinePreservesServingHandoff(t *testing.T) {
	for _, carrier := range []string{"h1_header", "h1_frame", "h1plus", "h3"} {
		t.Run(carrier, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				const authTimeout = 500 * time.Millisecond
				env, clientId, token := authenticationDeadlineEnvironment(ctx, t, authTimeout, nil)
				defer env.Close()
				client, err := dialAuthenticationDeadlineClient(ctx, env, carrier, token)
				if err != nil {
					t.Fatal(err)
				}
				defer client.close()
				if err := TestingWaitForConnectCondition(ctx, 3*time.Second, time.Millisecond, func(context.Context) (bool, string) {
					env.exchange.stateLock.Lock()
					defer env.exchange.stateLock.Unlock()
					return env.exchange.residents[clientId] != nil, "waiting for authenticated resident"
				}); err != nil {
					t.Fatal(err)
				}
				until := time.Now().Add(2 * authTimeout)
				for time.Now().Before(until) {
					if err := client.ping(); err != nil {
						t.Fatal("serving carrier was canceled by authentication deadline", err)
					}
					time.Sleep(authTimeout / 10)
				}
				active := authenticationDeadlineActiveConnections(env.handler)
				env.exchange.stateLock.Lock()
				resident := env.exchange.residents[clientId]
				env.exchange.stateLock.Unlock()
				if active != 1 || resident == nil || resident.ctx.Err() != nil || resident.client.IsDone() {
					t.Fatalf("successful handoff kept the authentication deadline as its serving lifetime: active=%d resident=%v", active, resident != nil)
				}
			})
		})
	}
}
