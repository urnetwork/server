package nginx

// Fixtures for the forwarder's tests: recording upstream blocks on loopback,
// a front that hands each request to a group, and groups built from
// synthetic route tables (.example names, documentation addresses for
// clients, loopback for sockets).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/net/http2"
)

// One request as a recording upstream saw it.
type recordedRequest struct {
	block string
	// the front's end of the connection, which tells connections apart
	remoteAddr       string
	method           string
	requestUri       string
	proto            string
	host             string
	header           http.Header
	contentLength    int64
	transferEncoding []string
	body             []byte
	bodyErr          error
}

// A block: an http server on loopback that records each request, then
// answers by its handler. A nil handler answers 200 with the block's name.
type recordingUpstream struct {
	t       *testing.T
	name    string
	server  *httptest.Server
	port    int
	handler func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest)
	// each recorded request is also sent here, when set, before the handler
	// runs: a barrier for tests that hold requests
	arrivals chan *recordedRequest

	stateLock sync.Mutex
	requests  []*recordedRequest
}

// A recording upstream on a new loopback listener, closed when the test
// ends.
func newRecordingUpstream(t *testing.T, name string, handler func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest)) *recordingUpstream {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return newRecordingUpstreamOn(t, name, listener, handler)
}

// A recording upstream on a listener the test opened, so it can learn the
// port before it decides what serves there.
func newRecordingUpstreamOn(t *testing.T, name string, listener net.Listener, handler func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest)) *recordingUpstream {
	upstream := &recordingUpstream{
		t:       t,
		name:    name,
		handler: handler,
	}
	upstream.server = httptest.NewUnstartedServer(http.HandlerFunc(upstream.serveHttp))
	upstream.server.Listener.Close()
	upstream.server.Listener = listener
	upstream.server.Start()
	t.Cleanup(upstream.server.Close)
	upstream.port = listener.Addr().(*net.TCPAddr).Port
	return upstream
}

// Records the request, its whole body included, then answers it.
func (self *recordingUpstream) serveHttp(w http.ResponseWriter, r *http.Request) {
	body, bodyErr := io.ReadAll(r.Body)
	recorded := &recordedRequest{
		block:            self.name,
		remoteAddr:       r.RemoteAddr,
		method:           r.Method,
		requestUri:       r.RequestURI,
		proto:            r.Proto,
		host:             r.Host,
		header:           r.Header.Clone(),
		contentLength:    r.ContentLength,
		transferEncoding: r.TransferEncoding,
		body:             body,
		bodyErr:          bodyErr,
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, recorded)
	}()
	if self.arrivals != nil {
		self.arrivals <- recorded
	}
	if self.handler != nil {
		self.handler(w, r, recorded)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, self.name)
}

// The requests recorded so far.
func (self *recordingUpstream) recorded() []*recordedRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*recordedRequest{}, self.requests...)
}

// Service names, numbered so that each run of a test, under -count, counts
// its metrics apart.
var testServiceNameCount atomic.Int64

// A service name for one run of a test: the name and a number.
func testServiceName(name string) string {
	return name + "-" + strconv.FormatInt(testServiceNameCount.Add(1), 10)
}

// A block for a test group: a name, a weight and the port its upstream
// listens on.
type testBlock struct {
	name   string
	weight int
	port   int
}

// A port with no listener: a block there refuses connections.
func closedPort(t *testing.T) int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

// Settings for tests: nginx's defaults, with the sweep off so no timer acts
// on its own, and a long wait for a request write that reports after its
// response was read, as Go's tests lengthen maxWriteWaitBeforeConnReuse, so
// a loaded scheduler never costs a test its pooled connection. Tests of
// that wait order it through the group's waitAfter.
func testForwardFrontSettings() *FrontSettings {
	settings := DefaultFrontSettings()
	settings.Forward.KeepaliveSweepInterval = 0
	settings.Forward.KeepaliveWriteWait = 30 * time.Second
	return settings
}

// The pool's idle connections, the least recently used first. The test
// fails unless there are want of them.
func idleConnsOf(t *testing.T, pool *upstreamPool, want int) []*upstreamConnection {
	t.Helper()
	pool.stateLock.Lock()
	defer pool.stateLock.Unlock()
	if len(pool.idleConns) != want {
		t.Fatalf("pool keeps %d connections, want %d", len(pool.idleConns), want)
	}
	return slices.Clone(pool.idleConns)
}

