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
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	http3qlog "github.com/quic-go/quic-go/http3/qlog"
	"github.com/quic-go/quic-go/qlog"
	"github.com/quic-go/quic-go/qlogwriter"
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
// holding, holds each connection inside its close window. quic-go records a connection's close after it closed the
// connection's streams and before it cancels the connection's context, so
// while the record is held every accept on the connection fails at once and
// its context is still live: the window in which a graceful http3 shutdown
// spins. In production the window stays open while Transport.close holds the
// transport mutex over its destroy fan-out and while every core is busy.
type testFrontConnectionTrace struct {
	// every GOAWAY created: one per connection the drain shut down, and one
	// per pass of an accept loop spinning against a destroyed transport
	goaways     *connect.MonitorValue[int]
	holding     bool
	held        atomic.Int64
	release     chan struct{}
	releaseOnce sync.Once
}

func newTestFrontConnectionTrace(holding bool) *testFrontConnectionTrace {
	return &testFrontConnectionTrace{
		goaways: connect.NewMonitorValue(0),
		holding: holding,
		release: make(chan struct{}),
	}
}

// quic.Config.Tracer
func (self *testFrontConnectionTrace) tracer(context.Context, bool, quic.ConnectionID) qlogwriter.Trace {
	return self
}

func (self *testFrontConnectionTrace) AddProducer() qlogwriter.Recorder {
	return self
}

// http3 records its frames here; the connection records whatever it does
func (self *testFrontConnectionTrace) SupportsSchemas(schema string) bool {
	return schema == http3qlog.EventSchema
}

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
			self.held.Add(1)
			<-self.release
		}
	}
}

func (self *testFrontConnectionTrace) Close() error {
	return nil
}

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

// The old alt's exit burst (10-24 to 10-26) cannot come back: the drain
// shuts each api connection down gracefully while its transport is live,
// and the forced close then closes every h3 server before its transport. An
// h3 server still shutting down gracefully when its transport destroyed the
// connection would spin its accept loop, one GOAWAY writer per pass, for as
// long as the connection is closed but not done. With every connection held
// in that window, the close may leave at most a few goroutines per
// connection: quic-go's destroy waits once per connection id.
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
	deadline := time.After(testFrontProgressTimeout)
	for {
		openCount, changed := env.frontServer.openCount.Get()
		if openCount == connectionCount {
			break
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("%d of %d api connections registered", openCount, connectionCount)
		}
	}
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
		if trace.held.Load() == connectionCount {
			if allHeldTime.IsZero() {
				allHeldTime = time.Now()
			} else if holdDuration <= time.Since(allHeldTime) {
				break
			}
		}
		if endTime.Before(time.Now()) {
			t.Fatalf("%d of %d connections reached their close window", trace.held.Load(), connectionCount)
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
