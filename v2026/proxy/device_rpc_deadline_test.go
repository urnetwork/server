// The real Proxy upgrader must retain the owned deadline guard; rejected
// liveness bounds contribute only a fixed session class, never private details.
package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Records whether Gorilla delegates bytes despite a rejected socket bound.
type proxyRpcDeadlineConn struct {
	writeErr error
	clearErr error
	writes   int
	closes   int
}

func (self *proxyRpcDeadlineConn) Read([]byte) (int, error) { return 0, io.EOF }
func (self *proxyRpcDeadlineConn) Write(p []byte) (int, error) {
	self.writes++
	return len(p), nil
}
func (self *proxyRpcDeadlineConn) Close() error                     { self.closes++; return nil }
func (self *proxyRpcDeadlineConn) LocalAddr() net.Addr              { return nil }
func (self *proxyRpcDeadlineConn) RemoteAddr() net.Addr             { return nil }
func (self *proxyRpcDeadlineConn) SetDeadline(time.Time) error      { return self.clearErr }
func (self *proxyRpcDeadlineConn) SetReadDeadline(time.Time) error  { return nil }
func (self *proxyRpcDeadlineConn) SetWriteDeadline(time.Time) error { return self.writeErr }

// The actual Gorilla server upgrade runs without a listener, auth or hosted DB.
type proxyRpcDeadlineResponse struct {
	*httptest.ResponseRecorder
	conn      *proxyRpcDeadlineConn
	hijackErr error
}

func (self *proxyRpcDeadlineResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if self.hijackErr != nil {
		return nil, nil, self.hijackErr
	}
	return self.conn, bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(nil)), bufio.NewWriter(self.conn)), nil
}

// Constructs only the protocol fixture; all names and data are synthetic.
func newProxyRpcDeadlineRequest(t *testing.T) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "http://deadline.example/device-rpc", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "c3ludGhldGljLWZpeHR1cg==")
	return request
}

func newProxyRpcDeadlineUpgrade(t *testing.T) (deviceRpcWebsocket, *proxyRpcDeadlineConn) {
	t.Helper()
	handler := NewDeviceRpcHandler(nil, nil)
	raw := &proxyRpcDeadlineConn{}
	response := &proxyRpcDeadlineResponse{ResponseRecorder: httptest.NewRecorder(), conn: raw}
	ws, err := handler.upgradeWebsocket(response, newProxyRpcDeadlineRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	raw.writes = 0
	return ws, raw
}

// Actual data writes must not escape Gorilla's ignored inner setter result.
func TestProxyRpcDeadlineActualMessageRejectsInnerError(t *testing.T) {
	ws, raw := newProxyRpcDeadlineUpgrade(t)
	raw.writeErr = errors.New("synthetic message bound rejected")
	if err := ws.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	err := ws.WriteMessage(websocket.BinaryMessage, []byte("synthetic payload"))
	if !errors.Is(err, raw.writeErr) || raw.writes != 0 || raw.closes != 1 {
		t.Fatalf("message error=%v writes=%d closes=%d", err, raw.writes, raw.closes)
	}
}

// The separate control writer reaches the same hijacked owned socket.
func TestProxyRpcDeadlineActualControlRejectsInnerError(t *testing.T) {
	ws, raw := newProxyRpcDeadlineUpgrade(t)
	raw.writeErr = errors.New("synthetic control bound rejected")
	err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second))
	if !errors.Is(err, raw.writeErr) || raw.writes != 0 || raw.closes != 1 {
		t.Fatalf("control error=%v writes=%d closes=%d", err, raw.writes, raw.closes)
	}
}

// Gorilla's pre-upgrade clear is fallible too; no upgrade bytes may escape.
func TestProxyRpcDeadlineHandshakeClearRejectsBeforeIo(t *testing.T) {
	handler := NewDeviceRpcHandler(nil, nil)
	raw := &proxyRpcDeadlineConn{clearErr: errors.New("synthetic handshake clear rejected")}
	response := &proxyRpcDeadlineResponse{ResponseRecorder: httptest.NewRecorder(), conn: raw}
	ws, err := handler.upgradeWebsocket(response, newProxyRpcDeadlineRequest(t))
	if ws != nil {
		defer ws.Close()
	}
	if !errors.Is(err, raw.clearErr) || raw.writes != 0 || raw.closes < 1 {
		t.Fatalf("upgrade error=%v writes=%d closes=%d", err, raw.writes, raw.closes)
	}
}

// Healthy framing and both ordinary writer paths are unchanged.
func TestProxyRpcDeadlineHealthyUpgradeAndWriters(t *testing.T) {
	ws, raw := newProxyRpcDeadlineUpgrade(t)
	if err := ws.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("healthy payload")); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if raw.writes != 2 || raw.closes != 0 {
		t.Fatalf("writes=%d closes=%d", raw.writes, raw.closes)
	}
}

