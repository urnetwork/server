// The refusal hold runs on the test's clock and tick: a hold reports its
// outcome and every served tick reports back, so each assertion reads the
// state the hold left at a barrier, never after a sleep.
package connect

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

type testingRefusalHoldClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func (self *testingRefusalHoldClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testingRefusalHoldClock) set(now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = now
}

// testingHeldConn records what the hold writes to a held socket.
type testingHeldConn struct {
	stateLock sync.Mutex
	messages  [][]byte
	closed    bool
	writeErr  error
}

func (self *testingHeldConn) Close() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed = true
	return nil
}

func (self *testingHeldConn) WriteMessage(messageType int, message []byte) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.writeErr != nil {
		return self.writeErr
	}
	self.messages = append(self.messages, append([]byte{}, message...))
	return nil
}

func (self *testingHeldConn) ReadMessage() (int, []byte, error) {
	return 0, nil, errors.New("a held socket is never read")
}

func (self *testingHeldConn) NextReader() (int, io.Reader, error) {
	return 0, nil, errors.New("a held socket is never read")
}

func (self *testingHeldConn) SetReadLimit(int64)               {}
func (self *testingHeldConn) SetReadDeadline(time.Time) error  { return nil }
func (self *testingHeldConn) SetWriteDeadline(time.Time) error { return nil }
func (self *testingHeldConn) UnderlyingConn() net.Conn         { return nil }

func (self *testingHeldConn) state() ([][]byte, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([][]byte{}, self.messages...), self.closed
}

// A hold on the test's clock whose run loop serves the test's ticks.
type testingRefusalHold struct {
	hold     *connectRefusalHold
	clock    *testingRefusalHoldClock
	tick     chan time.Time
	ticked   chan struct{}
	draining atomic.Bool
	cancel   context.CancelFunc
	done     chan struct{}
}

