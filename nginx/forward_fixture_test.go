package nginx

// Fixtures for the forwarder's tests: recording upstream blocks on loopback,
// a front that hands each request to a group, and groups built from
// synthetic route tables (.example names, documentation addresses for
// clients, loopback for sockets).

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// One request as a recording upstream saw it.
type recordedRequest struct {
	block            string
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

func newRecordingUpstream(t *testing.T, name string, handler func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest)) *recordingUpstream {
	upstream := &recordingUpstream{
		t:       t,
		name:    name,
		handler: handler,
	}
	upstream.server = httptest.NewServer(http.HandlerFunc(upstream.serveHttp))
	t.Cleanup(upstream.server.Close)
	upstream.port = upstream.server.Listener.Addr().(*net.TCPAddr).Port
	return upstream
}

func (self *recordingUpstream) serveHttp(w http.ResponseWriter, r *http.Request) {
	body, bodyErr := io.ReadAll(r.Body)
	recorded := &recordedRequest{
		block:            self.name,
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
// on its own.
func testFrontSettings() *FrontSettings {
	settings := DefaultFrontSettings()
	settings.Forward.KeepaliveSweepInterval = 0
	return settings
}

// A service of the route table with the given blocks and nginx's default
// keepalive.
func testRouteService(serviceName string, testBlocks []testBlock) *RouteService {
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
type testFront struct {
	server  *httptest.Server
	group   *upstreamGroup
	options func(r *http.Request) forwardOptions
	served  chan any
}

func newTestFront(t *testing.T, group *upstreamGroup, options func(r *http.Request) forwardOptions) *testFront {
	front := &testFront{
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
		front.group.serveHttp(w, r, front.options(r))
	}))
	t.Cleanup(front.server.Close)
	return front
}

// The default options of a test request: a documentation ipv4 client.
func testForwardOptions(r *http.Request) forwardOptions {
	return forwardOptions{
		interfaceName:  "eth1",
		clientAddrPort: netip.MustParseAddrPort("192.0.2.10:40123"),
	}
}

// The front's url for a path.
func (self *testFront) url(path string) string {
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
func (self *testFront) waitServed(t *testing.T) any {
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

// Cancels a context when the test ends.
func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
