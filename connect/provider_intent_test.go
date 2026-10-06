// Provider intent on the connect transports: the declaration, the client limit
// exceeded signal on each carrier, the per-connection check, and the full
// handshake against a handler.
package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
)

// Only the exact value 1 declares intent.
func TestProvideIntentFromHeader(t *testing.T) {
	cases := map[string]bool{
		"1":    true,
		" 1 ":  true,
		"":     false,
		"0":    false,
		"true": false,
		"11":   false,
		"yes":  false,
	}
	for value, provideIntent := range cases {
		header := http.Header{}
		if value != "" {
			header.Set(HeaderProvideIntent, value)
		}
		if provideIntentFromHeader(header) != provideIntent {
			t.Fatalf("header %q: provide intent %t", value, !provideIntent)
		}
	}
	connect.AssertEqual(t, provideIntentFromHeader(http.Header{}), false)
}

// The H1 close message is transport control 3 with close reason 1, which no
// other server path sends: payload messages are longer than 16 bytes and the
// other 5 byte controls are the speed test controls 1 and 2.
func TestClientLimitExceededControlMessage(t *testing.T) {
	message := clientLimitExceededControlMessage()
	connect.AssertEqual(t, message, []byte{3, 0, 0, 0, 1})
	if transportControlClose == connect.TransportControlSpeedStart || transportControlClose == connect.TransportControlSpeedStop {
		t.Fatal("the close control collides with a speed test control")
	}
}

// Reads the server's next message, failing on anything but a 5 byte binary
// client limit exceeded control.
func testingReadClientLimitExceededControl(t testing.TB, ws connect.H1MessageConn) {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	messageType, message, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read the close control: %s", err)
	}
	connect.AssertEqual(t, messageType, websocket.BinaryMessage)
	connect.AssertEqual(t, message, []byte{3, 0, 0, 0, 1})
}

// Requires the WebSocket close frame of the client limit exceeded close.
func testingRequireClientLimitExceededClose(t testing.TB, ws *websocket.Conn) {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, _, err := ws.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("expected a close frame, got %v", err)
	}
	connect.AssertEqual(t, closeErr.Code, 4001)
	connect.AssertEqual(t, closeErr.Text, "client limit exceeded")
}

// On WebSocket the signal is the control message, then a close frame 4001.
func TestConnectH1ClientLimitExceededWebSocketSignal(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		if err := writeConnectH1ClientLimitExceeded(ws, 5*time.Second); err != nil {
			t.Errorf("write the signal: %s", err)
		}
	}))
	defer httpServer.Close()

	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	testingReadClientLimitExceededControl(t, ws)
	testingRequireClientLimitExceededClose(t, ws)
}