// A service of the route table with the given blocks and nginx's default
// keepalive.
func testForwardRouteService(serviceName string, testBlocks []testBlock) *RouteService {
	routeBlocks := []*RouteBlock{}
	for _, block := range testBlocks {
		routeBlocks = append(routeBlocks, &RouteBlock{
			Name:   block.name,
			Weight: block.weight,
			Port:   block.port,
		})
	}
	return &RouteService{
		Name:               serviceName,
		Exposed:            true,
		LbExposed:          true,
		Status:             "standard",
		Names:              []string{serviceName + ".service.example"},
		RedirectAliases:    []*RouteRedirectAlias{},
		CorsOrigins:        []string{},
		StreamablePaths:    []string{},
		RoutePrefixes:      []string{""},
		RouteRateLimits:    []*RouteRateLimitRule{},
		RouteRequestBodies: []*RouteRequestBody{},
		NoSuccessAccessLog: []string{},
		Keepalive: &RouteKeepalive{
			Connections:    1024,
			Requests:       8192,
			TimeSeconds:    900,
			TimeoutSeconds: 60,
		},
		Blocks: routeBlocks,
	}
}

// A route table whose upstream bridge is loopback, so blocks are local test
// servers, named as the services network is.
func testRouteTable(services ...*RouteService) *RouteTable {
	return &RouteTable{
		SchemaVersion: RouteSchemaVersion,
		Env:           "test",
		Host:          "edge-a.example",
		Generation:    "test-connect-g1",
		Domains:       []string{"service.example"},
		Upstream: &RouteUpstream{
			Host: "warpservices",
			Ipv4: "127.0.0.1",
		},
		Services: services,
	}
}

// A group for a service, closed when the test ends, and its body store.
func newTestGroup(t *testing.T, service *RouteService, settings *FrontSettings) (*upstreamGroup, *bodyStore) {
	bodyStore := newBodyStore(settings.Body)
	group := newUpstreamGroup(testRouteTable(service), service, bodyStore, settings)
	t.Cleanup(func() {
		group.close()
		bodyStore.close()
	})
	return group, bodyStore
}

// A front for tests: an http server whose handler forwards every request
// through a group with options the test chooses. served receives one value
// each time serveHttp returns, its panic included.
type testForwardFront struct {
	server  *httptest.Server
	group   *upstreamGroup
	options func(r *http.Request) forwardOptions
	served  chan any
}

// A front over a group, closed when the test ends.
func newTestForwardFront(t *testing.T, group *upstreamGroup, options func(r *http.Request) forwardOptions) *testForwardFront {
	return newTestForwardFrontWrapping(t, group, func(w http.ResponseWriter) http.ResponseWriter {
		return w
	}, options)
}

// A front over a group whose handler gives serveHttp the response writer
// wrap makes of the server's, closed when the test ends.
func newTestForwardFrontWrapping(t *testing.T, group *upstreamGroup, wrap func(w http.ResponseWriter) http.ResponseWriter, options func(r *http.Request) forwardOptions) *testForwardFront {
	front := &testForwardFront{
		group:   group,
		options: options,
		served:  make(chan any, 1024),
	}
	front.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			front.served <- recovered
			if recovered != nil {
				panic(recovered)
			}
		}()
		front.group.serveHttp(wrap(w), r, front.options(r))
	}))
	t.Cleanup(front.server.Close)
	return front
}

// A response writer that tells a test each time the forwarder sets a read
// deadline in the past on the client's connection, as interrupting a
// streamed body does, and passes every deadline, and the rest, on to the
// server's writer.
type pastReadDeadlineWriter struct {
	http.ResponseWriter
	// one value for each past deadline, dropped when it is full
	pastDeadlines chan struct{}
}

// A writer over the server's w.
func newPastReadDeadlineWriter(w http.ResponseWriter) *pastReadDeadlineWriter {
	return &pastReadDeadlineWriter{
		ResponseWriter: w,
		pastDeadlines:  make(chan struct{}, 1),
	}
}

