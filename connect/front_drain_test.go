package connect

// The drain of a block that fronts its interfaces (D12 as amended), with
// alt's shutdown regressions (alt/alt_close_test.go) carried over.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	http3qlog "github.com/quic-go/quic-go/http3/qlog"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
	"github.com/quic-go/quic-go/quicvarint"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/nginx"
	"github.com/urnetwork/server/router"
)

// Ends a test front's drain as its deadline would.
func endTestFrontDrain(frontServer *frontServer) {
	drainCancel := func() context.CancelFunc {
		frontServer.stateLock.Lock()
		defer frontServer.stateLock.Unlock()
		return frontServer.drainCancel
	}()
	drainCancel()
}

// The sockets of protocol that are up.
func countTestFrontSocketsUp(frontServer *frontServer, protocol string) int {
	frontServer.stateLock.Lock()
	defer frontServer.stateLock.Unlock()
	count := 0
	for _, socket := range frontServer.sockets {
		if socket.key.protocol == protocol && socket.up {
			count += 1
		}
	}
	return count
}

// A trace on every server connection: it counts each GOAWAY the http3
// server creates, without ever blocking the recording loop, and, when
// holding, holds each connection inside its close window. quic-go records a
// connection's close after it closed the connection's streams and before it
// cancels the connection's context, so while the record is held every accept
// on the connection fails at once and its context is still live: the window
// in which upstream http3's graceful shutdown spins. In production the window
// stays open while Transport.close holds the transport mutex over its destroy
// fan-out and while every core is busy.
type testFrontConnectionTrace struct {
	// every GOAWAY created: one per connection the drain shut down, and one
	// per pass of an accept loop spinning in its graceful branch
	goaways *connect.MonitorValue[int]
	holding bool
	// the connections held in their close window so far
	held        *connect.MonitorValue[int]
	release     chan struct{}
	releaseOnce sync.Once
}

// A trace that holds closing connections only when holding.
func newTestFrontConnectionTrace(holding bool) *testFrontConnectionTrace {
	return &testFrontConnectionTrace{
		goaways: connect.NewMonitorValue(0),
		holding: holding,
		held:    connect.NewMonitorValue(0),
		release: make(chan struct{}),
	}
}

// quic.Config.Tracer
func (self *testFrontConnectionTrace) tracer(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
	return self
}

// qlogwriter.Trace
func (self *testFrontConnectionTrace) AddProducer() qlogwriter.Recorder {
	return self
}

// http3 records its frames here; the connection records whatever it does
func (self *testFrontConnectionTrace) SupportsSchemas(schema string) bool {
	return schema == http3qlog.EventSchema
}

// qlogwriter.Recorder: counts GOAWAYs, and holds a closing connection while
// holding.
func (self *testFrontConnectionTrace) RecordEvent(event qlogwriter.Event) {
	switch event := event.(type) {
	case http3qlog.FrameCreated:
		if _, ok := event.Frame.Frame.(http3qlog.GoAwayFrame); ok {
			self.goaways.Update(func(goaways int) int {
				return goaways + 1
			})
		}
	case qlog.ConnectionClosed:
		if self.holding {
			self.held.Update(func(held int) int {
				return held + 1
			})
			<-self.release
		}
	}
}

// qlogwriter.Recorder
func (self *testFrontConnectionTrace) Close() error {
	return nil
}

// Lets every held connection finish closing.
func (self *testFrontConnectionTrace) Release() {
	self.releaseOnce.Do(func() {
		close(self.release)
	})
}

// Waits until count GOAWAYs were created.
func (self *testFrontConnectionTrace) waitGoaways(t testing.TB, count int) {
	t.Helper()
	deadline := time.After(testFrontProgressTimeout)
	for {
		goaways, changed := self.goaways.Get()
		if count <= goaways {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("%d of %d GOAWAYs created", goaways, count)
		}
	}
}

// Waits until count connections are held in their close window.
func (self *testFrontConnectionTrace) waitHeld(t testing.TB, count int) {
	t.Helper()
	deadline := time.After(testFrontProgressTimeout)
	for {
		held, changed := self.held.Get()
		if count <= held {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("%d of %d connections reached their close window", held, count)
		}
	}
}

// Waits until the front serves no api connection while the trace holds them
// in their close window, failing as soon as a GOAWAY past goaways is created:
// each api connection's h3 server must finish serving it while quic-go is
// still closing it, with no second GOAWAY.
func (self *testFrontConnectionTrace) waitServedWithoutAnotherGoaway(t testing.TB, frontServer *frontServer, goaways int) {
	t.Helper()
	deadline := time.After(testFrontProgressTimeout)
	for {
		openCount, openChanged := frontServer.openCount.Get()
		created, goawaysChanged := self.goaways.Get()
		if goaways < created {
			t.Fatalf("%d GOAWAYs for %d api connections while they closed: an h3 accept loop spun in its graceful branch", created, goaways)
		}
		if openCount == 0 {
			return
		}
		select {
		case <-openChanged:
		case <-goawaysChanged:
		case <-deadline:
			t.Fatalf("%d api connections still served while their connections closed", openCount)
		}
	}
}

// Waits until the front's open count is want.
func (self *testFrontEnv) waitOpen(want int) {
	self.t.Helper()
	deadline := time.After(testFrontProgressTimeout)
	for {
		openCount, changed := self.frontServer.openCount.Get()
		if openCount == want {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			self.t.Fatalf("%d connections open, want %d", openCount, want)
		}
	}
}