// On the H1+ framed carrier, which has no control frames, the signal is the
// control message followed by the end of the stream.
func TestConnectH1ClientLimitExceededFramedSignal(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	serverFramed, err := connect.NewFramedMessageConn(serverConn, connect.H1FramerProtocol, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	clientFramed, err := connect.NewFramedMessageConn(clientConn, connect.H1FramerProtocol, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeResult := make(chan error, 1)
	go func() {
		defer serverFramed.Close()
		writeResult <- writeConnectH1ClientLimitExceeded(serverFramed, 5*time.Second)
	}()
	testingReadClientLimitExceededControl(t, clientFramed)
	if err := <-writeResult; err != nil {
		t.Fatalf("write the signal: %s", err)
	}
	clientFramed.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, _, err := clientFramed.ReadMessage(); err == nil {
		t.Fatal("the framed stream stayed open after the signal")
	}
}

// Requires a QUIC connection closed by the peer with the client limit
// exceeded application error.
func testingRequireQuicClientLimitExceeded(t testing.TB, conn *quic.Conn) {
	t.Helper()
	select {
	case <-conn.Context().Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the QUIC connection was not closed")
	}
	var applicationErr *quic.ApplicationError
	if !errors.As(context.Cause(conn.Context()), &applicationErr) {
		t.Fatalf("expected an application close, got %v", context.Cause(conn.Context()))
	}
	connect.AssertEqual(t, applicationErr.Remote, true)
	connect.AssertEqual(t, applicationErr.ErrorCode, quic.ApplicationErrorCode(4001))
	connect.AssertEqual(t, applicationErr.ErrorMessage, "client limit exceeded")
}

// On H3 the signal is the QUIC application close 4001.
func TestConnectQuicClientLimitExceededSignal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serverTlsConfig, clientTlsConfig, err := newConnectQuicWindowTlsConfigs()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", serverTlsConfig, &quic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept(ctx)
		if err != nil {
			return
		}
		closeConnectQuicClientLimitExceeded(conn)
	}()

	conn, err := quic.DialAddr(ctx, listener.Addr().String(), clientTlsConfig, &quic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	testingRequireQuicClientLimitExceeded(t, conn)
}

// A scripted provider intent store with a manual clock.
type testingProviderIntentStore struct {
	stateLock sync.Mutex

	connectResult model.ProviderIntentConnectResult
	connectPanic  any
	connectCount  int
	observeState  *model.ProviderIntentState
	observePanic  any
	scheduleTimes []time.Time
	enforcedValue bool
	nowValue      time.Time
}

// Returns the scripted result, or panics with the scripted error.
func (self *testingProviderIntentStore) connect(ctx context.Context, networkId server.Id, clientId server.Id, presenceTimeout time.Duration) model.ProviderIntentConnectResult {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.connectCount += 1
	if self.connectPanic != nil {
		panic(self.connectPanic)
	}
	return self.connectResult
}

// Returns the scripted state, or panics with the scripted error.
func (self *testingProviderIntentStore) observe(ctx context.Context, networkId server.Id, clientId server.Id, presenceTimeout time.Duration) *model.ProviderIntentState {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.observePanic != nil {
		panic(self.observePanic)
	}
	return self.observeState
}

// Records the schedule.
func (self *testingProviderIntentStore) scheduleCheck(ctx context.Context, networkId server.Id, clientId server.Id, runAt time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.scheduleTimes = append(self.scheduleTimes, runAt)
}

// The scripted enforcement.
func (self *testingProviderIntentStore) enforced() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.enforcedValue
}

// The manual clock.
func (self *testingProviderIntentStore) now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.nowValue
}

// A check of a synthetic client on a scripted store.
func newTestingProviderIntentCheck(ctx context.Context, store *testingProviderIntentStore) *providerIntentCheck {
	return newProviderIntentCheckWithStore(
		ctx,
		server.NewId(),
		server.NewId(),
		connectTransportH1,
		DefaultProviderIntentCheckSettings(),
		store,
	)
}

// An over limit client is kicked only while enforcement is on. In shadow mode
// the decision is counted once per decision and the connection keeps serving.
func TestProviderIntentCheckKicksOverLimitOnlyWhenEnforced(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	overLimit := &model.ProviderIntentState{
		Status:    model.ProviderIntentStatusOverLimit,
		CheckTime: now.Add(time.Hour),
	}
	normal := &model.ProviderIntentState{
		Status:    model.ProviderIntentStatusNormal,
		CheckTime: now.Add(time.Hour),
	}

	enforcedStore := &testingProviderIntentStore{
		connectResult: model.ProviderIntentConnectResult{State: overLimit},
		enforcedValue: true,
		nowValue:      now,
	}
	connect.AssertEqual(t, newTestingProviderIntentCheck(t.Context(), enforcedStore).Start(), false)

	shadow := clientLimitKicksCounter.WithLabelValues(connectTransportH1, clientLimitKickCauseProviderIntent, "shadow")
	shadowBefore := testutil.ToFloat64(shadow)
	shadowStore := &testingProviderIntentStore{
		connectResult: model.ProviderIntentConnectResult{State: overLimit},
		observeState:  overLimit,
		nowValue:      now,
	}
	check := newTestingProviderIntentCheck(t.Context(), shadowStore)
	connect.AssertEqual(t, check.Start(), true)
	connect.AssertEqual(t, testutil.ToFloat64(shadow), shadowBefore+1)
	// the same decision observed again is not counted again
	connect.AssertEqual(t, check.observe(), true)
	connect.AssertEqual(t, testutil.ToFloat64(shadow), shadowBefore+1)
	// a new decision is counted again
	shadowStore.observeState = normal
	connect.AssertEqual(t, check.observe(), true)
	shadowStore.observeState = overLimit
	connect.AssertEqual(t, check.observe(), true)
	connect.AssertEqual(t, testutil.ToFloat64(shadow), shadowBefore+2)

	// turning enforcement on kicks at the next observation
	shadowStore.enforcedValue = true
	connect.AssertEqual(t, check.observe(), false)

	// the loop returns false on the kick, without waiting for a context
	runStore := &testingProviderIntentStore{
		observeState:  overLimit,
		enforcedValue: true,
		nowValue:      now,
	}
	runCheck := newTestingProviderIntentCheck(t.Context(), runStore)
	runCheck.settings = &ProviderIntentCheckSettings{
		ObserveInterval:     time.Nanosecond,
		PresenceTimeout:     time.Minute,
		OverdueCheckTimeout: time.Minute,
		MinScheduleInterval: time.Minute,
	}
	connect.AssertEqual(t, runCheck.Run(), false)

	// a closed connection ends the loop
	closedCtx, closedCancel := context.WithCancel(t.Context())
	closedCancel()
	closedCheck := newTestingProviderIntentCheck(closedCtx, &testingProviderIntentStore{nowValue: now})
	connect.AssertEqual(t, closedCheck.Run(), true)
}

