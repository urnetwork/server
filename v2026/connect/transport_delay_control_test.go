// The runtime switch for the delay header (transport_delay_control.go,
// connect DELAY.md §11). The hermetic tests read the control from a test store
// that narrows a hash as `{connect_move}control` holds it, and drive the
// handler's H1, H1+ and H3 accept decisions; the last test reads the real
// control in Redis.
package connect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The Redis control as a test sets it: the hash's fields, a read error, or a
// read that raises.
type testTransportDelayControlStore struct {
	stateLock sync.Mutex
	fields    map[string]string
	err       error
	raise     bool
	reads     int
}

func (self *testTransportDelayControlStore) set(fields map[string]string, err error, raise bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.fields = fields
	self.err = err
	self.raise = raise
}

func (self *testTransportDelayControlStore) readCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.reads
}

// model.GetConnectMoveControl over the test's hash.
func (self *testTransportDelayControlStore) read(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.reads += 1
	if self.raise {
		panic(errors.New("synthetic control read raise"))
	}
	if self.err != nil {
		return nil, self.err
	}
	return model.NarrowConnectMoveControl(self.fields, ceiling), nil
}

// A handler with the default settings and a switch over store, without its
// loop: tests read the control in the loop's place.
func testTransportDelayControlHandler(ctx context.Context, store *testTransportDelayControlStore) *ConnectHandler {
	settings := DefaultConnectHandlerSettings()
	return &ConnectHandler{
		ctx:                   ctx,
		settings:              settings,
		transportDelayControl: newConnectTransportDelayControlWithStore(ctx, settings, store.read),
	}
}

// The handler's H1 decision, which the WebSocket and h1+ carriers share:
// whether the 101 to a header-auth request accepts the header.
func testTransportDelayControlH1Accepts(handler *ConnectHandler, offer bool, provideIntent bool) bool {
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	if offer {
		r.Header.Add("Sec-WebSocket-Protocol", connect.TransportDelayH1Subprotocol)
	}
	if provideIntent {
		r.Header.Set(HeaderProvideIntent, "1")
	}
	return connectH1TransportDelayResponseHeader(handler.acceptTransportDelay(), true, r) != nil
}

