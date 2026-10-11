package connect

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go"
	connectlib "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func admissionSessionToken(t testing.TB) (*session.ByJwt, string) {
	t.Helper()
	claims := newConnectAuthenticationWorkClaims(t.Context())
	claims.RootClientId = claims.ClientId
	sid := server.NewId()
	claims.SessionId = &sid
	token, err := session.MintNetworkSession(t.Context(), claims, "password")
	if err != nil {
		t.Fatal(err)
	}
	return claims, token
}

// These clients use the real transport boundary but do not start client-side
// reconnect loops. A rejected admission cannot be hidden by a later retry.
func dialSessionAdmission(ctx context.Context, env *peerDiscoveryEnv, carrier, token, info string) (connectlib.H1MessageConn, func(), error) {
	instance := server.NewId()
	authBytes, err := connectlib.EncodeFrame(&protocol.Auth{ByJwt: token, InstanceId: instance.Bytes(), AppVersion: "legacy-version", ClientInfo: info, StreamLeaseVersion: 1}, connectlib.DefaultProtocolVersion)
	if err != nil {
		return nil, nil, err
	}
	defer connectlib.MessagePoolReturn(authBytes)
	if carrier == "h3" {
		conn, err := quic.DialAddr(ctx, fmt.Sprintf("127.0.0.1:%d", env.h3Port), &tls.Config{InsecureSkipVerify: true, ServerName: "127.0.0.1"}, &quic.Config{MaxIdleTimeout: 10 * time.Second})
		if err != nil {
			return nil, nil, err
		}
		closeClient := func() { _ = conn.CloseWithError(0, "") }
		stream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			return nil, closeClient, err
		}
		_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
		framer := connectlib.NewFramer(env.handler.settings.FramerSettings)
		if err := framer.Write(stream, authBytes); err != nil {
			return nil, closeClient, err
		}
		response, err := framer.Read(stream)
		if err == nil {
			connectlib.MessagePoolReturn(response)
		}
		return nil, closeClient, err
	}
	address := fmt.Sprintf("ws://127.0.0.1:%d/", env.port)
	headers := authenticationDeadlineHeaders(token, instance)
	headers.Set("X-UR-AppVersion", "legacy-version")
	headers.Set(session.ClientInfoHeader, info)
	headers.Set("X-UR-StreamLeaseVersion", "1")
	dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	if carrier == "h1plus" {
		conn, err := connectlib.DialFramedUpgrade(ctx, address, headers, dialer, connectlib.H1FramerProtocol)
		if err != nil {
			return nil, nil, err
		}
		ws, err := connectlib.NewFramedMessageConn(conn, connectlib.H1FramerProtocol, env.handler.settings.FramerSettings.MaxMessageLen, nil)
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		return ws, func() { ws.Close() }, nil
	}
	if carrier == "h1_frame" {
		// Header metadata from another generation must not override auth-frame
		// metadata when the credential itself arrived in the first frame.
		headers.Del("Authorization")
		headers.Set(session.ClientInfoHeader, `{"v":1,"device_type":"windows","app_version":"wrong-generation"}`)
	}
	ws, _, err := dialer.DialContext(ctx, address, headers)
	if err != nil {
		return nil, nil, err
	}
	closeClient := func() { ws.Close() }
	if carrier == "h1_frame" {
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err := ws.WriteMessage(websocket.BinaryMessage, authBytes); err != nil {
			return ws, closeClient, err
		}
		_, _, err = ws.ReadMessage()
	}
	return ws, closeClient, err
}

func TestSessionAdmissionRevokedAfterRegistrationCannotPassFinalCheck(t *testing.T) {
	for _, carrier := range []string{"h1plus", "h3"} {
		t.Run(carrier, func(t *testing.T) {
			environment := server.DefaultTestEnv()
			environment.RerunCount = 0
			environment.Run(t, func(t testing.TB) {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				claims, token := admissionSessionToken(t)
				actorClaims := session.NewByJwt(claims.NetworkId, claims.UserId, claims.NetworkName, false, false)
				actor := session.NewLocalClientSession(ctx, "192.0.2.1:1", actorClaims)
				defer actor.Cancel()
				hookResult := make(chan error, 1)
				var admitted atomic.Int32
				var env *peerDiscoveryEnv
				env = testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, nil, func(settings *ConnectHandlerSettings) {
					settings.testingBeforeAuthorizationLease = func(value *session.ByJwt) {
						env.exchange.stateLock.Lock()
						registered := len(env.exchange.connections[*value.ClientId]) > 0
						env.exchange.stateLock.Unlock()
						if !registered {
							hookResult <- errors.New("final check preceded connection registration")
							return
						}
						_, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *value.SessionId, OperationId: server.NewId()}, actor)
						hookResult <- err
					}
					settings.testingAfterAuthorizationLease = func(*session.ByJwt, *session.AuthorizationLease) { admitted.Add(1) }
				})
				defer env.Close()
				_, closeClient, err := dialSessionAdmission(ctx, env, carrier, token, "")
				if closeClient != nil {
					defer closeClient()
				}
				if err == nil {
					t.Fatal("revoked session completed transport admission")
				}
				if carrier == "h1plus" {
					var rejection *connectlib.HTTPUpgradeError
					if !errors.As(err, &rejection) || rejection.StatusCode != http.StatusUnauthorized || !rejection.Terminal {
						t.Fatal("known revoke did not return terminal401", err)
					}
				}
				select {
				case err := <-hookResult:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("admission never reached final-check race")
				}
				if admitted.Load() != 0 {
					t.Fatal("revoked credential got an authorization lease")
				}
			})
		})
	}
}