// A connection starts the check chain when its connect asks for it, and
// restarts a chain whose check is overdue (it stopped while the client was
// away), at most once per interval.
func TestProviderIntentCheckRestartsStoppedChain(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	settings := DefaultProviderIntentCheckSettings()
	store := &testingProviderIntentStore{
		connectResult: model.ProviderIntentConnectResult{
			State: &model.ProviderIntentState{
				Status:    model.ProviderIntentStatusPending,
				CheckTime: now,
			},
			ScheduleCheck: true,
		},
		nowValue: now,
	}
	check := newTestingProviderIntentCheck(t.Context(), store)
	connect.AssertEqual(t, check.Start(), true)
	check.scheduleCheckIfNeeded()
	connect.AssertEqual(t, store.scheduleTimes, []time.Time{now})
	// nothing more to start
	check.scheduleCheckIfNeeded()
	connect.AssertEqual(t, len(store.scheduleTimes), 1)

	// a check due just now is not yet a stopped chain
	store.nowValue = now.Add(settings.OverdueCheckTimeout - time.Second)
	store.observeState = &model.ProviderIntentState{
		Status:    model.ProviderIntentStatusQualified,
		CheckTime: now,
	}
	connect.AssertEqual(t, check.observe(), true)
	check.scheduleCheckIfNeeded()
	connect.AssertEqual(t, len(store.scheduleTimes), 1)

	// an overdue check restarts the chain, once per interval
	store.nowValue = now.Add(settings.OverdueCheckTimeout)
	connect.AssertEqual(t, check.observe(), true)
	check.scheduleCheckIfNeeded()
	connect.AssertEqual(t, len(store.scheduleTimes), 1)
	store.nowValue = now.Add(settings.MinScheduleInterval)
	connect.AssertEqual(t, check.observe(), true)
	check.scheduleCheckIfNeeded()
	connect.AssertEqual(t, store.scheduleTimes, []time.Time{now, now.Add(settings.MinScheduleInterval)})

	// a record that expired while connected starts again
	connectCount := store.connectCount
	store.observeState = nil
	connect.AssertEqual(t, check.observe(), true)
	connect.AssertEqual(t, store.connectCount, connectCount+1)
}

// Unset settings fall back to the defaults: a zero observe interval would spin
// the observation loop.
func TestProviderIntentCheckDefaultsUnsetSettings(t *testing.T) {
	for _, settings := range []*ProviderIntentCheckSettings{nil, {}} {
		check := newProviderIntentCheckWithStore(t.Context(), server.NewId(), server.NewId(), connectTransportH1, settings, &testingProviderIntentStore{})
		connect.AssertEqual(t, *check.settings, *DefaultProviderIntentCheckSettings())
	}
}