// drainConnect runs D12's order: the latch reads draining before the front
// begins its drain; the exchange drains alongside the front, starting while
// the front still waits for a hijacked connection, and finds the front
// draining with its transports live; only once both are drained does the
// forced close run; cancel is last.
func TestFrontDrainRunsInD12Order(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.front.httpsHandler = testFrontEchoHandler
	})
	hijacked := env.dialHijacked(testFrontConnectName)
	hijacked.echo("before")
	udpSocketCount := 0
	for _, socket := range env.frontServer.sockets {
		if socket.key.protocol == frontProtocolUdp {
			udpSocketCount += 1
		}
	}
	warpStatus := &router.WarpStatusState{}
	warpStatus.SetReady()

	exchangeEntered := make(chan struct{})
	exchangeRelease := make(chan struct{})
	var exchangeEntryStatus string
	var exchangeEntryEvents []string
	var exchangeEntryUdpUp int
	exchangeDrain := func() {
		exchangeEntryStatus = warpStatus.Status()
		exchangeEntryEvents = env.front.recordedEvents()
		exchangeEntryUdpUp = countTestFrontSocketsUp(env.frontServer, frontProtocolUdp)
		close(exchangeEntered)
		<-exchangeRelease
		env.front.record("exchange drained")
	}
	canceled := make(chan struct{})
	cancel := func() {
		env.front.record("cancel")
		close(canceled)
	}
	go drainConnect(warpStatus, env.frontServer, exchangeDrain, cancel)

	testFrontWait(t, exchangeEntered, "the exchange drain")
	if exchangeEntryStatus != "draining" {
		t.Fatalf("the exchange drained under status %q", exchangeEntryStatus)
	}
	if !slices.Equal(exchangeEntryEvents, []string{"front begin drain"}) {
		t.Fatalf("at the exchange drain the front saw %v, want its drain begun and nothing closed", exchangeEntryEvents)
	}
	if exchangeEntryUdpUp != udpSocketCount {
		t.Fatalf("%d of %d QUIC sockets up during the exchange drain", exchangeEntryUdpUp, udpSocketCount)
	}
	// the front still waits for the hijacked connection, which still serves
	hijacked.echo("during the exchange drain")
	hijacked.tlsConn.Close()
	select {
	case <-canceled:
		t.Fatal("canceled while the exchange drained")
	default:
	}
	if events := env.front.recordedEvents(); slices.Contains(events, "front close") {
		t.Fatalf("the front closed while the exchange drained: %v", events)
	}

	close(exchangeRelease)
	testFrontWait(t, canceled, "the cancel")
	want := []string{"front begin drain", "exchange drained", "front close", "cancel"}
	if events := env.front.recordedEvents(); !slices.Equal(events, want) {
		t.Fatalf("drain events %v, want %v", events, want)
	}
	if up := countTestFrontSocketsUp(env.frontServer, frontProtocolUdp); up != 0 {
		t.Fatalf("%d QUIC sockets up after the forced close", up)
	}
}

// Expiry cuts the public sockets and cancels serving even when a resident
// still needs cancellation to finish its drain.
func TestFrontDrainDeadlineEndsServingWhileExchangeDrains(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.front.httpsHandler = testFrontEchoHandler
	})
	hijacked := env.dialHijacked(testFrontConnectName)
	entered, release := make(chan struct{}), make(chan struct{})
	canceled, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		drainConnect(&router.WarpStatusState{}, env.frontServer, func() {
			close(entered)
			<-release
		}, func() { close(canceled) })
	}()
	defer func() {
		close(release)
		testFrontWait(t, done, "resident drain exit")
	}()
	testFrontWait(t, entered, "resident drain start")
	hijacked.echo("during")
	endTestFrontDrain(env.frontServer)
	testFrontWait(t, canceled, "serving cancellation at the front deadline")
	testFrontReadEnd(t, hijacked.reader, "public connection at the front deadline")
}

// Without a front, connect drains as before: draining, the exchange drain,
// then cancel.
func TestFrontDrainWithoutAFrontKeepsConnectsOrder(t *testing.T) {
	warpStatus := &router.WarpStatusState{}
	warpStatus.SetReady()
	events := []string{}
	drainConnect(warpStatus, nil, func() {
		events = append(events, "exchange drain "+warpStatus.Status())
	}, func() {
		events = append(events, "cancel")
	})
	if want := []string{"exchange drain draining", "cancel"}; !slices.Equal(events, want) {
		t.Fatalf("drain events %v, want %v", events, want)
	}
}