// Sets the deadline through the server's writer, and tells the test of one
// in the past.
func (self *pastReadDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	err := http.NewResponseController(self.ResponseWriter).SetReadDeadline(deadline)
	if !deadline.IsZero() && deadline.Before(time.Now()) {
		select {
		case self.pastDeadlines <- struct{}{}:
		default:
		}
	}
	return err
}

// The server's writer, for the response controller's other methods.
func (self *pastReadDeadlineWriter) Unwrap() http.ResponseWriter {
	return self.ResponseWriter
}

// A front over a group that serves h2 over TLS, as the public TCP 443 does,
// closed when the test ends. Its server's Client speaks h2 to it.
func newTestForwardH2Front(t *testing.T, group *upstreamGroup, options func(r *http.Request) forwardOptions) *testForwardFront {
	front := &testForwardFront{
		group:   group,
		options: options,
		served:  make(chan any, 1024),
	}
	front.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			front.served <- recovered
			if recovered != nil {
				panic(recovered)
			}
		}()
		front.group.serveHttp(w, r, front.options(r))
	}))
	front.server.EnableHTTP2 = true
	front.server.StartTLS()
	t.Cleanup(front.server.Close)
	return front
}

// One h2 connection to an h2 front, through golang.org/x/net/http2's client,
// so a test sees the stream errors the front's server sends and can send
// the next stream on the same connection. It closes when the test ends.
func (self *testForwardFront) h2ClientConn(t *testing.T) *http2.ClientConn {
	tlsConfig := self.server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConfig.NextProtos = []string{"h2"}
	tlsConn, err := tls.Dial("tcp", self.server.Listener.Addr().String(), tlsConfig)
	if err != nil {
		t.Fatalf("dial h2 front: %v", err)
	}
	clientConn, err := (&http2.Transport{}).NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		t.Fatalf("h2 client connection: %v", err)
	}
	t.Cleanup(func() {
		clientConn.Close()
	})
	return clientConn
}

// The default options of a test request: a documentation ipv4 client.
func testForwardOptions(r *http.Request) forwardOptions {
	return forwardOptions{
		interfaceName:  "eth1",
		clientAddrPort: netip.MustParseAddrPort("192.0.2.10:40123"),
	}
}

// The front's url for a path.
func (self *testForwardFront) url(path string) string {
	return self.server.URL + path
}

// A client that never reuses a connection, so each request is its own.
func testClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
}

// Sends a request to the front and reads the whole response.
func testDo(t *testing.T, request *http.Request) (*http.Response, []byte, error) {
	response, err := testClient().Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return response, body, err
}

// Waits for serveHttp to return, with a bound so a broken test fails rather
// than hangs.
func (self *testForwardFront) waitServed(t *testing.T) any {
	select {
	case recovered := <-self.served:
		return recovered
	case <-time.After(30 * time.Second):
		t.Fatalf("serveHttp did not return")
		return nil
	}
}

// The physical attempts counted for a service's block and result.
func attemptCount(serviceName string, blockName string, result string) float64 {
	return testutil.ToFloat64(upstreamAttemptsCounter.WithLabelValues(serviceName, blockName, result))
}

// The D7 retries counted for a service and reason.
func retryCount(serviceName string, reason string) float64 {
	return testutil.ToFloat64(upstreamRetriesCounter.WithLabelValues(serviceName, reason))
}

// The front's own error responses counted for a service and status.
func forwardErrorCount(serviceName string, status int) float64 {
	return testutil.ToFloat64(forwardErrorsCounter.WithLabelValues(serviceName, strconv.Itoa(status)))
}

// The body refusals counted for a service and reason.
func bodyRefusalCount(serviceName string, reason string) float64 {
	return testutil.ToFloat64(forwardBodyRefusalsCounter.WithLabelValues(serviceName, reason))
}

// A raw upstream: a loopback listener whose connections a test serves by
// hand, for responses net/http won't write.
type rawUpstream struct {
	listener net.Listener
	port     int
	conns    chan net.Conn
}

// A raw upstream on a new loopback listener; it and every connection it
// accepted close when the test ends.
func newRawUpstream(t *testing.T) *rawUpstream {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	upstream := &rawUpstream{
		listener: listener,
		port:     listener.Addr().(*net.TCPAddr).Port,
		conns:    make(chan net.Conn, 16),
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			upstream.conns <- conn
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		for {
			select {
			case conn := <-upstream.conns:
				conn.Close()
			default:
				return
			}
		}
	})
	return upstream
}