// A store error fails open: the connection keeps serving and the error is
// counted. A closed connection is not an error.
func TestProviderIntentCheckFailsOpenOnStoreError(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	errorsCounter := provideIntentCheckErrorsCounter.WithLabelValues(connectTransportH1)
	errorsBefore := testutil.ToFloat64(errorsCounter)

	store := &testingProviderIntentStore{
		connectPanic:  errors.New("synthetic store outage"),
		observePanic:  errors.New("synthetic store outage"),
		enforcedValue: true,
		nowValue:      now,
	}
	check := newTestingProviderIntentCheck(t.Context(), store)
	connect.AssertEqual(t, check.Start(), true)
	connect.AssertEqual(t, check.observe(), true)
	connect.AssertEqual(t, testutil.ToFloat64(errorsCounter), errorsBefore+2)

	store.connectPanic = context.Canceled
	connect.AssertEqual(t, check.Start(), true)
	connect.AssertEqual(t, testutil.ToFloat64(errorsCounter), errorsBefore+2)
}

// A connect handler with an exchange, serving H1 on loopback TCP and H3 on
// loopback UDP, and a network to connect clients of.
type providerIntentTestServer struct {
	ctx         context.Context
	h1Address   string
	h3Address   string
	networkId   server.Id
	userId      server.Id
	networkName string
}

// Serves until the test ends.
func newProviderIntentTestServer(t testing.TB, ctx context.Context) *providerIntentTestServer {
	exchangeSettings := DefaultExchangeSettings()
	exchangeSettings.KeyEventDelivery.Enabled = false
	exchange := NewExchange(
		ctx,
		"host0",
		"test",
		"test",
		map[int]int{},
		map[string]string{"host0": "127.0.0.1"},
		exchangeSettings,
	)
	t.Cleanup(exchange.Close)

	h3PacketConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handlerId := model.CreateNetworkClientHandler(ctx)
	settings := DefaultConnectHandlerSettings()
	settings.ConnectionAnnounceTimeout = 0
	settings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
	settings.ListenH3Port = 0
	settings.ListenDnsPort = 0
	settings.EnableProxyProtocol = false
	settings.TransportTlsSettings.EnableSelfSign = true
	settings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
	handler := NewConnectHandlerWithPacketConns(ctx, handlerId, exchange, settings, ConnectHandlerPacketConns{
		H3: h3PacketConn,
	})
	t.Cleanup(handler.Close)

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{
		Handler: http.HandlerFunc(handler.Connect),
	}
	go httpServer.Serve(listener)
	t.Cleanup(func() {
		httpServer.Close()
	})

	testServer := &providerIntentTestServer{
		ctx:         ctx,
		h1Address:   listener.Addr().String(),
		h3Address:   h3PacketConn.LocalAddr().String(),
		networkId:   server.NewId(),
		userId:      server.NewId(),
		networkName: "testProvideIntent",
	}
	model.Testing_CreateNetwork(ctx, testServer.networkId, testServer.networkName, testServer.userId)
	model.Testing_ClearNetworkPeersEnabledCache()
	return testServer
}

// Creates a top-level client of the network and its signed client jwt.
func (self *providerIntentTestServer) createClient() (clientId server.Id, byJwt string) {
	clientId = server.NewId()
	deviceId := server.NewId()
	model.Testing_CreateDevice(self.ctx, self.networkId, deviceId, clientId, "synthetic", "synthetic")
	byJwt = jwt.NewByJwt(self.networkId, self.userId, self.networkName, false, false).Client(deviceId, clientId).Sign()
	return
}

// Registers a connected ordinary client, which takes a normal slot.
func (self *providerIntentTestServer) connectOrdinaryClient() {
	clientId, _ := self.createClient()
	model.AddNetworkPeer(self.ctx, self.networkId, &model.NetworkPeer{ClientId: clientId}, server.NewId(), time.Hour)
}

// Dials H1 WebSocket with v2 header auth.
func (self *providerIntentTestServer) dialH1(t testing.TB, byJwt string, provideIntent bool) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+byJwt)
	header.Set("X-UR-AppVersion", "0.0.0")
	header.Set("X-UR-InstanceId", server.NewId().String())
	header.Set("X-UR-TransportVersion", "2")
	if provideIntent {
		header.Set(HeaderProvideIntent, "1")
	}
	ws, _, err := websocket.DefaultDialer.DialContext(self.ctx, "ws://"+self.h1Address+"/", header)
	if err != nil {
		t.Fatalf("dial h1: %s", err)
	}
	t.Cleanup(func() {
		ws.Close()
	})
	return ws
}