// On h3 the drain starts while the transport can send: an api connection
// with a request in flight gets one GOAWAY, a new stream on it is refused
// with H3_REQUEST_REJECTED, the request in flight still completes, and a new
// api connection is closed without being served.
func TestFrontDrainFinishesAnInFlightH3ResponseAndRefusesNewStreams(t *testing.T) {
	trace := newTestFrontConnectionTrace(false)
	inFlight := make(chan struct{})
	releaseSlow := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseSlow)
		})
	}
	defer release()
	servedPaths := make(chan string, 16)
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.connectQuic.tracer = trace.tracer
		env.front.h3ApiHandler = func(interfaceName string) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				servedPaths <- r.URL.Path
				if r.URL.Path == "/slow" {
					close(inFlight)
					<-releaseSlow
				}
				fmt.Fprintf(w, "done %s", r.URL.Path)
			})
		}
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	conn, err := env.dialQuic(address, frontPortQuic, testFrontApiName, []string{http3.NextProtoH3})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	env.nextDispatch()
	clientConn := (&http3.Transport{}).NewClientConn(conn)
	type slowResult struct {
		status int
		body   string
		err    error
	}
	slowResults := make(chan slowResult, 1)
	go func() {
		request, err := http.NewRequestWithContext(env.ctx, http.MethodGet, "https://"+testFrontApiName+"/slow", nil)
		if err != nil {
			slowResults <- slowResult{err: err}
			return
		}
		response, err := clientConn.RoundTrip(request)
		if err != nil {
			slowResults <- slowResult{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		slowResults <- slowResult{status: response.StatusCode, body: string(body), err: err}
	}()
	testFrontWait(t, inFlight, "the slow request")

	env.frontServer.beginDrain()
	trace.waitGoaways(t, 1)

	// a stream opened after the GOAWAY is refused
	ctx, cancel := context.WithTimeout(env.ctx, testFrontProgressTimeout)
	defer cancel()
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stream.Write([]byte{0})
	_, err = stream.Read(make([]byte, 1))
	var streamErr *quic.StreamError
	if !errors.As(err, &streamErr) || !streamErr.Remote || streamErr.ErrorCode != quic.StreamErrorCode(http3.ErrCodeRequestRejected) {
		t.Fatalf("a new stream during the drain read %v, want H3_REQUEST_REJECTED", err)
	}

	// the request in flight completes
	release()
	select {
	case result := <-slowResults:
		if result.err != nil || result.status != http.StatusOK || result.body != "done /slow" {
			t.Fatalf("the in-flight request = %+v", result)
		}
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("the in-flight request never completed")
	}

	// a new api connection is closed without being served
	fresh, err := env.dialQuic(address, frontPortQuic, testFrontApiName, []string{http3.NextProtoH3})
	if code := testFrontApplicationCode(t, fresh, err); code != quic.ApplicationErrorCode(http3.ErrCodeNoError) {
		t.Fatalf("a new api connection during the drain closed with %#x", code)
	}
	if dispatch := env.nextDispatch(); dispatch.nameClass != nginx.NameClassApi {
		t.Fatalf("the new connection's dispatch = %+v", dispatch)
	}
	if paths := []string{<-servedPaths}; !slices.Equal(paths, []string{"/slow"}) {
		t.Fatalf("served %v", paths)
	}
	select {
	case path := <-servedPaths:
		t.Fatalf("the drain served a new request: %s", path)
	default:
	}
	if events := env.front.recordedEvents(); !slices.Equal(events, []string{"front begin drain"}) {
		t.Fatalf("front events %v", events)
	}
}

// A client of one TLS connection to an https socket that hijacks its
// request into a line echo, as a websocket or H1+ tunnel holds its
// connection.
type testFrontHijackedConn struct {
	t       testing.TB
	tlsConn *tls.Conn
	reader  *bufio.Reader
}

// The https handler of a front whose /upgrade hijacks into a line echo.
func testFrontEchoHandler(interfaceName string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upgrade" {
			fmt.Fprintf(w, "plain %s", interfaceName)
			return
		}
		conn, readWriter, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		readWriter.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: test-echo\r\nConnection: Upgrade\r\n\r\n")
		readWriter.Flush()
		for {
			line, err := readWriter.ReadString('\n')
			if err != nil {
				return
			}
			readWriter.WriteString(line)
			if err := readWriter.Flush(); err != nil {
				return
			}
		}
	})
}

// Opens a TLS connection to eth1's https socket and hijacks it into the
// echo handler's /upgrade.
func (self *testFrontEnv) dialHijacked(serverName string) *testFrontHijackedConn {
	self.t.Helper()
	address, _ := self.address(testFrontInterfaceA, frontFamilyIpv4)
	tlsConn, err := tls.Dial("tcp", self.bindAddress(address, frontPortHttps), self.clientTlsConfig(serverName, []string{"http/1.1"}))
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() { tlsConn.Close() })
	tlsConn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
	fmt.Fprintf(tlsConn, "GET /upgrade HTTP/1.1\r\nHost: %s\r\nUpgrade: test-echo\r\nConnection: Upgrade\r\n\r\n", serverName)
	reader := bufio.NewReader(tlsConn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		self.t.Fatal(err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		self.t.Fatalf("upgrade status %d", response.StatusCode)
	}
	return &testFrontHijackedConn{
		t:       self.t,
		tlsConn: tlsConn,
		reader:  reader,
	}
}

// Sends one line and reads it back.
func (self *testFrontHijackedConn) echo(text string) {
	self.t.Helper()
	fmt.Fprintf(self.tlsConn, "%s\n", text)
	line, err := self.reader.ReadString('\n')
	if err != nil || line != text+"\n" {
		self.t.Fatalf("echo of %q = %q %v", text, line, err)
	}
}

// Reads until the server ends the connection; a timeout fails.
func testFrontReadEnd(t testing.TB, reader io.Reader, what string) {
	t.Helper()
	buffer := make([]byte, 1024)
	for {
		_, err := reader.Read(buffer)
		if err == nil {
			continue
		}
		var timeoutErr interface{ Timeout() bool }
		if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
			t.Fatalf("%s was not ended: %v", what, err)
		}
		return
	}
}