// The handler's H3 decision as connectQuicWithCarrier makes it: whether the
// auth response accepts the header.
func testTransportDelayControlH3Accepts(t *testing.T, handler *ConnectHandler, offer bool, provideIntent bool) bool {
	t.Helper()
	auth, authFrameBytes := testConnectH3DelayAuthFrame(t, false, offer, provideIntent)
	_, useTransportDelay, err := writeConnectQuicAuthResponse(
		auth,
		authFrameBytes,
		true,
		true,
		true,
		handler.acceptTransportDelay() && !provideIntent,
		func([]byte) error {
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return useTransportDelay
}

// On by default: the code switch is on, a switch reaches new connections
// within a minute, and the header is accepted before the first read, after a
// first read that fails, after a read of an absent control, and by a handler
// with no control at all.
func TestConnectTransportDelayControlDefaultOn(t *testing.T) {
	settings := DefaultConnectHandlerSettings()
	if !settings.EnableTransportDelay {
		t.Fatal("the delay header is off by default")
	}
	if settings.TransportDelayControlRefreshInterval <= 0 || settings.TransportDelayControlReadTimeout <= 0 ||
		time.Minute < settings.TransportDelayControlRefreshInterval+settings.TransportDelayControlReadTimeout {
		t.Fatalf("a switch reaches new connections within %s plus a read of %s",
			settings.TransportDelayControlRefreshInterval, settings.TransportDelayControlReadTimeout)
	}

	store := &testTransportDelayControlStore{}
	handler := testTransportDelayControlHandler(context.Background(), store)
	requireAccepted := func(when string) {
		t.Helper()
		if !handler.acceptTransportDelay() ||
			!testTransportDelayControlH1Accepts(handler, true, false) ||
			!testTransportDelayControlH3Accepts(t, handler, true, false) {
			t.Fatalf("%s: an offer is declined", when)
		}
	}
	requireAccepted("before the first read")
	store.set(nil, errors.New("synthetic redis unavailable"), false)
	handler.transportDelayControl.refresh()
	requireAccepted("after a first read that failed")
	store.set(nil, nil, false)
	handler.transportDelayControl.refresh()
	requireAccepted("after a read of an absent control")

	bare := &ConnectHandler{settings: DefaultConnectHandlerSettings()}
	if !bare.acceptTransportDelay() {
		t.Fatal("a handler with no control declines")
	}
	bare.settings.EnableTransportDelay = false
	if bare.acceptTransportDelay() {
		t.Fatal("a handler with no control accepts past the code switch")
	}
}

// The operator's switch reaches the next negotiation on H1, H1+ and H3: off
// declines every offer, deleting the field accepts again, and a value that
// does not parse is off. A client that does not offer and a connection that
// declares provide intent keep the legacy wire whatever the control says, and
// the decisions themselves never read the store.
func TestConnectTransportDelayControlSwitchesNewNegotiations(t *testing.T) {
	store := &testTransportDelayControlStore{}
	handler := testTransportDelayControlHandler(context.Background(), store)
	for _, step := range []struct {
		name     string
		fields   map[string]string
		accepted bool
	}{
		{name: "absent", accepted: true},
		{name: "off", fields: map[string]string{model.ConnectMoveControlFieldTransportDelay: "off"}},
		{name: "off beside the mover's fields", fields: map[string]string{model.ConnectMoveControlFieldTransportDelay: "off", model.ConnectMoveControlFieldMode: "on"}},
		{name: "field deleted", fields: map[string]string{model.ConnectMoveControlFieldMode: "on"}, accepted: true},
		{name: "false", fields: map[string]string{model.ConnectMoveControlFieldTransportDelay: "false"}},
		{name: "on", fields: map[string]string{model.ConnectMoveControlFieldTransportDelay: "on"}, accepted: true},
		{name: "a value that does not parse", fields: map[string]string{model.ConnectMoveControlFieldTransportDelay: "disabled"}},
		{name: "control deleted", accepted: true},
	} {
		store.set(step.fields, nil, false)
		handler.transportDelayControl.refresh()
		reads := store.readCount()
		if accepted := handler.acceptTransportDelay(); accepted != step.accepted {
			t.Fatalf("%s: accept = %t", step.name, accepted)
		}
		if accepted := testTransportDelayControlH1Accepts(handler, true, false); accepted != step.accepted {
			t.Fatalf("%s: H1 offer accepted = %t", step.name, accepted)
		}
		if accepted := testTransportDelayControlH3Accepts(t, handler, true, false); accepted != step.accepted {
			t.Fatalf("%s: H3 offer accepted = %t", step.name, accepted)
		}
		if testTransportDelayControlH1Accepts(handler, false, false) || testTransportDelayControlH3Accepts(t, handler, false, false) {
			t.Fatalf("%s: a client that does not offer got the header", step.name)
		}
		if testTransportDelayControlH1Accepts(handler, true, true) || testTransportDelayControlH3Accepts(t, handler, true, true) {
			t.Fatalf("%s: a provide intent connection got the header", step.name)
		}
		if store.readCount() != reads {
			t.Fatalf("%s: a negotiation read the control", step.name)
		}
	}

	// the control cannot widen past the code switch
	handler.settings.EnableTransportDelay = false
	store.set(map[string]string{model.ConnectMoveControlFieldTransportDelay: "on"}, nil, false)
	handler.transportDelayControl.refresh()
	if handler.acceptTransportDelay() || testTransportDelayControlH1Accepts(handler, true, false) || testTransportDelayControlH3Accepts(t, handler, true, false) {
		t.Fatal("the control turned the header on past the code switch")
	}
}

// A read that fails or raises keeps the last value, off or on, and the next
// good read applies.
func TestConnectTransportDelayControlReadFailureKeepsLast(t *testing.T) {
	store := &testTransportDelayControlStore{}
	handler := testTransportDelayControlHandler(context.Background(), store)
	off := map[string]string{model.ConnectMoveControlFieldTransportDelay: "off"}
	unavailable := errors.New("synthetic redis unavailable")
	for _, step := range []struct {
		name     string
		fields   map[string]string
		err      error
		raise    bool
		accepted bool
	}{
		{name: "off", fields: off},
		{name: "a failed read after off", err: unavailable},
		{name: "a second failed read after off", err: unavailable},
		{name: "a deadline after off", err: context.DeadlineExceeded},
		{name: "a raised read after off", raise: true},
		{name: "deleted", accepted: true},
		{name: "a failed read after on", err: unavailable, accepted: true},
		{name: "a raised read after on", raise: true, accepted: true},
		{name: "off again", fields: off},
	} {
		store.set(step.fields, step.err, step.raise)
		handler.transportDelayControl.refresh()
		if accepted := handler.acceptTransportDelay(); accepted != step.accepted {
			t.Fatalf("%s: accept = %t", step.name, accepted)
		}
		if accepted := testTransportDelayControlH1Accepts(handler, true, false); accepted != step.accepted {
			t.Fatalf("%s: H1 offer accepted = %t", step.name, accepted)
		}
		if accepted := testTransportDelayControlH3Accepts(t, handler, true, false); accepted != step.accepted {
			t.Fatalf("%s: H3 offer accepted = %t", step.name, accepted)
		}
	}
}

// The loop reads at start, not an interval later, then every interval, so a
// change reaches the handler within one; it ends with its context, and a
// non-positive interval reads once.
func TestConnectTransportDelayControlLoop(t *testing.T) {
	waitAllows := func(control *connectTransportDelayControl, allows bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for control.allows() != allows {
			if time.Now().After(deadline) {
				t.Fatalf("the loop did not apply allows=%t", allows)
			}
			time.Sleep(time.Millisecond)
		}
	}
	runLoop := func(control *connectTransportDelayControl) chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			control.run()
		}()
		return done
	}
	waitDone := func(done chan struct{}) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the loop did not end with its context")
		}
	}
	off := map[string]string{model.ConnectMoveControlFieldTransportDelay: "off"}

	t.Run("reads at start", func(t *testing.T) {
		store := &testTransportDelayControlStore{fields: off}
		settings := DefaultConnectHandlerSettings()
		settings.TransportDelayControlRefreshInterval = time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		control := newConnectTransportDelayControlWithStore(ctx, settings, store.read)
		done := runLoop(control)
		waitAllows(control, false)
		cancel()
		waitDone(done)
	})

	t.Run("reads every interval", func(t *testing.T) {
		store := &testTransportDelayControlStore{}
		settings := DefaultConnectHandlerSettings()
		settings.TransportDelayControlRefreshInterval = 10 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		control := newConnectTransportDelayControlWithStore(ctx, settings, store.read)
		done := runLoop(control)
		store.set(off, nil, false)
		waitAllows(control, false)
		store.set(nil, nil, false)
		waitAllows(control, true)
		store.set(off, nil, false)
		waitAllows(control, false)
		cancel()
		waitDone(done)
	})

	t.Run("a non-positive interval reads once", func(t *testing.T) {
		store := &testTransportDelayControlStore{fields: off}
		settings := DefaultConnectHandlerSettings()
		settings.TransportDelayControlRefreshInterval = 0
		control := newConnectTransportDelayControlWithStore(context.Background(), settings, store.read)
		waitDone(runLoop(control))
		if reads := store.readCount(); reads != 1 || control.allows() {
			t.Fatalf("reads = %d, allows = %t", reads, control.allows())
		}
	})
}