func newTestingRefusalHold(t *testing.T, maxCount int) *testingRefusalHold {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	fixture := &testingRefusalHold{
		clock:  &testingRefusalHoldClock{now: time.Date(2026, 10, 11, 4, 0, 0, 0, time.UTC)},
		tick:   make(chan time.Time),
		ticked: make(chan struct{}, 16),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	settings := DefaultConnectHandlerSettings()
	settings.RefusalHoldMaxCount = maxCount
	settings.refusalHoldNowForTest = fixture.clock.Now
	settings.refusalHoldTickForTest = fixture.tick
	settings.refusalHoldTickedForTest = func() { fixture.ticked <- struct{}{} }
	fixture.hold = newConnectRefusalHold(ctx, fixture.draining.Load, settings)
	go func() {
		defer close(fixture.done)
		fixture.hold.run()
	}()
	t.Cleanup(func() {
		cancel()
		<-fixture.done
	})
	return fixture
}

// Moves the clock to at and waits until the hold served one tick there.
func (self *testingRefusalHold) tickAt(t *testing.T, at time.Time) {
	t.Helper()
	self.clock.set(at)
	select {
	case self.tick <- at:
	case <-time.After(10 * time.Second):
		t.Fatal("the hold took no tick")
	}
	select {
	case <-self.ticked:
	case <-time.After(10 * time.Second):
		t.Fatal("the hold did not serve its tick")
	}
}

func testingTransportCloseControl(reason uint32) []byte {
	control := []byte{clientconnect.TransportControlClose, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(control[1:], reason)
	return control
}

// Holds release in hold order at their timeout, each with its own close, and
// keepalives go out once per interval until then.
func TestConnectRefusalHoldReleasesAtItsTimeoutWithItsClose(t *testing.T) {
	fixture := newTestingRefusalHold(t, 4)
	start := fixture.clock.Now()
	rejected := &testingHeldConn{}
	revoked := &testingHeldConn{}
	if !fixture.hold.hold(rejected, session.AuthorizationRejectedCloseCode) {
		t.Fatal("the first refusal was not held")
	}
	fixture.clock.set(start.Add(10 * time.Second))
	if !fixture.hold.hold(revoked, session.AuthorizationRevokedCloseCode) {
		t.Fatal("the second refusal was not held")
	}

	fixture.tickAt(t, start.Add(15*time.Second))
	for _, conn := range []*testingHeldConn{rejected, revoked} {
		if messages, closed := conn.state(); closed || len(messages) != 1 || len(messages[0]) != 0 {
			t.Fatalf("after one keepalive interval: %d messages, closed = %t; want one empty keepalive", len(messages), closed)
		}
	}
	fixture.tickAt(t, start.Add(time.Minute-time.Nanosecond))
	if _, closed := rejected.state(); closed || fixture.hold.count() != 2 {
		t.Fatal("a refusal was released before its hold timeout")
	}
	fixture.tickAt(t, start.Add(time.Minute))
	messages, closed := rejected.state()
	if !closed || string(messages[len(messages)-1]) != string(testingTransportCloseControl(clientconnect.TransportCloseReasonCredentialRejected)) {
		t.Fatalf("at its timeout the first refusal got %d messages, closed = %t; want its rejected close", len(messages), closed)
	}
	if _, closed := revoked.state(); closed || fixture.hold.count() != 1 {
		t.Fatal("a later refusal was released with the first")
	}
	fixture.tickAt(t, start.Add(10*time.Second+time.Minute))
	messages, closed = revoked.state()
	if !closed || string(messages[len(messages)-1]) != string(testingTransportCloseControl(clientconnect.TransportCloseReasonSessionRevoked)) {
		t.Fatal("the revoked refusal did not close with its revoked close")
	}
	if fixture.hold.count() != 0 {
		t.Fatalf("%d refusals still held", fixture.hold.count())
	}
}

// A held socket whose keepalive fails has no peer left: its hold ends at once
// and frees its place.
func TestConnectRefusalHoldEndsAHoldWhoseKeepaliveFails(t *testing.T) {
	fixture := newTestingRefusalHold(t, 1)
	start := fixture.clock.Now()
	gone := &testingHeldConn{writeErr: errors.New("synthetic broken pipe")}
	if !fixture.hold.hold(gone, session.AuthorizationRejectedCloseCode) {
		t.Fatal("the refusal was not held")
	}
	if fixture.hold.hold(&testingHeldConn{}, session.AuthorizationRejectedCloseCode) {
		t.Fatal("a refusal past the bound was held")
	}
	fixture.tickAt(t, start.Add(15*time.Second))
	if _, closed := gone.state(); !closed || fixture.hold.count() != 0 {
		t.Fatal("a failed keepalive kept its hold")
	}
	if !fixture.hold.hold(&testingHeldConn{}, session.AuthorizationRejectedCloseCode) {
		t.Fatal("the ended hold did not free its place")
	}
	fixture.tickAt(t, start.Add(time.Minute))
	if messages, _ := gone.state(); len(messages) != 0 {
		t.Fatal("the released socket was written again at its timeout")
	}
}

// A drain or a close of the handler releases every hold with its close at
// once, and later refusals are not held.
func TestConnectRefusalHoldReleasesEveryHoldOnDrainAndClose(t *testing.T) {
	fixture := newTestingRefusalHold(t, 4)
	drained := &testingHeldConn{}
	if !fixture.hold.hold(drained, session.AuthorizationRejectedCloseCode) {
		t.Fatal("the refusal was not held")
	}
	fixture.draining.Store(true)
	fixture.tickAt(t, fixture.clock.Now())
	if messages, closed := drained.state(); !closed || len(messages) != 1 || string(messages[0]) != string(testingTransportCloseControl(clientconnect.TransportCloseReasonCredentialRejected)) {
		t.Fatal("a drain did not release the hold with its close")
	}
	if fixture.hold.hold(&testingHeldConn{}, session.AuthorizationRejectedCloseCode) {
		t.Fatal("a refusal was held during a drain")
	}

	closing := newTestingRefusalHold(t, 4)
	closed := &testingHeldConn{}
	if !closing.hold.hold(closed, session.AuthorizationRejectedCloseCode) {
		t.Fatal("the refusal was not held")
	}
	closing.cancel()
	<-closing.done
	if _, isClosed := closed.state(); !isClosed || closing.hold.count() != 0 {
		t.Fatal("closing the handler did not release the hold")
	}
	if closing.hold.hold(&testingHeldConn{}, session.AuthorizationRejectedCloseCode) {
		t.Fatal("a refusal was held after the handler closed")
	}
}

// A real handler without listeners, constructed as in production, whose hold
// runs on the test's clock and tick.
type testingRefusalHoldHandler struct {
	handler *ConnectHandler
	clock   *testingRefusalHoldClock
	tick    chan time.Time
	ticked  chan struct{}
	held    chan string
	url     string
}

func newTestingRefusalHoldHandler(t testing.TB, maxCount int) *testingRefusalHoldHandler {
	t.Helper()
	fixture := &testingRefusalHoldHandler{
		clock:  &testingRefusalHoldClock{now: time.Date(2026, 10, 11, 4, 0, 0, 0, time.UTC)},
		tick:   make(chan time.Time),
		ticked: make(chan struct{}, 16),
		held:   make(chan string, 16),
	}
	settings := DefaultConnectHandlerSettings()
	settings.ListenH3Port = 0
	settings.ListenDnsPort = 0
	settings.EnableProxyProtocol = false
	settings.TransportTlsSettings.EnableSelfSign = true
	settings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
	settings.transportTlsLoader = func(tlsSettings *server.TransportTlsSettings) (*server.TransportTls, error) {
		return server.NewTransportTls(map[string]bool{}, tlsSettings), nil
	}
	settings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
	settings.RefusalHoldMaxCount = maxCount
	settings.refusalHoldNowForTest = fixture.clock.Now
	settings.refusalHoldTickForTest = fixture.tick
	settings.refusalHoldHeldForTest = func(outcome string) { fixture.held <- outcome }
	settings.refusalHoldTickedForTest = func() { fixture.ticked <- struct{}{} }
	exchange := &Exchange{settings: DefaultExchangeSettings()}
	exchange.settings.EnableDrainCoordination = false
	handler, err := newConnectHandlerWithPacketConns(context.Background(), server.NewId(), exchange, settings, ConnectHandlerPacketConns{})
	if err != nil {
		t.Fatal(err)
	}
	fixture.handler = handler
	routerCtx, routerCancel := context.WithCancel(context.Background())
	routes := router.NewRouter(routerCtx, []*router.Route{router.NewRoute(http.MethodGet, "/", handler.Connect)})
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "198.51.100.208:45000"
		routes.ServeHTTP(w, r)
	}))
	fixture.url = "ws" + strings.TrimPrefix(front.URL, "http")
	t.Cleanup(func() {
		front.Close()
		routerCancel()
		handler.Close()
	})
	return fixture
}