// A hijacked connection, a websocket or H1+ tunnel, stays through the drain
// until the drain's deadline, while the drain closes idle keepalive
// connections at once; the forced close then cuts it.
func TestFrontDrainKeepsAHijackedConnectionToTheDeadline(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.front.httpsHandler = testFrontEchoHandler
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	hijacked := env.dialHijacked(testFrontConnectName)
	hijacked.echo("before")

	// an idle keepalive connection
	idleConn, err := tls.Dial("tcp", env.bindAddress(address, frontPortHttps), env.clientTlsConfig(testFrontApiName, []string{"http/1.1"}))
	if err != nil {
		t.Fatal(err)
	}
	defer idleConn.Close()
	idleConn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
	fmt.Fprintf(idleConn, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", testFrontApiName)
	idleReader := bufio.NewReader(idleConn)
	response, err := http.ReadResponse(idleReader, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.Close {
		t.Fatal("the keepalive connection was not kept alive before the drain")
	}

	env.frontServer.beginDrain()
	testFrontReadEnd(t, idleReader, "the idle keepalive connection")
	hijacked.echo("during")

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		env.frontServer.waitDrained()
	}()
	hijacked.echo("still")
	select {
	case <-drained:
		t.Fatal("the drain ended with a hijacked connection open, before its deadline")
	default:
	}

	endTestFrontDrain(env.frontServer)
	testFrontWait(t, drained, "the drain's end at its deadline")
	hijacked.echo("after the deadline, before the forced close")
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		env.frontServer.close()
	}()
	testFrontWait(t, closed, "the forced close")
	testFrontReadEnd(t, hijacked.reader, "the hijacked connection")
}

// The drain ends as soon as the last tracked connection closes, before its
// deadline.
func TestFrontDrainEndsWhenTheLastConnectionCloses(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.front.httpsHandler = testFrontEchoHandler
	})
	hijacked := env.dialHijacked(testFrontConnectName)
	hijacked.echo("before")
	env.frontServer.beginDrain()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		env.frontServer.waitDrained()
	}()
	hijacked.echo("during")
	hijacked.tlsConn.Close()
	testFrontWait(t, drained, "the drain's end once the last connection closed")
}

// The raw sockets of a test's https connections, below the front's tracked
// connections, and a signal each time a public server holds a connection
// idle, so a test can wedge an idle keepalive connection's server side.
type testFrontIdleConns struct {
	accepted chan *testFrontStallingConn
	idle     chan struct{}
}

// Wraps the env's tcp binds and its servers' ConnState, before the start.
func newTestFrontIdleConns(env *testFrontEnv) *testFrontIdleConns {
	idleConns := &testFrontIdleConns{
		accepted: make(chan *testFrontStallingConn, 16),
		idle:     make(chan struct{}, 16),
	}
	listen := env.settings.Listen
	env.settings.Listen = func(network string, address string) (net.Listener, error) {
		listener, err := listen(network, address)
		if err != nil {
			return listener, err
		}
		return &testFrontStallingListener{
			Listener: listener,
			accepted: idleConns.accepted,
		}, nil
	}
	env.front.connState = func(_ net.Conn, connState http.ConnState) {
		if connState == http.StateIdle {
			select {
			case idleConns.idle <- struct{}{}:
			default:
			}
		}
	}
	return idleConns
}

// Serves one request on a new h1 keepalive connection to eth1's https
// socket and waits until its server holds it idle. Returns the client and
// the connection's raw socket.
func (self *testFrontIdleConns) serveIdle(env *testFrontEnv) (*testFrontH1Client, *testFrontStallingConn) {
	env.t.Helper()
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	client := env.dialH1Client(env.bindAddress(address, frontPortHttps), testFrontApiName)
	if response := client.get(testFrontApiName, "/"); response.Close {
		env.t.Fatal("the connection was not kept alive")
	}
	var stallingConn *testFrontStallingConn
	select {
	case stallingConn = <-self.accepted:
	case <-time.After(testFrontProgressTimeout):
		env.t.Fatal("no connection was accepted")
	}
	// a front that never cuts a wedge, or a stalled write, would otherwise
	// hold the test's own close of the front
	env.t.Cleanup(func() {
		stallingConn.Close()
	})
	testFrontWait(env.t, self.idle, "the keepalive connection's idle state")
	return client, stallingConn
}

// As serveIdle, then wedges the raw socket: every later write on it,
// close_notify included, waits for its close.
func (self *testFrontIdleConns) wedgeIdle(env *testFrontEnv) *testFrontStallingConn {
	env.t.Helper()
	_, stallingConn := self.serveIdle(env)
	stallingConn.wedged.Store(true)
	return stallingConn
}

// The forced close is bounded whatever a connection's TLS would send: it
// cuts every raw socket before http.Server.Close closes the TLS connections
// the servers own, each with close_notify. An idle keepalive connection
// whose close_notify waits past the close's bound, as one does behind the
// answer to a KeyUpdate request from a client that stopped reading until
// that answer's send_timeout, would otherwise hold the close, and with it
// the exit after a handler retirement's cancel, which closes the front
// without a drain.
func TestFrontForcedCloseIsNotHeldByAWedgedConnection(t *testing.T) {
	reports := make(chan string, 4)
	var idleConns *testFrontIdleConns
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		idleConns = newTestFrontIdleConns(env)
		env.settings.forcedCloseReportObserverForTest = func(step string) {
			reports <- step
		}
	})
	wedged := idleConns.wedgeIdle(env)

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		env.frontServer.close()
	}()
	testFrontWait(t, closed, "the forced close")
	select {
	case <-wedged.closed:
	default:
		t.Fatal("the forced close returned with the wedged connection open")
	}
	select {
	case step := <-reports:
		t.Fatalf("the forced close reported the %s as not closed", step)
	default:
	}
}