// The switch on real upgrades of both H1 carriers, in the handler's order: its
// acceptance, the upgrade, the peer conn. A connection that negotiated before
// the operator turned the header off keeps it on every later message; the
// next connection gets a 101 that selects nothing and frames byte for byte, as
// a client from before the header reads them; and once the field is deleted
// the next connection negotiates again. Hermetic: no database.
func TestConnectTransportDelayControlH1Carriers(t *testing.T) {
	const (
		carrierWebSocket = "websocket"
		carrierH1Plus    = "h1plus"
	)
	payload := []byte("transfer frame")
	for _, carrier := range []string{carrierWebSocket, carrierH1Plus} {
		t.Run(carrier, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			store := &testTransportDelayControlStore{}
			handler := testTransportDelayControlHandler(ctx, store)

			// each upgrade's server conn, in order
			serverConns := make(chan connect.H1MessageConn, 4)
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// both carriers are header auth here
				responseHeader := connectH1TransportDelayResponseHeader(handler.acceptTransportDelay(), true, r)
				var ws connect.H1MessageConn
				switch carrier {
				case carrierH1Plus:
					conn, err := connect.AcceptFramedUpgradeWithHeader(w, r, connect.H1FramerProtocol, 5*time.Second, responseHeader)
					if err != nil {
						t.Errorf("accept: %s", err)
						return
					}
					framed, err := connect.NewFramedMessageConn(conn, connect.H1FramerProtocol, 65535, nil)
					if err != nil {
						conn.Close()
						t.Errorf("framed conn: %s", err)
						return
					}
					ws = framed
				default:
					upgrader := websocket.Upgrader{HandshakeTimeout: 5 * time.Second}
					webSocket, err := upgrader.Upgrade(w, r, responseHeader)
					if err != nil {
						t.Errorf("upgrade: %s", err)
						return
					}
					ws = webSocket
				}
				if delayed, ok := connect.NewTransportDelayH1PeerConn(ws, connect.Id{}); ok {
					ws = delayed
				}
				serverConns <- ws
			}))
			defer listener.Close()
			address := "ws" + listener.URL[len("http"):] + "/"

			type connection struct {
				// a legacy reader, which strips nothing
				client      connect.H1MessageConn
				subprotocol string
				server      connect.H1MessageConn
			}
			// a client of this release, which offers the header
			dial := func() *connection {
				t.Helper()
				header := http.Header{}
				header.Add("Sec-WebSocket-Protocol", connect.TransportDelayH1Subprotocol)
				dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
				c := &connection{}
				switch carrier {
				case carrierH1Plus:
					raw, err := connect.DialFramedUpgrade(ctx, address, header, dialer, connect.H1FramerProtocol)
					if err != nil {
						t.Fatalf("dial: %s", err)
					}
					framed, err := connect.NewFramedMessageConn(raw, connect.H1FramerProtocol, 65535, nil)
					if err != nil {
						raw.Close()
						t.Fatalf("framed conn: %s", err)
					}
					c.client, c.subprotocol = framed, framed.Subprotocol()
				default:
					webSocket, response, err := dialer.DialContext(ctx, address, header)
					if response != nil && response.Body != nil {
						response.Body.Close()
					}
					if err != nil {
						t.Fatalf("dial: %s", err)
					}
					c.client, c.subprotocol = webSocket, webSocket.Subprotocol()
				}
				t.Cleanup(func() {
					c.client.Close()
				})
				select {
				case c.server = <-serverConns:
				case <-ctx.Done():
					t.Fatal("the server did not upgrade")
				}
				t.Cleanup(func() {
					c.server.Close()
				})
				return c
			}
			// one server write as the client's legacy reader sees it: the
			// header and the payload when the 101 accepted, else the payload
			requireWire := func(name string, c *connection, delayed bool) {
				t.Helper()
				if err := c.server.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				if err := c.server.WriteMessage(websocket.BinaryMessage, payload); err != nil {
					t.Fatalf("%s: write: %s", name, err)
				}
				if err := c.client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				_, message, err := c.client.ReadMessage()
				if err != nil {
					t.Fatalf("%s: read: %s", name, err)
				}
				if delayed {
					if len(message) != 12+len(payload) || !bytes.Equal(message[12:], payload) {
						t.Fatalf("%s: message of %d bytes, expected the 12-byte header and %d bytes", name, len(message), len(payload))
					}
				} else if !bytes.Equal(message, payload) {
					t.Fatalf("%s: message of %d bytes, expected exactly the %d-byte frame", name, len(message), len(payload))
				}
			}

			// on by default
			before := dial()
			if before.subprotocol != connect.TransportDelayH1Subprotocol {
				t.Fatalf("the first 101 selected %q, expected the delay header", before.subprotocol)
			}
			requireWire("before the switch", before, true)

			// the operator turns the header off
			store.set(map[string]string{model.ConnectMoveControlFieldTransportDelay: "off"}, nil, false)
			handler.transportDelayControl.refresh()
			after := dial()
			if after.subprotocol != "" {
				t.Fatalf("the 101 after the switch selected %q, expected none", after.subprotocol)
			}
			requireWire("after the switch", after, false)
			requireWire("negotiated before the switch", before, true)

			// the operator deletes the field
			store.set(nil, nil, false)
			handler.transportDelayControl.refresh()
			again := dial()
			if again.subprotocol != connect.TransportDelayH1Subprotocol {
				t.Fatalf("the 101 after the field was deleted selected %q, expected the delay header", again.subprotocol)
			}
			requireWire("after the field was deleted", again, true)
			requireWire("negotiated while off", after, false)
		})
	}
}