func (self *testingRefusalHoldHandler) tickAt(t testing.TB, at time.Time) {
	t.Helper()
	self.clock.set(at)
	select {
	case self.tick <- at:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler's hold took no tick")
	}
	select {
	case <-self.ticked:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler's hold did not serve its tick")
	}
}

// One frame a client read, or the error that ended its reads.
type testingRefusalFrame struct {
	messageType int
	message     []byte
	err         error
}

// Dials one WebSocket admission and streams each frame the client reads,
// ending with the error that closed it. A client without stream leases cannot
// read the authorization close.
func dialTestingRefusal(t testing.TB, url string, token string, leased bool, frameAuth bool) <-chan testingRefusalFrame {
	t.Helper()
	instance := server.NewId()
	headers := authenticationDeadlineHeaders(token, instance)
	if leased {
		headers.Set("X-UR-StreamLeaseVersion", "1")
	}
	if frameAuth {
		headers.Del("Authorization")
	}
	ws, response, err := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second}).Dial(url, headers)
	if err != nil || response == nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the refused credential's WebSocket was not upgraded: %v", err)
	}
	t.Cleanup(func() { ws.Close() })
	if frameAuth {
		authBytes, err := clientconnect.EncodeFrame(&protocol.Auth{ByJwt: token, InstanceId: instance.Bytes()}, clientconnect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		if err := ws.WriteMessage(websocket.BinaryMessage, authBytes); err != nil {
			t.Fatal(err)
		}
	}
	frames := make(chan testingRefusalFrame, 64)
	go func() {
		for {
			messageType, message, err := ws.ReadMessage()
			frames <- testingRefusalFrame{messageType: messageType, message: message, err: err}
			if err != nil {
				return
			}
		}
	}()
	return frames
}

func nextTestingRefusalFrame(t testing.TB, frames <-chan testingRefusalFrame) testingRefusalFrame {
	t.Helper()
	select {
	case frame := <-frames:
		return frame
	case <-time.After(10 * time.Second):
		t.Fatal("the client read nothing")
		return testingRefusalFrame{}
	}
}

// The next frames are the authorization close: the close control with the
// reason, then the WebSocket close with its code.
func expectTestingRefusalClose(t testing.TB, label string, frames <-chan testingRefusalFrame) {
	t.Helper()
	frame := nextTestingRefusalFrame(t, frames)
	if frame.err != nil || frame.messageType != websocket.BinaryMessage || string(frame.message) != string(testingTransportCloseControl(clientconnect.TransportCloseReasonCredentialRejected)) {
		t.Fatalf("%s: the client read %d bytes (err %v), want the rejected close control", label, len(frame.message), frame.err)
	}
	frame = nextTestingRefusalFrame(t, frames)
	var closeError *websocket.CloseError
	if !errors.As(frame.err, &closeError) || closeError.Code != session.AuthorizationRejectedCloseCode {
		t.Fatalf("%s: the socket ended with %v, want close %d", label, frame.err, session.AuthorizationRejectedCloseCode)
	}
}

// The first thing a refusal of a client without stream leases does is the
// hold. Closing at once was the old behavior: such clients redial within five
// seconds.
func waitTestingRefusalHeld(t testing.TB, fixture *testingRefusalHoldHandler, frames <-chan testingRefusalFrame, want string) {
	t.Helper()
	select {
	case outcome := <-fixture.held:
		if outcome != want {
			t.Fatalf("hold outcome %q, want %q", outcome, want)
		}
	case frame := <-frames:
		t.Fatalf("the refusal closed at once instead of being held (read %d bytes, err %v)", len(frame.message), frame.err)
	case <-time.After(15 * time.Second):
		t.Fatal("the refusal reached no hold")
	}
}