// The next accepted connection, with its request head read.
func (self *rawUpstream) accept(t *testing.T) (net.Conn, *bufio.Reader, *http.Request) {
	select {
	case conn := <-self.conns:
		t.Cleanup(func() { conn.Close() })
		reader := bufio.NewReader(conn)
		request, err := http.ReadRequest(reader)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		return conn, reader, request
	case <-time.After(30 * time.Second):
		t.Fatalf("no upstream connection")
		return nil, nil, nil
	}
}

// Makes the group wrap every connection it dials from now on, as wrap
// returns it, given the address dialed.
func wrapGroupDials(group *upstreamGroup, wrap func(conn net.Conn, address string) net.Conn) {
	dialer := &net.Dialer{}
	group.dialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return wrap(conn, address), nil
	}
}

// A dialed connection whose first write sends its bytes, then holds before
// it returns, as a write goroutine descheduled right after its last write
// would: the block has the bytes, and the forwarder doesn't know yet. held
// is closed when the hold starts; the hold ends on release or Close.
type holdWriteConn struct {
	net.Conn
	armed     atomic.Bool
	held      chan struct{}
	released  chan struct{}
	release   func()
	closed    chan struct{}
	closeOnce sync.Once
}

// A connection over conn whose first write holds. The hold ends when the
// test does, at the latest.
func newHoldWriteConn(t *testing.T, conn net.Conn) *holdWriteConn {
	holdConn := &holdWriteConn{
		Conn:     conn,
		held:     make(chan struct{}),
		released: make(chan struct{}),
		closed:   make(chan struct{}),
	}
	holdConn.release = sync.OnceFunc(func() {
		close(holdConn.released)
	})
	holdConn.armed.Store(true)
	t.Cleanup(holdConn.release)
	return holdConn
}

// Writes p; the first write then holds until released.
func (self *holdWriteConn) Write(p []byte) (int, error) {
	n, err := self.Conn.Write(p)
	if self.armed.CompareAndSwap(true, false) {
		close(self.held)
		<-self.released
	}
	return n, err
}

// Closes the connection, which ends a held write, as cutting a connection
// ends a write blocked on it.
func (self *holdWriteConn) Close() error {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
	self.release()
	return self.Conn.Close()
}

// A dialed connection whose first write carrying marker holds before it
// writes, as a write to a block that stopped reading blocks, until released
// or closed. held is closed when the hold starts.
type holdMarkedWriteConn struct {
	net.Conn
	marker   []byte
	armed    atomic.Bool
	held     chan struct{}
	released chan struct{}
	release  func()
}

// A connection over conn whose first write of marker holds. The hold ends
// when the test does, at the latest.
func newHoldMarkedWriteConn(t *testing.T, conn net.Conn, marker []byte) *holdMarkedWriteConn {
	holdConn := &holdMarkedWriteConn{
		Conn:     conn,
		marker:   marker,
		held:     make(chan struct{}),
		released: make(chan struct{}),
	}
	holdConn.release = sync.OnceFunc(func() {
		close(holdConn.released)
	})
	holdConn.armed.Store(true)
	t.Cleanup(holdConn.release)
	return holdConn
}

// Holds the first write of the marker until released, then writes p.
func (self *holdMarkedWriteConn) Write(p []byte) (int, error) {
	if bytes.Contains(p, self.marker) && self.armed.CompareAndSwap(true, false) {
		close(self.held)
		<-self.released
	}
	return self.Conn.Write(p)
}

// Closes the connection, which ends a held write.
func (self *holdMarkedWriteConn) Close() error {
	self.release()
	return self.Conn.Close()
}

// Receives the next value from a channel, with a bound so a broken test
// fails rather than hangs.
func receiveBounded[T any](t *testing.T, values <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(30 * time.Second):
		t.Fatalf("no %s", what)
		var zero T
		return zero
	}
}

// Waits for a channel to close, with a bound so a broken test fails rather
// than hangs.
func waitClosed(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not happen", what)
	}
}

// Cancels a context when the test ends.
func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
