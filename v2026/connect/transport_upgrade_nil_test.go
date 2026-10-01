// Upgrade failure never transfers a nil concrete connection into the deferred
// interface owner. These tests call the actual Connect handler and pinned
// Gorilla upgrader; synthetic in-memory sockets isolate cleanup without traffic.
package connect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Only the handler goroutine uses this socket; failed/healthy upgrade return
// joins its request worker before tests inspect counters. No borrowed close.
type upgradeNilSocket struct {
	bytes.Buffer
	writeErr error
	reads    int
	writes   int
	closes   int
}

func (self *upgradeNilSocket) Read([]byte) (int, error) { self.reads++; return 0, io.EOF }
func (self *upgradeNilSocket) Write(p []byte) (int, error) {
	self.writes++
	if self.writeErr != nil {
		return 0, self.writeErr
	}
	return self.Buffer.Write(p)
}
func (self *upgradeNilSocket) Close() error { self.closes++; return nil }
func (self *upgradeNilSocket) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 41000}
}
func (self *upgradeNilSocket) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(198, 51, 100, 1), Port: 41001}
}
func (self *upgradeNilSocket) SetDeadline(time.Time) error      { return nil }
func (self *upgradeNilSocket) SetReadDeadline(time.Time) error  { return nil }
func (self *upgradeNilSocket) SetWriteDeadline(time.Time) error { return nil }

type upgradeNilResponse struct {
	*httptest.ResponseRecorder
	conn      *upgradeNilSocket
	hijackErr error
	hijacks   int
}

func (self *upgradeNilResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	self.hijacks++
	if self.hijackErr != nil {
		return nil, nil, self.hijackErr
	}
	return self.conn, bufio.NewReadWriter(bufio.NewReader(self.conn), bufio.NewWriter(self.conn)), nil
}

// Requests use a generated challenge and documentation address, never a copied
// production header or identity. Authentication is absent unless a test mints it.
func upgradeNilRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://transport.example/", nil)
	r.RemoteAddr = "198.51.100.27:41000"
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString([]byte("synthetic-key-16")))
	return r
}

func upgradeNilHandler() *ConnectHandler {
	h := newLifecycleTestConnectHandler()
	h.handlerId = server.NewId()
	h.settings = DefaultConnectHandlerSettings()
	h.settings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
	h.exchange = &Exchange{settings: DefaultExchangeSettings()}
	h.exchange.settings.EnableDrainCoordination = false
	return h
}

// Catch only to assert the recovered application error is absent, not to hide
// it. Production router recovery is outside this owner and need not be copied.
func upgradeNilInvoke(h *ConnectHandler, w http.ResponseWriter, r *http.Request) (recovered any) {
	defer func() { recovered = recover() }()
	h.Connect(w, r)
	return
}

func upgradeNilReject(t *testing.T, mutate func(*http.Request), wantStatus int) {
	t.Helper()
	server.DefaultTestEnv().Run(t, func(testing.TB) {
		h := upgradeNilHandler()
		defer h.Close()
		r := upgradeNilRequest()
		mutate(r)
		w := httptest.NewRecorder()
		if recovered := upgradeNilInvoke(h, w, r); recovered != nil {
			t.Errorf("rejected upgrade panicked during deferred cleanup: %T", recovered)
		}
		if w.Code != wantStatus {
			t.Errorf("rejection status=%d want=%d", w.Code, wantStatus)
		}
		if h.activeCount != 0 {
			t.Error("rejected handler retained active ownership")
		}
	})
}

func TestConnectUpgradeNilMissingUpgradeDoesNotPanic(t *testing.T) {
	upgradeNilReject(t, func(r *http.Request) { r.Header.Del("Connection") }, http.StatusBadRequest)
}

func TestConnectUpgradeNilWrongMethodDoesNotPanic(t *testing.T) {
	upgradeNilReject(t, func(r *http.Request) { r.Method = http.MethodPost }, http.StatusMethodNotAllowed)
}

func TestConnectUpgradeNilWrongVersionDoesNotPanic(t *testing.T) {
	upgradeNilReject(t, func(r *http.Request) { r.Header.Set("Sec-WebSocket-Version", "12") }, http.StatusBadRequest)
}

func TestConnectUpgradeNilOriginRejectionDoesNotPanic(t *testing.T) {
	upgradeNilReject(t, func(r *http.Request) { r.Header.Set("Origin", "https://different.example") }, http.StatusForbidden)
}

func TestConnectUpgradeNilMissingHijackerDoesNotPanic(t *testing.T) {
	upgradeNilReject(t, func(*http.Request) {}, http.StatusInternalServerError)
}

