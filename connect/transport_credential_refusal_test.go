// A refused credential must reach the client as a refusal on every carrier,
// and its retries must not repeat the state query. These run the production
// Connect entry points against real state; each assertion reads the exact
// frames the client received, never a timing.
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
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/quic-go/quic-go"
	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// a release older than the day window, so its label is its month
const testingRefusalAppVersion = "2026.3.26-897899540"

func testingRefusedClientToken(t testing.TB, ctx context.Context) string {
	t.Helper()
	claims := newConnectAuthenticationWorkClaims(ctx)
	token := claims.Testing_Sign()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, *claims.ClientId))
	})
	return token
}

// Dials one WebSocket admission and returns what the client read after its
// 101: the first message, then the error that ended the connection.
func dialRefusedWebSocket(t testing.TB, ctx context.Context, env *peerDiscoveryEnv, token string, frameAuth bool) (int, []byte, error) {
	t.Helper()
	instance := server.NewId()
	headers := authenticationDeadlineHeaders(token, instance)
	headers.Set("X-UR-AppVersion", testingRefusalAppVersion)
	if frameAuth {
		headers.Del("Authorization")
	}
	dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ws, _, err := dialer.DialContext(ctx, fmt.Sprintf("ws://127.0.0.1:%d/", env.port), headers)
	if err != nil {
		t.Fatalf("the refused credential's WebSocket was not upgraded: %v", err)
	}
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	if frameAuth {
		authBytes, err := clientconnect.EncodeFrame(&protocol.Auth{ByJwt: token, InstanceId: instance.Bytes(), AppVersion: testingRefusalAppVersion}, clientconnect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		if err := ws.WriteMessage(websocket.BinaryMessage, authBytes); err != nil {
			t.Fatal(err)
		}
	}
	messageType, message, err := ws.ReadMessage()
	if err != nil {
		return 0, nil, err
	}
	_, _, closeErr := ws.ReadMessage()
	return messageType, message, closeErr
}

// A WebSocket refusal follows the 101, so without an in-band close the
// client saw only a drop and redialed it at once. Every refusal now carries
// the authorization close, and a credential's retries run one state query.
func TestConnectWebSocketRefusalCarriesTheAuthorizationClose(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		env := testing_newPeerDiscoveryEnv(ctx, t)
		defer env.Close()
		// a child read directly exists before its first refusal
		refusals := connectCredentialRefusals.WithLabelValues("websocket", "2026.3")
		for _, frameAuth := range []bool{false, true} {
			token := testingRefusedClientToken(t, ctx)
			queriesBefore := connectStateQueryMetric(t, "connect_h1", "handshake", "client", "no_active_row")
			refusalsBefore := testutil.ToFloat64(refusals)
			const retries = 3
			for range retries {
				messageType, message, closeErr := dialRefusedWebSocket(t, ctx, env, token, frameAuth)
				reason, ok := uint32(0), false
				if messageType == websocket.BinaryMessage && len(message) == 5 && message[0] == clientconnect.TransportControlClose {
					reason, ok = uint32(message[1])<<24|uint32(message[2])<<16|uint32(message[3])<<8|uint32(message[4]), true
				}
				if !ok || reason != clientconnect.TransportCloseReasonCredentialRejected {
					t.Fatalf("frame auth = %t: the refusal was not an authorization close: message type %d, %d bytes, read err %v", frameAuth, messageType, len(message), closeErr)
				}
				var closeError *websocket.CloseError
				if !errors.As(closeErr, &closeError) || closeError.Code != session.AuthorizationRejectedCloseCode {
					t.Fatalf("frame auth = %t: the refusal did not close with %d: %v", frameAuth, session.AuthorizationRejectedCloseCode, closeErr)
				}
			}
			if queries := connectStateQueryMetric(t, "connect_h1", "handshake", "client", "no_active_row") - queriesBefore; queries != 1 {
				t.Errorf("frame auth = %t: %d refused handshakes ran %.0f state queries, want 1", frameAuth, retries, queries)
			}
			if attributed := testutil.ToFloat64(refusals) - refusalsBefore; attributed != retries {
				t.Errorf("frame auth = %t: %.0f of %d refusals were attributed to their release", frameAuth, attributed, retries)
			}
		}
	})
}

// H3 closed a refused connection with code 0, which no client reads as a
// refusal; H1+ keeps its terminal 401 before any upgrade.
func TestConnectH3AndH1PlusRefusalsAreDefinitive(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		env := testing_newPeerDiscoveryEnv(ctx, t)
		defer env.Close()
		token := testingRefusedClientToken(t, ctx)

		conn, err := quic.DialAddr(ctx, fmt.Sprintf("127.0.0.1:%d", env.h3Port), &tls.Config{InsecureSkipVerify: true, ServerName: "127.0.0.1"}, &quic.Config{MaxIdleTimeout: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseWithError(0, "")
		stream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_ = stream.SetDeadline(time.Now().Add(10 * time.Second))
		authBytes, err := clientconnect.EncodeFrame(&protocol.Auth{ByJwt: token, InstanceId: server.NewId().Bytes(), AppVersion: testingRefusalAppVersion}, clientconnect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		framer := clientconnect.NewFramer(env.handler.settings.FramerSettings)
		if err := framer.Write(stream, authBytes); err != nil {
			t.Fatal(err)
		}
		_, readErr := framer.Read(stream)
		var applicationErr *quic.ApplicationError
		if !errors.As(readErr, &applicationErr) || !applicationErr.Remote || applicationErr.ErrorCode != quic.ApplicationErrorCode(session.AuthorizationRejectedCloseCode) {
			t.Fatalf("the h3 refusal did not close with %d: %v", session.AuthorizationRejectedCloseCode, readErr)
		}

		headers := authenticationDeadlineHeaders(token, server.NewId())
		_, err = clientconnect.DialFramedUpgrade(ctx, fmt.Sprintf("ws://127.0.0.1:%d/", env.port), headers, &websocket.Dialer{HandshakeTimeout: 5 * time.Second}, clientconnect.H1FramerProtocol)
		var rejection *clientconnect.HTTPUpgradeError
		if !errors.As(err, &rejection) || rejection.StatusCode != http.StatusUnauthorized || !rejection.Terminal {
			t.Fatalf("the h1+ refusal was not a terminal 401: %v", err)
		}
	})
}