func TestSessionAdmissionClientInfoRoundTripsAcrossCarriers(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		env := testing_newPeerDiscoveryEnv(ctx, t)
		defer env.Close()
		for _, carrier := range []string{"h1_header", "h1_frame", "h1plus", "h3"} {
			for _, metadata := range []struct{ raw, device, version string }{
				{`{"v":1,"device_type":"android","app_version":"1.2.3"}`, "android", "1.2.3"},
				{`{"v":1,"v":1,"device_type":"android","app_version":"invalid"}`, "unknown", "legacy-version"},
			} {
				claims, token := admissionSessionToken(t)
				_, closeClient, err := dialSessionAdmission(ctx, env, carrier, token, metadata.raw)
				if closeClient != nil {
					defer closeClient()
				}
				if err != nil {
					t.Fatal(carrier, err)
				}
				deadline := time.NewTimer(3 * time.Second)
				tick := time.NewTicker(10 * time.Millisecond)
				var observed session.SessionLastUsed
				found := false
				for !found {
					server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
						raw, err := r.Get(ctx, session.SessionKey(claims.NetworkId, "u:"+claims.SessionId.String())).Bytes()
						if errors.Is(err, server.RedisNil) {
							return nil
						}
						if err != nil {
							return err
						}
						found = true
						return json.Unmarshal(raw, &observed)
					}))
					if !found {
						select {
						case <-tick.C:
						case <-deadline.C:
							t.Fatal("transport did not record typed last use", carrier)
						}
					}
				}
				tick.Stop()
				deadline.Stop()
				if observed.UnixTime == 0 || observed.DeviceType != metadata.device || observed.AppVersion != metadata.version {
					t.Fatal("metadata generation/parsing differed by transport", carrier, observed)
				}
			}
		}
	})
}

func TestSessionAdmissionLeaseCloseDistinguishesRevokedAndUnavailable(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		leases := make(chan *session.AuthorizationLease, 2)
		env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, nil, func(settings *ConnectHandlerSettings) {
			settings.testingAfterAuthorizationLease = func(_ *session.ByJwt, lease *session.AuthorizationLease) { leases <- lease }
		})
		defer env.Close()
		for _, revoked := range []bool{false, true} {
			claims, token := admissionSessionToken(t)
			ws, closeClient, err := dialSessionAdmission(ctx, env, "h1plus", token, "")
			if err != nil {
				t.Fatal(err)
			}
			defer closeClient()
			lease := <-leases
			want := uint32(session.AuthorizationUnavailableCloseCode - 4000)
			if revoked {
				actor := session.NewLocalClientSession(ctx, "192.0.2.1:1", session.NewByJwt(claims.NetworkId, claims.UserId, claims.NetworkName, false, false))
				_, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: *claims.SessionId, OperationId: server.NewId()}, actor)
				actor.Cancel()
				if err != nil {
					t.Fatal(err)
				}
				want = uint32(session.AuthorizationRevokedCloseCode - 4000)
				lease.Recheck()
			} else {
				lease.Testing_Expire(time.Now().Add(91 * time.Second))
			}
			_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
			for {
				_, message, err := ws.ReadMessage()
				if err != nil {
					t.Fatal("transport closed without typed retirement reason", revoked, err)
				}
				if len(message) == 5 && message[0] == connectlib.TransportControlClose {
					if got := binary.BigEndian.Uint32(message[1:]); got != want {
						t.Fatal("unknown authority treated as known revoke", revoked, got, want)
					}
					break
				}
			}
		}
	})
}