// The operator's commands against Redis reach the real handler, whose own
// loop reads the control: `HSET {connect_move}control transportDelay off`
// gives the next offering client of either H1 carrier a 101 that selects
// nothing, and `HDEL {connect_move}control transportDelay` accepts the one
// after.
func TestConnectTransportDelayControlInRedis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, nil, func(settings *ConnectHandlerSettings) {
			settings.EnableH1Plus = true
			settings.TransportDelayControlRefreshInterval = 20 * time.Millisecond
			settings.ConnectionTestConfig = V0TestConfig()
		})
		defer env.Close()
		address := fmt.Sprintf("ws://127.0.0.1:%d/", env.port)

		redis := func(command func(ctx context.Context, r server.RedisClient) error) {
			redisCtx, redisCancel := context.WithTimeout(ctx, 5*time.Second)
			defer redisCancel()
			if err := server.RedisWithDeadline(redisCtx, func(r server.RedisClient) error {
				return command(redisCtx, r)
			}); err != nil {
				t.Fatal(err)
			}
		}
		waitAccept := func(accept bool) {
			deadline := time.Now().Add(10 * time.Second)
			for env.handler.acceptTransportDelay() != accept {
				if time.Now().After(deadline) {
					t.Fatalf("the handler did not apply accept=%t", accept)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		// the 101's selection for a client of this release, which offers
		subprotocol := func(carrier string) string {
			_, token := env.authClient(&model.AuthNetworkClientArgs{Description: "transport delay control", DeviceSpec: "test"})
			header := http.Header{
				"Authorization":           []string{"Bearer " + token},
				"X-Ur-Instanceid":         []string{server.NewId().String()},
				"X-Ur-Transportversion":   []string{"2"},
				"X-Ur-Streamleaseversion": []string{"1"},
			}
			header.Add("Sec-WebSocket-Protocol", connect.TransportDelayH1Subprotocol)
			dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
			if carrier == "h1plus" {
				raw, err := connect.DialFramedUpgrade(ctx, address, header, dialer, connect.H1FramerProtocol)
				if err != nil {
					t.Fatalf("%s: dial: %s", carrier, err)
				}
				framed, err := connect.NewFramedMessageConn(raw, connect.H1FramerProtocol, 65535, nil)
				if err != nil {
					raw.Close()
					t.Fatalf("%s: framed conn: %s", carrier, err)
				}
				defer framed.Close()
				return framed.Subprotocol()
			}
			webSocket, response, err := dialer.DialContext(ctx, address, header)
			if response != nil && response.Body != nil {
				response.Body.Close()
			}
			if err != nil {
				t.Fatalf("%s: dial: %s", carrier, err)
			}
			defer webSocket.Close()
			return webSocket.Subprotocol()
		}
		requireSubprotocol := func(when string, want string) {
			for _, carrier := range []string{"websocket", "h1plus"} {
				if got := subprotocol(carrier); got != want {
					t.Fatalf("%s: the %s 101 selected %q, expected %q", when, carrier, got, want)
				}
			}
		}

		waitAccept(true)
		requireSubprotocol("absent", connect.TransportDelayH1Subprotocol)

		redis(func(ctx context.Context, r server.RedisClient) error {
			return r.HSet(ctx, "{connect_move}control", model.ConnectMoveControlFieldTransportDelay, "off").Err()
		})
		waitAccept(false)
		requireSubprotocol("off", "")

		redis(func(ctx context.Context, r server.RedisClient) error {
			return r.HDel(ctx, "{connect_move}control", model.ConnectMoveControlFieldTransportDelay).Err()
		})
		waitAccept(true)
		requireSubprotocol("deleted", connect.TransportDelayH1Subprotocol)
	})
}