// Optional HTTP hijacking stays an explicit error rather than a type panic.
func TestProxyRpcDeadlineMissingHijackerIsError(t *testing.T) {
	handler := NewDeviceRpcHandler(nil, nil)
	ws, err := handler.upgradeWebsocket(httptest.NewRecorder(), newProxyRpcDeadlineRequest(t))
	if ws != nil {
		defer ws.Close()
	}
	if err == nil {
		t.Fatal("non-hijackable response accepted")
	}
}

// Gorilla wraps hijack errors as HandshakeError; the original 500 response
// and lack of socket ownership must remain unchanged.
func TestProxyRpcDeadlineHijackFailurePreservesOwner(t *testing.T) {
	handler := NewDeviceRpcHandler(nil, nil)
	want := errors.New("synthetic hijack rejected")
	raw := &proxyRpcDeadlineConn{}
	response := &proxyRpcDeadlineResponse{ResponseRecorder: httptest.NewRecorder(), conn: raw, hijackErr: want}
	ws, err := handler.upgradeWebsocket(response, newProxyRpcDeadlineRequest(t))
	if ws != nil {
		defer ws.Close()
	}
	var handshakeErr websocket.HandshakeError
	if !errors.As(err, &handshakeErr) || err.Error() != want.Error() || response.Code != http.StatusInternalServerError || raw.closes != 0 || raw.writes != 0 {
		t.Fatalf("hijack=%v closes=%d writes=%d", err, raw.closes, raw.writes)
	}
}

// Only the three terminal methods differ from the existing synthetic surface.
type proxyRpcDeadlineObservedDelegate struct {
	testingDeviceRpcObservedWebsocket
	readDeadlineErr  error
	writeDeadlineErr error
	controlErr       error
}

func (self *proxyRpcDeadlineObservedDelegate) SetReadDeadline(time.Time) error {
	return self.readDeadlineErr
}
func (self *proxyRpcDeadlineObservedDelegate) SetWriteDeadline(time.Time) error {
	return self.writeDeadlineErr
}
func (self *proxyRpcDeadlineObservedDelegate) WriteControl(int, []byte, time.Time) error {
	return self.controlErr
}

// The assertion checks the entire fixed output, preventing raw error leakage.
func assertProxyRpcDeadlineObservation(t *testing.T, ws *deviceRpcObservedWebsocket, want string) {
	t.Helper()
	stage, result, ingress, egress := ws.observation()
	if stage != "transport" || result != want || ingress != "absent" || egress != "absent" {
		t.Fatalf("observation=%s/%s/%s/%s", stage, result, ingress, egress)
	}
}

func TestProxyRpcDeadlineReadFailureKeepsBoundedClass(t *testing.T) {
	want := errors.New("synthetic read failure secret=must-not-emit")
	ws := newDeviceRpcObservedWebsocket(&proxyRpcDeadlineObservedDelegate{readDeadlineErr: want})
	if err := ws.SetReadDeadline(time.Now()); !errors.Is(err, want) {
		t.Fatal(err)
	}
	assertProxyRpcDeadlineObservation(t, ws, "io-error")
}

func TestProxyRpcDeadlineWriteFailureKeepsBoundedClass(t *testing.T) {
	want := errors.New("synthetic write failure secret=must-not-emit")
	ws := newDeviceRpcObservedWebsocket(&proxyRpcDeadlineObservedDelegate{writeDeadlineErr: want})
	if err := ws.SetWriteDeadline(time.Now()); !errors.Is(err, want) {
		t.Fatal(err)
	}
	assertProxyRpcDeadlineObservation(t, ws, "io-error")
}

func TestProxyRpcDeadlineControlFailureKeepsBoundedClass(t *testing.T) {
	want := errors.New("synthetic control failure secret=must-not-emit")
	ws := newDeviceRpcObservedWebsocket(&proxyRpcDeadlineObservedDelegate{controlErr: want})
	if err := ws.WriteControl(websocket.PingMessage, nil, time.Now()); !errors.Is(err, want) {
		t.Fatal(err)
	}
	assertProxyRpcDeadlineObservation(t, ws, "io-error")
}

// Nil setters/control traffic do not fabricate non-empty binary transfer.
func TestProxyRpcDeadlineHealthyObservationStaysEmpty(t *testing.T) {
	ws := newDeviceRpcObservedWebsocket(&proxyRpcDeadlineObservedDelegate{})
	if err := ws.SetReadDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ws.SetWriteDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteControl(websocket.PingMessage, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	assertProxyRpcDeadlineObservation(t, ws, "local-close")
}

// A later bound failure must not replace an already classified orderly close.
func TestProxyRpcDeadlineEarlierTerminalClassRemains(t *testing.T) {
	want := errors.New("synthetic later deadline rejection")
	ws := newDeviceRpcObservedWebsocket(&proxyRpcDeadlineObservedDelegate{readDeadlineErr: want})
	ws.observeError(&websocket.CloseError{Code: websocket.CloseNormalClosure})
	if err := ws.SetReadDeadline(time.Now()); !errors.Is(err, want) {
		t.Fatal(err)
	}
	assertProxyRpcDeadlineObservation(t, ws, "orderly-close")
}