// An idle keepalive connection whose close waits, here for good, holds no
// step of the drain: the servers' Shutdowns close idle connections off the
// drain's goroutine, so the exchange drain and the h3 GOAWAYs begin while
// that close still waits; and at the drain's deadline the forced close cuts
// the raw socket, which frees the Shutdown holding its server's lock,
// before it closes the servers. The cancel comes last.
func TestFrontDrainIsNotHeldByAWedgedIdleConnection(t *testing.T) {
	var idleConns *testFrontIdleConns
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		idleConns = newTestFrontIdleConns(env)
	})
	wedged := idleConns.wedgeIdle(env)
	warpStatus := &router.WarpStatusState{}
	warpStatus.SetReady()

	// whether the wedged connection was still open as the exchange drain began
	exchangeBegan := make(chan bool, 1)
	exchangeDrain := func() {
		select {
		case <-wedged.closed:
			exchangeBegan <- false
		default:
			exchangeBegan <- true
		}
	}
	canceled := make(chan struct{})
	go drainConnect(warpStatus, env.frontServer, exchangeDrain, func() {
		close(canceled)
	})
	select {
	case open := <-exchangeBegan:
		if !open {
			t.Fatal("the exchange drain began only once the wedged connection was cut")
		}
	case <-time.After(testFrontProgressTimeout):
		t.Fatal("the exchange drain never began: the drain's start waits on the wedged connection's close")
	}
	quicRuns := func() []*frontQuicRun {
		env.frontServer.stateLock.Lock()
		defer env.frontServer.stateLock.Unlock()
		quicRuns := []*frontQuicRun{}
		for _, socket := range env.frontServer.sockets {
			if quicRun, ok := socket.run.(*frontQuicRun); ok {
				quicRuns = append(quicRuns, quicRun)
			}
		}
		return quicRuns
	}()
	if len(quicRuns) == 0 {
		t.Fatal("no QUIC socket is up")
	}
	for _, quicRun := range quicRuns {
		drainBegan := func() bool {
			quicRun.stateLock.Lock()
			defer quicRun.stateLock.Unlock()
			return quicRun.drainCtx != nil
		}()
		if !drainBegan {
			t.Fatalf("the h3 drain of %s did not begin with the exchange drain", quicRun.socket.key)
		}
	}

	endTestFrontDrain(env.frontServer)
	testFrontWait(t, canceled, "the cancel after the forced close")
	select {
	case <-wedged.closed:
	default:
		t.Fatal("the drain ended with the wedged connection open")
	}
}

// Each h2 connection gets one GOAWAY when the drain begins, while its
// stream in flight still completes.
func TestFrontDrainSendsOneH2GoawayPerConnection(t *testing.T) {
	inFlight := make(chan struct{})
	releaseSlow := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseSlow)
		})
	}
	defer release()
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.front.httpsHandler = func(interfaceName string) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/slow" {
					close(inFlight)
					<-releaseSlow
				}
				fmt.Fprint(w, "done")
			})
		}
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	tlsConn, err := tls.Dial("tcp", env.bindAddress(address, frontPortHttps), env.clientTlsConfig(testFrontApiName, []string{"h2"}))
	if err != nil {
		t.Fatal(err)
	}
	defer tlsConn.Close()
	if protocol := tlsConn.ConnectionState().NegotiatedProtocol; protocol != "h2" {
		t.Fatalf("negotiated %q", protocol)
	}
	tlsConn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
	if _, err := io.WriteString(tlsConn, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	framer := http2.NewFramer(tlsConn, tlsConn)
	if err := framer.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	headerBlock := &bytes.Buffer{}
	encoder := hpack.NewEncoder(headerBlock)
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: http.MethodGet},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: testFrontApiName},
		{Name: ":path", Value: "/slow"},
	} {
		encoder.WriteField(field)
	}
	if err := framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      1,
		BlockFragment: headerBlock.Bytes(),
		EndStream:     true,
		EndHeaders:    true,
	}); err != nil {
		t.Fatal(err)
	}
	testFrontWait(t, inFlight, "the slow h2 request")

	env.frontServer.beginDrain()
	goawayCount := 0
	streamEnded := false
	for !streamEnded || goawayCount == 0 {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatalf("read before the GOAWAY and the response: %v", err)
		}
		switch frame := frame.(type) {
		case *http2.SettingsFrame:
			if !frame.IsAck() {
				framer.WriteSettingsAck()
			}
		case *http2.PingFrame:
			if !frame.IsAck() {
				framer.WritePing(true, frame.Data)
			}
		case *http2.GoAwayFrame:
			goawayCount += 1
			if frame.ErrCode != http2.ErrCodeNo {
				t.Fatalf("GOAWAY with %s, want a graceful one", frame.ErrCode)
			}
			// the stream in flight completes after it
			release()
		case *http2.DataFrame:
			if frame.StreamID == 1 && frame.StreamEnded() {
				streamEnded = true
			}
		case *http2.HeadersFrame:
			if frame.StreamID == 1 && frame.StreamEnded() {
				streamEnded = true
			}
		}
	}
	// the server closes the connection once its last stream is done, with no
	// second GOAWAY
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			var timeoutErr interface{ Timeout() bool }
			if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
				t.Fatalf("the drained h2 connection was not closed: %v", err)
			}
			break
		}
		if _, ok := frame.(*http2.GoAwayFrame); ok {
			goawayCount += 1
		}
	}
	if goawayCount != 1 {
		t.Fatalf("%d GOAWAYs on one h2 connection, want one", goawayCount)
	}
}

// The forced close waits for an api request in flight, which its close
// cancels, as alt's Close did: the front closes after the last handler.
func TestFrontCloseWaitsForAnInFlightApiRequest(t *testing.T) {
	// what the handler still does once its request is canceled
	const handlerCleanup = 200 * time.Millisecond
	inFlight := make(chan struct{})
	handlerDone := make(chan struct{})
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.front.h3ApiHandler = func(interfaceName string) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				close(inFlight)
				<-r.Context().Done()
				time.Sleep(handlerCleanup)
			})
		}
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	client, _ := env.apiClient(address, frontPortQuic, nil)
	go func() {
		if response, err := client.Get("https://" + testFrontApiName + "/in-flight"); err == nil {
			response.Body.Close()
		}
	}()
	testFrontWait(t, inFlight, "the api request")

	env.frontServer.close()
	select {
	case <-handlerDone:
	default:
		t.Fatal("the close returned while an api request's handler was still running")
	}
}