// An authenticated H3 connection and its stream.
type providerIntentTestH3 struct {
	conn   *quic.Conn
	stream *quic.Stream
	framer *connect.Framer
}

// Dials H3 and authenticates with an Auth frame.
func (self *providerIntentTestServer) dialH3(t testing.TB, byJwt string, provideIntent bool) *providerIntentTestH3 {
	t.Helper()
	conn, err := quic.DialAddr(
		self.ctx,
		self.h3Address,
		// like the platform transport: no ALPN
		&tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "127.0.0.1",
			MinVersion:         tls.VersionTLS13,
		},
		&quic.Config{},
	)
	if err != nil {
		t.Fatalf("dial h3: %s", err)
	}
	t.Cleanup(func() {
		conn.CloseWithError(0, "")
	})
	stream, err := conn.OpenStreamSync(self.ctx)
	if err != nil {
		t.Fatalf("open h3 stream: %s", err)
	}
	authBytes, err := connect.EncodeFrame(&protocol.Auth{
		ByJwt:         byJwt,
		AppVersion:    "0.0.0",
		InstanceId:    server.NewId().Bytes(),
		ProvideIntent: provideIntent,
	}, connect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	framer := connect.NewFramer(connect.DefaultFramerSettings(int(connect.DefaultClientSettings().MinimumMessageLenLimit())))
	stream.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := framer.Write(stream, authBytes); err != nil {
		t.Fatalf("write h3 auth: %s", err)
	}
	stream.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := framer.Read(stream); err != nil {
		t.Fatalf("read h3 auth: %s", err)
	}
	return &providerIntentTestH3{
		conn:   conn,
		stream: stream,
		framer: framer,
	}
}

// An intent client that failed qualification finds no normal slot: with
// enforcement on, its H1 connection gets the client limit exceeded signal.
func TestConnectProvideIntentOverLimitKicksH1(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer model.Testing_SetEnforceConcurrentClients(true)()
		defer model.Testing_SetConcurrentClientsLimit(1, 1)()
		testServer := newProviderIntentTestServer(t, ctx)

		testServer.connectOrdinaryClient()
		clientId, byJwt := testServer.createClient()
		now := server.NowUtc()
		model.Testing_SetProviderIntentState(ctx, testServer.networkId, clientId, &model.ProviderIntentState{
			Status:      model.ProviderIntentStatusNormal,
			AttemptTime: now,
			CheckTime:   now.Add(model.ProviderIntentAttemptAllowance),
		}, 0)

		kicks := clientLimitKicksCounter.WithLabelValues(connectTransportH1, clientLimitKickCauseProviderIntent, "enforced")
		kicksBefore := testutil.ToFloat64(kicks)
		ws := testServer.dialH1(t, byJwt, true)
		testingReadClientLimitExceededControl(t, ws)
		testingRequireClientLimitExceededClose(t, ws)
		connect.AssertEqual(t, testutil.ToFloat64(kicks), kicksBefore+1)
		connect.AssertEqual(t, model.GetProviderIntentState(ctx, testServer.networkId, clientId).Status, model.ProviderIntentStatusOverLimit)
	})
}

// The same client over H3 gets the QUIC application close 4001.
func TestConnectProvideIntentOverLimitKicksH3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer model.Testing_SetEnforceConcurrentClients(true)()
		defer model.Testing_SetConcurrentClientsLimit(1, 1)()
		testServer := newProviderIntentTestServer(t, ctx)

		testServer.connectOrdinaryClient()
		clientId, byJwt := testServer.createClient()
		now := server.NowUtc()
		model.Testing_SetProviderIntentState(ctx, testServer.networkId, clientId, &model.ProviderIntentState{
			Status:      model.ProviderIntentStatusNormal,
			AttemptTime: now,
			CheckTime:   now.Add(model.ProviderIntentAttemptAllowance),
		}, 0)

		kicks := clientLimitKicksCounter.WithLabelValues(connectTransportH3, clientLimitKickCauseProviderIntent, "enforced")
		kicksBefore := testutil.ToFloat64(kicks)
		h3 := testServer.dialH3(t, byJwt, true)
		testingRequireQuicClientLimitExceeded(t, h3.conn)
		connect.AssertEqual(t, testutil.ToFloat64(kicks), kicksBefore+1)
	})
}