func TestConnectUpgradeNilFailedHijackKeepsBorrowedSocket(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(testing.TB) {
		h := upgradeNilHandler()
		defer h.Close()
		conn := &upgradeNilSocket{}
		w := &upgradeNilResponse{ResponseRecorder: httptest.NewRecorder(), conn: conn, hijackErr: errors.New("synthetic rejected hijack")}
		if recovered := upgradeNilInvoke(h, w, upgradeNilRequest()); recovered != nil {
			t.Errorf("failed hijack panicked during deferred cleanup: %T", recovered)
		}
		if w.Code != http.StatusInternalServerError || w.hijacks != 1 || conn.closes != 0 || conn.writes != 0 {
			t.Errorf("failed hijack transferred ownership: status=%d hijacks=%d closes=%d writes=%d", w.Code, w.hijacks, conn.closes, conn.writes)
		}
	})
}

func TestConnectUpgradeNilFailedHandshakeClosesExactlyOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(testing.TB) {
		h := upgradeNilHandler()
		defer h.Close()
		conn := &upgradeNilSocket{writeErr: errors.New("synthetic handshake write failure")}
		w := &upgradeNilResponse{ResponseRecorder: httptest.NewRecorder(), conn: conn}
		if recovered := upgradeNilInvoke(h, w, upgradeNilRequest()); recovered != nil {
			t.Errorf("failed handshake panicked during deferred cleanup: %T", recovered)
		}
		if w.hijacks != 1 || conn.closes != 1 || conn.writes != 1 {
			t.Errorf("failed handshake cleanup changed: hijacks=%d closes=%d writes=%d", w.hijacks, conn.closes, conn.writes)
		}
	})
}

func TestConnectUpgradeNilSuccessfulUpgradeRetainsCloseOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(testing.TB) {
		h := upgradeNilHandler()
		defer h.Close()
		conn := &upgradeNilSocket{}
		w := &upgradeNilResponse{ResponseRecorder: httptest.NewRecorder(), conn: conn}
		if recovered := upgradeNilInvoke(h, w, upgradeNilRequest()); recovered != nil {
			t.Errorf("successful upgrade cleanup panicked: %T", recovered)
		}
		if w.hijacks != 1 || conn.closes != 1 || conn.reads == 0 || !strings.HasPrefix(conn.String(), "HTTP/1.1 101 ") {
			t.Errorf("healthy upgrade ownership/framing changed: hijacks=%d closes=%d reads=%d", w.hijacks, conn.closes, conn.reads)
		}
	})
}

func TestConnectUpgradeNilEarlyAddressRejectionHasNoOwner(t *testing.T) {
	h := upgradeNilHandler()
	defer h.Close()
	r := upgradeNilRequest()
	r.RemoteAddr = "synthetic-unparseable"
	if recovered := upgradeNilInvoke(h, httptest.NewRecorder(), r); recovered != nil {
		t.Errorf("pre-upgrade rejection panicked: %T", recovered)
	}
	if h.activeCount != 0 {
		t.Error("early rejection retained active ownership")
	}
}

func TestConnectUpgradeNilCustomAuthRejectionHasNoOwner(t *testing.T) {
	upgradeNilReject(t, func(r *http.Request) { r.Header.Set("Upgrade", clientconnect.H1FramerProtocol) }, http.StatusUnauthorized)
}

func TestConnectUpgradeNilFramedConstructorFailureClosesOwnedSocket(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		networkId, userId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-upgrade-network", userId)
		userSession := session.Testing_CreateClientSession(ctx, jwt.NewByJwt(networkId, userId, "synthetic-upgrade-network", false, false))
		client, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "synthetic upgrade client", DeviceSpec: "synthetic"}, userSession)
		if err != nil || client == nil || client.Error != nil || client.ByClientJwt == nil {
			tb.Fatalf("synthetic authentication setup failed: %T", err)
		}
		h := upgradeNilHandler()
		defer h.Close()
		h.settings.FramerSettings.MaxMessageLen = -1
		conn := &upgradeNilSocket{}
		w := &upgradeNilResponse{ResponseRecorder: httptest.NewRecorder(), conn: conn}
		r := upgradeNilRequest()
		r.Header.Set("Upgrade", clientconnect.H1FramerProtocol)
		r.Header.Set("Authorization", "Bearer "+*client.ByClientJwt)
		r.Header.Set("X-UR-InstanceId", server.NewId().String())
		if recovered := upgradeNilInvoke(h, w, r); recovered != nil {
			t.Errorf("failed framed constructor panicked during deferred cleanup: %T", recovered)
		}
		if w.hijacks != 1 || conn.closes != 1 || !strings.HasPrefix(conn.String(), "HTTP/1.1 101 ") {
			t.Errorf("framed constructor failure bypassed boundary or changed cleanup: status=%d hijacks=%d closes=%d", w.Code, w.hijacks, conn.closes)
		}
	})
}