// A handler that ignores its canceled request holds the forced close for
// ForcedCloseTimeout and no longer.
func TestFrontCloseBoundsAStuckApiRequest(t *testing.T) {
	const forcedCloseTimeout = 100 * time.Millisecond
	inFlight := make(chan struct{})
	stuck := make(chan struct{})
	var unstickOnce sync.Once
	unstick := func() {
		unstickOnce.Do(func() {
			close(stuck)
		})
	}
	defer unstick()
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.ForcedCloseTimeout = forcedCloseTimeout
		env.front.h3ApiHandler = func(interfaceName string) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(inFlight)
				// ignores its canceled request
				<-stuck
			})
		}
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	client, _ := env.apiClient(address, frontPortQuic, nil)
	go func() {
		if response, err := client.Get("https://" + testFrontApiName + "/stuck"); err == nil {
			response.Body.Close()
		}
	}()
	testFrontWait(t, inFlight, "the api request")

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		env.frontServer.close()
	}()
	select {
	case <-closed:
	case <-time.After(50 * forcedCloseTimeout):
		t.Fatalf("the close waited over %s for a stuck api request, past its %s bound", 50*forcedCloseTimeout, forcedCloseTimeout)
	}
}

// A close that finds the front started waits for every socket's supervisor,
// even one start has yet to run, since start counts the supervisors before
// the front reads as started. The close runs on start's own goroutine in the
// moment between the two, so no supervisor can run before the close's
// bounded wait for the sockets ends, and the close reports them as not
// closed in time; a close that counted none would return at once with
// nothing to report. Once start goes on, every socket is released.
func TestFrontCloseRacingStartWaitsForEverySupervisor(t *testing.T) {
	reports := make(chan string, 4)
	closed := false
	env := newUnstartedTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.ForcedCloseTimeout = 100 * time.Millisecond
		env.settings.forcedCloseReportObserverForTest = func(step string) {
			reports <- step
		}
		env.settings.startedObserverForTest = func() {
			env.frontServer.close()
			closed = true
		}
	})
	if err := env.frontServer.start(env.ctx, env.front, env.connectQuic); err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("start never read as started")
	}
	select {
	case step := <-reports:
		if step != "sockets" {
			t.Fatalf("the close reported the %s, want the sockets", step)
		}
	default:
		t.Fatal("the close found no supervisor to wait for")
	}
	env.waitSocketsUp(0)
}

// The drain is not held by an api request stream whose HEADERS never
// arrive: the stream is reset at the client header timeout, its connection
// then idles out at the keepalive timeout under the drain's graceful
// shutdown, and the drain ends long before its deadline. A request on a
// later stream of the same connection is the barrier that the pending
// stream was accepted first, since quic-go accepts a connection's streams in
// order of their ids. Both are written raw, since an http3 client closes a
// connection itself once a GOAWAY finds it without a request of its own.
func TestFrontDrainEndsDespiteAnApiStreamWhoseHeadersNeverArrive(t *testing.T) {
	served := make(chan struct{}, 1)
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.Front.Http.ClientHeaderTimeout = 200 * time.Millisecond
		env.settings.Front.Http.KeepaliveTimeout = 200 * time.Millisecond
		env.front.h3ApiHandler = func(string) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case served <- struct{}{}:
				default:
				}
			})
		}
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	conn, err := env.dialQuic(address, frontPortQuic, testFrontApiName, []string{http3.NextProtoH3})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	env.nextDispatch()

	pending, err := conn.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	// a HEADERS frame's type and announced length, and nothing of its
	// field section
	frameHead := quicvarint.Append(nil, 0x1)
	frameHead = quicvarint.Append(frameHead, 64)
	if _, err := pending.Write(frameHead); err != nil {
		t.Fatal(err)
	}
	// a complete GET / on a later stream, its field section encoded with
	// QPACK's static table alone (RFC 9204 §4.5, Appendix A): no dynamic
	// table, then :method GET, :scheme https and :path / indexed, and
	// :authority as a literal with a static name reference
	fieldSection := []byte{0x00, 0x00, 0xc0 | 17, 0xc0 | 23, 0xc0 | 1, 0x50, byte(len(testFrontApiName))}
	fieldSection = append(fieldSection, testFrontApiName...)
	headersFrame := quicvarint.Append(nil, 0x1)
	headersFrame = quicvarint.Append(headersFrame, uint64(len(fieldSection)))
	headersFrame = append(headersFrame, fieldSection...)
	barrier, err := conn.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := barrier.Write(headersFrame); err != nil {
		t.Fatal(err)
	}
	barrier.Close()
	testFrontWait(t, served, "the later stream's request")

	env.frontServer.beginDrain()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		env.frontServer.waitDrained()
	}()
	testFrontWait(t, drained, "the drain's end once the pending stream was reset and its connection idled out")
	if openCount := env.frontServer.openCount.Value(); openCount != 0 {
		t.Fatalf("the drain ended with %d connections open", openCount)
	}
}