// Shadow mode: with enforcement dark the same over limit client keeps its
// connection, and the decision is counted.
func TestConnectProvideIntentShadowServesOverLimitH1(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer model.Testing_SetEnforceConcurrentClients(false)()
		defer model.Testing_SetConcurrentClientsLimit(1, 1)()
		testServer := newProviderIntentTestServer(t, ctx)

		testServer.connectOrdinaryClient()
		clientId, byJwt := testServer.createClient()
		now := server.NowUtc()
		model.Testing_SetProviderIntentState(ctx, testServer.networkId, clientId, &model.ProviderIntentState{
			Status:      model.ProviderIntentStatusNormal,
			AttemptTime: now,
			CheckTime:   now.Add(model.ProviderIntentAttemptAllowance),
		}, 0)

		shadow := clientLimitKicksCounter.WithLabelValues(connectTransportH1, clientLimitKickCauseProviderIntent, "shadow")
		shadowBefore := testutil.ToFloat64(shadow)
		ws := testServer.dialH1(t, byJwt, true)
		// the writer starts after the check decided; its first message is a
		// heartbeat or a test, never the close
		ws.SetReadDeadline(time.Now().Add(30 * time.Second))
		messageType, message, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("the connection closed in shadow mode: %s", err)
		}
		connect.AssertEqual(t, messageType, websocket.BinaryMessage)
		if len(message) == 5 && message[0] == transportControlClose {
			t.Fatal("shadow mode sent the close")
		}
		connect.AssertEqual(t, testutil.ToFloat64(shadow), shadowBefore+1)
		connect.AssertEqual(t, model.GetProviderIntentState(ctx, testServer.networkId, clientId).Status, model.ProviderIntentStatusOverLimit)
	})
}

// A connection with the header starts the client's qualification; without it,
// nothing is recorded for the client.
func TestConnectProvideIntentHeaderStartsQualification(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		testServer := newProviderIntentTestServer(t, ctx)

		for _, provideIntent := range []bool{true, false} {
			clientId, byJwt := testServer.createClient()
			ws := testServer.dialH1(t, byJwt, provideIntent)
			ws.SetReadDeadline(time.Now().Add(30 * time.Second))
			if _, _, err := ws.ReadMessage(); err != nil {
				t.Fatalf("intent=%t: %s", provideIntent, err)
			}
			state := model.GetProviderIntentState(ctx, testServer.networkId, clientId)
			if provideIntent {
				if state == nil || state.Status != model.ProviderIntentStatusPending {
					t.Fatalf("the header did not start a qualification: %+v", state)
				}
			} else if state != nil {
				t.Fatalf("a connection without intent recorded a state: %+v", state)
			}
		}

		// the frame field declares intent on H3. The writer starts after the
		// check ran, so its first frame orders the read after it.
		clientId, byJwt := testServer.createClient()
		h3 := testServer.dialH3(t, byJwt, true)
		h3.stream.SetReadDeadline(time.Now().Add(30 * time.Second))
		if _, err := h3.framer.Read(h3.stream); err != nil {
			t.Fatalf("h3 first frame: %s", err)
		}
		state := model.GetProviderIntentState(ctx, testServer.networkId, clientId)
		if state == nil || state.Status != model.ProviderIntentStatusPending {
			t.Fatalf("the frame field did not start a qualification: %+v", state)
		}
	})
}