// A dead client's WebSocket refusal is held, kept alive inside the 30 second
// read deadline of the old builds that cannot read the close, and closed with
// the authorization close at the hold timeout. Clients that read the close,
// and frame-auth clients, are closed at once.
func TestConnectWebSocketRefusalHeldUntilTheHoldTime(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		fixture := newTestingRefusalHoldHandler(t, 4)
		start := fixture.clock.Now()
		token := testingRefusedClientToken(t, ctx)

		frames := dialTestingRefusal(t, fixture.url, token, false, false)
		waitTestingRefusalHeld(t, fixture, frames, "held")
		fixture.tickAt(t, start.Add(15*time.Second))
		if frame := nextTestingRefusalFrame(t, frames); frame.err != nil || frame.messageType != websocket.BinaryMessage || len(frame.message) != 0 {
			t.Fatalf("the held client read %d bytes (err %v), want an empty keepalive", len(frame.message), frame.err)
		}
		fixture.tickAt(t, start.Add(time.Minute-time.Nanosecond))
		if count := fixture.handler.refusalHold.count(); count != 1 {
			t.Fatalf("%d refusals held just before the hold timeout, want 1", count)
		}
		fixture.tickAt(t, start.Add(time.Minute))
		if frame := nextTestingRefusalFrame(t, frames); frame.err != nil || len(frame.message) != 0 {
			t.Fatalf("the held client read %d bytes (err %v), want its second keepalive", len(frame.message), frame.err)
		}
		expectTestingRefusalClose(t, "at the hold timeout", frames)
		if count := fixture.handler.refusalHold.count(); count != 0 {
			t.Fatalf("%d refusals still held after the hold timeout", count)
		}

		for _, control := range []struct {
			label     string
			leased    bool
			frameAuth bool
		}{
			{label: "stream leases", leased: true},
			{label: "frame auth", frameAuth: true},
		} {
			expectTestingRefusalClose(t, control.label, dialTestingRefusal(t, fixture.url, token, control.leased, control.frameAuth))
		}
		if count := fixture.handler.refusalHold.count(); count != 0 {
			t.Fatalf("a client that reads the close was held: %d held", count)
		}
	})
}

// Past the bound a refusal closes at once, and the held one stays held.
func TestConnectWebSocketRefusalOverTheHoldCapClosesAtOnce(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		fixture := newTestingRefusalHoldHandler(t, 1)
		heldFrames := dialTestingRefusal(t, fixture.url, "synthetic-invalid-token", false, false)
		waitTestingRefusalHeld(t, fixture, heldFrames, "held")

		overFrames := dialTestingRefusal(t, fixture.url, "synthetic-invalid-token", false, false)
		select {
		case outcome := <-fixture.held:
			if outcome != "over_cap" {
				t.Fatalf("hold outcome past the bound %q, want over_cap", outcome)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the refusal past the bound reached no hold")
		}
		expectTestingRefusalClose(t, "past the bound", overFrames)
		if count := fixture.handler.refusalHold.count(); count != 1 {
			t.Fatalf("%d refusals held, want the first only", count)
		}
		select {
		case frame := <-heldFrames:
			t.Fatalf("the held refusal was released by a refusal past the bound (read %d bytes, err %v)", len(frame.message), frame.err)
		default:
		}
	})
}

// A drain releases every hold with its close without waiting for the hold
// timeout, and a refusal during the drain closes at once.
func TestConnectWebSocketRefusalHoldReleasedOnDrain(t *testing.T) {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		fixture := newTestingRefusalHoldHandler(t, 4)
		frames := dialTestingRefusal(t, fixture.url, "synthetic-invalid-token", false, false)
		waitTestingRefusalHeld(t, fixture, frames, "held")

		fixture.handler.exchange.draining.Store(true)
		fixture.tickAt(t, fixture.clock.Now())
		expectTestingRefusalClose(t, "on drain", frames)
		if count := fixture.handler.refusalHold.count(); count != 0 {
			t.Fatalf("%d refusals still held after the drain", count)
		}

		duringFrames := dialTestingRefusal(t, fixture.url, "synthetic-invalid-token", false, false)
		select {
		case outcome := <-fixture.held:
			if outcome != "closing" {
				t.Fatalf("hold outcome during the drain %q, want closing", outcome)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the refusal during the drain reached no hold")
		}
		expectTestingRefusalClose(t, "during the drain", duringFrames)
		if count := fixture.handler.refusalHold.count(); count != 0 {
			t.Fatalf("%d refusals held during the drain", count)
		}
	})
}