// The old alt's exit burst (10-24 to 10-26) cannot come back: the drain
// shuts each api connection down gracefully while its transport is live,
// and the forced close then closes every h3 server before its transport. An
// upstream h3 server still shutting down gracefully when its transport
// destroyed the connection would spin its accept loop, one GOAWAY writer per
// pass, for as long as the connection is closed but not done; the vendored
// http3 ends the loop there too, so the close order and the vendored loop
// each keep the burst away. With every connection held in that window, the
// close may leave at most a few goroutines per connection: quic-go's destroy
// waits once per connection id.
func TestFrontCloseStartsNoGoroutineBurstPerApiConnection(t *testing.T) {
	const (
		connectionCount         = 32
		goroutinesPerConnection = 16
		// how long every connection stays held before release
		holdDuration = 25 * time.Millisecond
	)
	trace := newTestFrontConnectionTrace(true)
	defer trace.Release()
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.connectQuic.tracer = trace.tracer
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)

	// idle api connections that keep their connection after a GOAWAY
	for range connectionCount {
		conn, err := env.dialQuic(address, frontPortQuic, testFrontApiName, []string{http3.NextProtoH3})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseWithError(0, "")
		env.nextDispatch()
	}
	env.waitOpen(connectionCount)
	env.frontServer.beginDrain()
	trace.waitGoaways(t, connectionCount)

	baseline := runtime.NumGoroutine()
	limit := baseline + goroutinesPerConnection*connectionCount
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		env.frontServer.close()
	}()
	peak := baseline
	var allHeldTime time.Time
	endTime := time.Now().Add(testFrontProgressTimeout)
	for {
		goroutines := runtime.NumGoroutine()
		peak = max(peak, goroutines)
		if limit < goroutines {
			// a burst: release at once rather than let it grow
			break
		}
		if trace.held.Value() == connectionCount {
			if allHeldTime.IsZero() {
				allHeldTime = time.Now()
			} else if holdDuration <= time.Since(allHeldTime) {
				break
			}
		}
		if endTime.Before(time.Now()) {
			t.Fatalf("%d of %d connections reached their close window", trace.held.Value(), connectionCount)
		}
		time.Sleep(100 * time.Microsecond)
	}
	trace.Release()
	testFrontWait(t, closed, "the forced close")
	t.Logf(
		"the close of %d api connections peaked at +%.1f goroutines per connection",
		connectionCount,
		float64(peak-baseline)/float64(connectionCount),
	)
	if limit < peak {
		t.Fatalf(
			"the close of %d api connections peaked at %d goroutines over a baseline of %d (+%.1f per connection, at most %d)",
			connectionCount,
			peak,
			baseline,
			float64(peak-baseline)/float64(connectionCount),
			goroutinesPerConnection,
		)
	}
	// each accept loop's pass through its graceful branch creates a GOAWAY:
	// the drain's one per connection, and none from a loop that met its
	// destroyed transport while shutting down gracefully
	if goaways := trace.goaways.Value(); goaways != connectionCount {
		t.Fatalf("%d GOAWAYs for %d api connections, want one each: a graceful accept loop spun", goaways, connectionCount)
	}
	if !strings.Contains(strings.Join(env.front.recordedEvents(), ","), "front close") {
		t.Fatal("the nginx front was not closed")
	}
}

// A client that closes its api connection after the drain's GOAWAY, as a
// client should, gets no second GOAWAY. The connection's h3 server finishes
// serving it while quic-go is still closing it, with the close window held
// open: the vendored http3 ends its graceful accept loop there, where
// upstream's spun, one GOAWAY writer per pass, until the window closed.
func TestFrontDrainSendsOneH3GoawayWhenTheClientCloses(t *testing.T) {
	trace := newTestFrontConnectionTrace(true)
	defer trace.Release()
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.connectQuic.tracer = trace.tracer
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	conn, err := env.dialQuic(address, frontPortQuic, testFrontApiName, []string{http3.NextProtoH3})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	env.nextDispatch()
	env.waitOpen(1)
	env.frontServer.beginDrain()
	trace.waitGoaways(t, 1)

	conn.CloseWithError(quic.ApplicationErrorCode(http3.ErrCodeNoError), "")
	trace.waitHeld(t, 1)
	trace.waitServedWithoutAnotherGoaway(t, env.frontServer, 1)

	trace.Release()
	deadline := time.After(testFrontProgressTimeout)
	for {
		releasedCount, changed := env.connectQuic.releasedCount.Get()
		if releasedCount == 1 {
			break
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("the closed api connection's dispatch never finished")
		}
	}
	if goaways := trace.goaways.Value(); goaways != 1 {
		t.Fatalf("%d GOAWAYs for one api connection its client closed during the drain", goaways)
	}
}

// A QUIC socket lost while the drain shuts its api connections down
// gracefully destroys every connection on it at once (D22), and each gets no
// second GOAWAY. Each h3 server finishes serving its connection while quic-go
// destroys it, with the close window held open and the lost sockets' release
// held, so only the destroyed connection can end the serve: before the
// release, nothing closes the h3 servers.
func TestFrontDrainSendsOneH3GoawayWhenItsSocketIsLost(t *testing.T) {
	const connectionCount = 4
	trace := newTestFrontConnectionTrace(true)
	defer trace.Release()
	lostSockets := make(chan frontSocketKey, 16)
	releaseLost := make(chan struct{})
	var releaseLostOnce sync.Once
	release := func() {
		releaseLostOnce.Do(func() {
			close(releaseLost)
		})
	}
	defer release()
	lostBindAddress := ""
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.connectQuic.tracer = trace.tracer
		address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
		lostBindAddress = env.bindAddress(address, frontPortQuic)
		env.settings.socketServeEndObserverForTest = func(key frontSocketKey) {
			if key.portKind == frontPortQuic && key.bindAddress() == lostBindAddress {
				lostSockets <- key
				<-releaseLost
			}
		}
	})
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	for range connectionCount {
		conn, err := env.dialQuic(address, frontPortQuic, testFrontApiName, []string{http3.NextProtoH3})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseWithError(0, "")
		env.nextDispatch()
	}
	env.waitOpen(connectionCount)
	env.frontServer.beginDrain()
	trace.waitGoaways(t, connectionCount)

	packetConns := env.binds.boundPacketConns(lostBindAddress)
	if len(packetConns) != env.settings.SocketsPerPort {
		t.Fatalf("%d quic sockets bound on %s", len(packetConns), lostBindAddress)
	}
	for _, packetConn := range packetConns {
		packetConn.Close()
	}
	for range packetConns {
		select {
		case <-lostSockets:
		case <-time.After(testFrontProgressTimeout):
			t.Fatal("a lost socket's serve did not end")
		}
	}
	trace.waitHeld(t, connectionCount)
	trace.waitServedWithoutAnotherGoaway(t, env.frontServer, connectionCount)

	release()
	trace.Release()
	env.frontServer.waitDrained()
	if goaways := trace.goaways.Value(); goaways != connectionCount {
		t.Fatalf("%d GOAWAYs for %d api connections a socket loss destroyed during the drain", goaways, connectionCount)
	}
}