// A connection without intent whose nomination is refused for the concurrent
// client limit gets the client limit exceeded signal instead of a connection
// that never becomes active.
func TestConnectConcurrentLimitRefusalKicksH1(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer model.Testing_SetEnforceConcurrentClients(true)()
		defer model.Testing_SetConcurrentClientsLimit(1, 1)()
		testServer := newProviderIntentTestServer(t, ctx)

		testServer.connectOrdinaryClient()
		// a public provider without declared intent is an ordinary client
		_, byJwt := testServer.createClient()

		kicks := clientLimitKicksCounter.WithLabelValues(connectTransportH1, clientLimitKickCauseConcurrentClientLimit, "enforced")
		kicksBefore := testutil.ToFloat64(kicks)
		ws := testServer.dialH1(t, byJwt, false)
		testingReadClientLimitExceededControl(t, ws)
		testingRequireClientLimitExceededClose(t, ws)
		connect.AssertEqual(t, testutil.ToFloat64(kicks), kicksBefore+1)
	})
}

// A provider install (a top-level client created with provide intent) and a
// regular client of the same network connect through a real exchange: the
// install is counted toward the network's connected total but never appears as
// a network peer. The category is the client's, so the install is hidden even
// on a connection that does not declare intent.
func TestExchangeProviderInstallHidden(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		env := testing_newPeerDiscoveryEnv(ctx, t)
		defer env.Close()

		clientIdRegular, byClientJwtRegular := env.authClient(&model.AuthNetworkClientArgs{
			Description: "regular device",
			DeviceSpec:  "regular spec",
		})
		clientIdInstall, byClientJwtInstall := env.authClient(&model.AuthNetworkClientArgs{
			Description:   "provider install",
			DeviceSpec:    "install spec",
			ProvideIntent: true,
		})

		clientRegular := env.newClient(clientIdRegular)
		defer clientRegular.Close()
		clientInstall := env.newClient(clientIdInstall)
		defer clientInstall.Close()
		transportRegular := env.newTransport(byClientJwtRegular, server.NewId(), clientRegular.RouteManager())
		defer transportRegular.Close()
		transportInstall := env.newTransport(byClientJwtInstall, server.NewId(), clientInstall.RouteManager())
		defer transportInstall.Close()

		// both registered: the regular client as a peer, the install apart
		endTime := time.Now().Add(60 * time.Second)
		for model.GetNetworkConnectedCount(ctx, env.networkId) != 2 {
			if endTime.Before(time.Now()) {
				t.Fatalf("connected count = %d, want 2", model.GetNetworkConnectedCount(ctx, env.networkId))
			}
			select {
			case <-ctx.Done():
				t.Fatal("canceled")
			case <-time.After(100 * time.Millisecond):
			}
		}

		peersResult, err := model.GetNetworkPeersForSession(env.userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, peersResult.Error, nil)
		connect.AssertEqual(t, len(peersResult.Peers), 1)
		connect.AssertEqual(t, peersResult.Peers[0].ClientId, clientIdRegular)
	})
}

// The resident heartbeat keeps a provider install registered apart from the
// peers: counted toward the connected total, never in the peer list, even for
// a client whose profile would make a peer.
func TestResidentHeartbeatRegistersProviderInstallApart(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultExchangeSettingsWithBufferSize(4)
		settings.EnableNetworkPeers = true
		resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
		resident.residentId, resident.instanceId = server.NewId(), server.NewId()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "testProviderInstallHeartbeat", server.NewId())
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), resident.clientId, "install", "synthetic")
		resident.peerNetworkId = &networkId
		resident.peerCategory = model.NetworkPeerCategoryProvider
		send, receive, detach, err := resident.AddTransport()
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			detach()
			cancel()
			if err := resident.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			returnReadyPooledMessages(send)
			returnReadyPooledMessages(receive)
		}()
		nominee := &model.NetworkClientResident{
			ClientId:        resident.clientId,
			InstanceId:      resident.instanceId,
			ResidentId:      resident.residentId,
			ResidentHost:    "127.0.0.1",
			ResidentService: "connect",
			ResidentBlock:   "provider-install-heartbeat",
		}
		if !model.NominateResident(ctx, nil, nominee, settings.ExchangeResidentTtl) {
			t.Fatal("resident nomination failed")
		}

		if !resident.exchange.refreshResidentRegistration(resident) {
			t.Fatal("the heartbeat dropped a current resident")
		}
		connect.AssertEqual(t, model.GetNetworkConnectedCount(ctx, networkId), 1)
		_, peers := model.GetNetworkPeers(ctx, networkId)
		connect.AssertEqual(t, len(peers), 0)
	})
}