// A forwarded websocket that never closes holds the drain only to the drain
// timeout less the stop reserve. warpctl's stop grace equals the drain
// timeout, so a wait to the drain timeout would leave the forced close and
// the cancel to the SIGKILL; here the drain timeout is an hour, as the grace
// is in production, and the reserve leaves a moment of it, so the cancel
// comes long before the grace ends.
func TestFrontDrainEndsInsideTheStopGraceWithAWebsocketOpen(t *testing.T) {
	const drainWait = 200 * time.Millisecond
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.front.httpsHandler = testFrontEchoHandler
		env.settings.Front.Http.DrainTimeout = time.Hour
		env.settings.DrainStopReserve = time.Hour - drainWait
	})
	hijacked := env.dialHijacked(testFrontConnectName)
	hijacked.echo("before")
	warpStatus := &router.WarpStatusState{}
	warpStatus.SetReady()
	canceled := make(chan struct{})
	go drainConnect(warpStatus, env.frontServer, func() {}, func() {
		env.front.record("cancel")
		close(canceled)
	})
	testFrontWait(t, canceled, "the cancel inside the stop grace")
	want := []string{"front begin drain", "front close", "cancel"}
	if events := env.front.recordedEvents(); !slices.Equal(events, want) {
		t.Fatalf("drain events %v, want %v", events, want)
	}
	testFrontReadEnd(t, hijacked.reader, "the websocket")
}

// The drain's deadline is the drain timeout less the stop reserve: by
// default 58 m of nginx's 60 m, leaving the forced close its bound and the
// rest of the stop its margin. A drain timeout no longer than the reserve is
// refused; no drain timeout is no deadline, and no reserve keeps the drain
// timeout.
func TestFrontDrainDeadlineKeepsTheStopReserve(t *testing.T) {
	settings := DefaultFrontServerSettings()
	if deadline := settings.drainDeadlineTimeout(); deadline != 58*time.Minute {
		t.Fatalf("default drain deadline %s, want 58m", deadline)
	}
	// the forced close and the private listener's 30 s shutdown fit in the
	// reserve
	if settings.DrainStopReserve <= settings.Front.Http.ForcedCloseTimeout+30*time.Second {
		t.Fatalf("stop reserve %s leaves no margin after the forced close's %s", settings.DrainStopReserve, settings.Front.Http.ForcedCloseTimeout)
	}
	if err := settings.validate(); err != nil {
		t.Fatal(err)
	}

	settings.Front.Http.DrainTimeout = settings.DrainStopReserve
	if err := settings.validate(); err == nil {
		t.Fatal("a drain timeout no longer than the stop reserve was accepted")
	}
	settings.Front.Http.DrainTimeout = 0
	if err := settings.validate(); err != nil {
		t.Fatal(err)
	}
	if deadline := settings.drainDeadlineTimeout(); deadline != 0 {
		t.Fatalf("drain deadline %s without a drain timeout", deadline)
	}
	settings.Front.Http.DrainTimeout = time.Hour
	settings.DrainStopReserve = 0
	if deadline := settings.drainDeadlineTimeout(); deadline != time.Hour {
		t.Fatalf("drain deadline %s without a reserve, want the drain timeout", deadline)
	}
}

// The forced close reports only the steps that did not close. Once the
// sockets step has spent the whole ForcedCloseTimeout, the nginx front's
// step has no budget left, and a front that already closed must not be
// reported as one that did not close within it. Each pass's close is a
// fresh front, since the report raced a due timer against a closed channel
// one time in two.
func TestFrontCloseReportsOnlyTheStepsThatDidNotClose(t *testing.T) {
	const (
		forcedCloseTimeout = 50 * time.Millisecond
		passCount          = 10
	)
	for pass := range passCount {
		releaseSockets := make(chan struct{})
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() {
				close(releaseSockets)
			})
		}
		reportLock := sync.Mutex{}
		reports := []string{}
		env := newTestFrontEnv(t, func(env *testFrontEnv) {
			env.settings.Front.Http.ForcedCloseTimeout = forcedCloseTimeout
			// every socket's release waits past the budget
			env.settings.socketServeEndObserverForTest = func(key frontSocketKey) {
				<-releaseSockets
			}
			env.settings.forcedCloseReportObserverForTest = func(step string) {
				reportLock.Lock()
				defer reportLock.Unlock()
				reports = append(reports, step)
			}
		})
		env.frontServer.close()
		release()
		reportLock.Lock()
		passReports := slices.Clone(reports)
		reportLock.Unlock()
		if !slices.Equal(passReports, []string{"sockets"}) {
			t.Fatalf("pass %d: the forced close reported %v not closed, want only the sockets", pass, passReports)
		}
	}
}
