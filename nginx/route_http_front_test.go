package nginx

// Tests of the Front's assembly and lifecycle (front.go): its argument
// checks, ClassifyName, the cached per-interface handlers, the QUIC
// connection cap, Close and the drain deadline, and the metric labels.

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// NewFront refuses a missing route table, certificate store, connect
// handler or settings group before it builds anything.
func TestNewFrontRefusesMissingParts(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	certificateStore := &CertificateStore{}
	handler := &testConnectHandler{}
	missingGroup := DefaultFrontSettings()
	missingGroup.Limit = nil
	cases := []struct {
		name             string
		routeTable       *RouteTable
		certificateStore *CertificateStore
		connectHandler   http.Handler
		settings         *FrontSettings
	}{
		{name: "no route table", routeTable: nil, certificateStore: certificateStore, connectHandler: handler, settings: DefaultFrontSettings()},
		{name: "no certificate store", routeTable: routeTable, certificateStore: nil, connectHandler: handler, settings: DefaultFrontSettings()},
		{name: "no connect handler", routeTable: routeTable, certificateStore: certificateStore, connectHandler: nil, settings: DefaultFrontSettings()},
		{name: "no settings", routeTable: routeTable, certificateStore: certificateStore, connectHandler: handler, settings: nil},
		{name: "a missing group", routeTable: routeTable, certificateStore: certificateStore, connectHandler: handler, settings: missingGroup},
	}
	for _, c := range cases {
		front, err := NewFront(t.Context(), c.routeTable, "revision", c.certificateStore, c.connectHandler, "1.2.3", c.settings)
		if err == nil || front != nil {
			t.Errorf("%s: NewFront = (%v, %v), want an error", c.name, front, err)
		}
	}
}

// A front whose services lack a forwarder is refused.
func TestNewFrontNeedsAForwarderPerService(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	_, err := newFront(t.Context(), routeTable, "revision", &frontDependencies{
		names: newTestFrontNames(routeTable),
		serviceForwarders: map[string]serviceForwarder{
			"api": &testServiceForwarder{},
		},
		bodyStore: &testBodyStore{},
		metrics:   newFrontMetrics(prometheus.NewRegistry()),
		accessLog: &testFrontAccessLog{},
		now:       time.Now,
	}, &testConnectHandler{}, "1.2.3", testFrontSettings())
	if err == nil || !strings.Contains(err.Error(), "no forwarder") {
		t.Fatalf("newFront = %v, want the missing forwarder refused", err)
	}
}

// ClassifyName normalizes as nginx's $host and matches with nginx's
// server_name precedence.
func TestFrontClassifyName(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	cases := []struct {
		name      string
		nameClass NameClass
	}{
		{name: "test-lb.service.example", nameClass: NameClassLb},
		{name: "test-lb.other.example", nameClass: NameClassLb},
		{name: "CONNECT.service.example.", nameClass: NameClassConnect},
		{name: "x.y.connect.service.example", nameClass: NameClassConnect},
		{name: "api.service.example:443", nameClass: NameClassApi},
		{name: "mcp.service.example", nameClass: NameClassForwarded},
		{name: "old-api.service.example", nameClass: NameClassRedirect},
		{name: "unknown.example", nameClass: NameClassUnknown},
		{name: "connect.service.example.unknown.example", nameClass: NameClassUnknown},
		{name: "api..service.example", nameClass: NameClassUnknown},
		{name: "", nameClass: NameClassUnknown},
	}
	for _, c := range cases {
		if nameClass := testFront.front.ClassifyName(c.name); nameClass != c.nameClass {
			t.Errorf("ClassifyName(%q) = %s, want %s", c.name, nameClass, c.nameClass)
		}
	}
}

// The revision and the TLS configs are the ones the front was built with,
// the same on every call, and each interface's handlers are built once.
// An interface the table doesn't list gets a handler that refuses
// everything, and admits no QUIC connection.
func TestFrontServesItsBuiltPartsPerInterface(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	tlsConfig := &tls.Config{}
	quicTlsConfig := &tls.Config{}
	serviceForwarders := map[string]serviceForwarder{}
	for _, service := range routeTable.Services {
		serviceForwarders[service.Name] = &testServiceForwarder{}
	}
	metrics := newFrontMetrics(prometheus.NewRegistry())
	front, err := newFront(t.Context(), routeTable, "0123abcd", &frontDependencies{
		names:             newTestFrontNames(routeTable),
		serviceForwarders: serviceForwarders,
		tlsConfig:         tlsConfig,
		quicTlsConfig:     quicTlsConfig,
		bodyStore:         &testBodyStore{},
		metrics:           metrics,
		accessLog:         &testFrontAccessLog{},
		now:               time.Now,
	}, &testConnectHandler{}, "1.2.3", testFrontSettings())
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()

	if front.Revision() != "0123abcd" {
		t.Errorf("Revision() = %q", front.Revision())
	}
	if front.TlsConfig() != tlsConfig || front.TlsConfig() != front.TlsConfig() || front.QuicTlsConfig() != quicTlsConfig {
		t.Error("the TLS configs are not the long-lived ones the front was built with")
	}
	if front.HttpsHandler("eth1") != front.HttpsHandler("eth1") || front.HttpHandler("eth1") != front.HttpHandler("eth1") || front.H3ApiHandler("eth1") != front.H3ApiHandler("eth1") {
		t.Error("a handler was built twice for one interface")
	}
	if front.HttpsHandler("eth1") == front.HttpsHandler("eth2") {
		t.Error("two interfaces share a handler")
	}
	for _, handler := range []http.Handler{front.HttpsHandler("eth9"), front.HttpHandler("eth9"), front.H3ApiHandler("eth9")} {
		response := serveTestRequest(handler, newTestRequest("GET", "test-lb.service.example", "/hidden-example/status", true))
		if response.Code != http.StatusServiceUnavailable {
			t.Errorf("an unknown interface's handler = %d, want 503", response.Code)
		}
	}
	if release, ok := front.AcquireQuicConnection("eth9", netip.MustParseAddr("198.51.100.7")); ok || release != nil {
		t.Error("an unknown interface admitted a QUIC connection")
	}
	if count := testCounterValue(t, metrics.limitRefusals.WithLabelValues("interface", routeHttpNoLabel)); count != 1 {
		t.Errorf("%v unknown interface refusals counted, want 1", count)
	}
}

// A hijack after the cut started can't escape it: the connection is closed
// at once, its slot is let go, and the handler gets an error.
func TestFrontHijackAfterTheCutIsClosed(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	setTestRateLimit(t, routeTable, "eth1", 0, 0, 1)
	testFront := newTestFront(t, routeTable, testFrontSettings())
	hijackErrs := []error{}
	testFront.connectHandler.serve = func(w http.ResponseWriter, r *http.Request) {
		_, _, err := w.(http.Hijacker).Hijack()
		hijackErrs = append(hijackErrs, err)
	}
	testFront.front.cut()

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	testFront.front.HttpsHandler("eth1").ServeHTTP(&testHijackResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             serverConn,
	}, newTestRequestFrom("198.51.100.91:1", "GET", "connect.service.example", "/", true))
	if len(hijackErrs) != 1 || !errors.Is(hijackErrs[0], errResponseFrontCut) {
		t.Fatalf("hijack errors %v, want the cut's", hijackErrs)
	}
	if _, err := clientConn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the client end read %v, want EOF", err)
	}
	if count := testFront.front.hijackedConnCount(); count != 0 {
		t.Fatalf("%d tracked tunnels, want 0", count)
	}
	response := serveTestRequest(testFront.front.HttpsHandler("eth1"), newTestRequestFrom("198.51.100.91:1", "GET", "api.service.example", "/v1", true))
	if response.Code != http.StatusOK {
		t.Fatalf("after the refused hijack = %d, want 200: its slot was let go", response.Code)
	}
}

// AcquireQuicConnection caps QUIC connections per (interface, address) at
// the interface's net_connections, apart from HTTP requests; a release frees
// one; exclude_subnets and a zero cap admit everything.
func TestFrontAcquireQuicConnection(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	setTestRateLimit(t, routeTable, "eth1", 1, 0, 2, "203.0.113.0/24")
	setTestRateLimit(t, routeTable, "eth2", 1, 0, 0)
	testFront := newTestFront(t, routeTable, testFrontSettings())
	front := testFront.front
	addr := netip.MustParseAddr("2001:db8::81")

	releases := []func(){}
	for i := range 2 {
		release, ok := front.AcquireQuicConnection("eth1", addr)
		if !ok {
			t.Fatalf("connection %d refused", i)
		}
		releases = append(releases, release)
	}
	if release, ok := front.AcquireQuicConnection("eth1", addr); ok || release != nil {
		t.Fatal("a third connection was admitted")
	}
	if count := testCounterValue(t, testFront.metrics.limitRefusals.WithLabelValues("quic", limitDefaultZone)); count != 1 {
		t.Errorf("%v quic refusals counted, want 1", count)
	}
	// another interface is another quota, and HTTP isn't touched
	if _, ok := front.AcquireQuicConnection("eth2", addr); !ok {
		t.Error("eth2 refused the same address")
	}
	r := newTestRequestFrom("[2001:db8::81]:1", "GET", "api.service.example", "/v1", true)
	if response := serveTestRequest(front.HttpsHandler("eth1"), r); response.Code != http.StatusOK {
		t.Errorf("an HTTP request with two QUIC connections open = %d, want 200", response.Code)
	}
	releases[0]()
	release, ok := front.AcquireQuicConnection("eth1", addr)
	if !ok {
		t.Fatal("refused after a release")
	}
	release()
	releases[1]()
	for i := range 5 {
		if _, ok := front.AcquireQuicConnection("eth1", netip.MustParseAddr("203.0.113.8")); !ok {
			t.Fatalf("excluded connection %d refused", i)
		}
	}
}

// Close releases every forwarder and the body store once, returns the
// body store's error, answers later requests with 503, and returns the same
// result again.
func TestFrontCloseReleasesForwardersAndTheBodyStore(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	testFront.bodyStore.closeErr = errors.New("spill directory gone")
	if err := testFront.front.Close(); err == nil || !strings.Contains(err.Error(), "spill directory gone") {
		t.Fatalf("Close() = %v, want the body store's error", err)
	}
	for name, forwarder := range testFront.forwarders {
		if closes := forwarder.closes(); closes != 1 {
			t.Errorf("%s forwarder closed %d times, want 1", name, closes)
		}
	}
	if closes := testFront.bodyStore.closes(); closes != 1 {
		t.Errorf("body store closed %d times, want 1", closes)
	}
	if err := testFront.front.Close(); err == nil {
		t.Error("a second Close() lost the result")
	}
	if closes := testFront.bodyStore.closes(); closes != 1 {
		t.Errorf("a second Close() closed the body store again: %d", closes)
	}
	response := serveTestRequest(testFront.front.HttpsHandler("eth1"), newTestRequest("GET", "api.service.example", "/v1", true))
	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("a request after Close = %d, want 503", response.Code)
	}
}

// Close waits for the forced close at most ForcedCloseTimeout: a forwarder
// stuck in its close can't hold the exit.
func TestFrontCloseIsBoundedByTheForcedCloseTimeout(t *testing.T) {
	settings := testFrontSettings()
	settings.Http.ForcedCloseTimeout = 50 * time.Millisecond
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	stuck := make(chan struct{})
	defer close(stuck)
	testFront.forwarders["api"].closeHook = func() {
		<-stuck
	}
	err := testFront.front.Close()
	if err == nil || !strings.Contains(err.Error(), "did not complete") {
		t.Fatalf("Close() with a stuck forwarder = %v, want the bound's error", err)
	}
}

// Close cuts a hijacked connection still open, which lets go of its slot.
func TestFrontCloseCutsHijackedConnections(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	testFront.connectHandler.serve = func(w http.ResponseWriter, r *http.Request) {
		w.(http.Hijacker).Hijack()
	}
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	testFront.front.HttpsHandler("eth1").ServeHTTP(&testHijackResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             serverConn,
	}, newTestRequest("GET", "connect.service.example", "/", true))
	if count := testFront.front.hijackedConnCount(); count != 1 {
		t.Fatalf("%d tracked tunnels, want 1", count)
	}

	if err := testFront.front.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := clientConn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("the client end read %v, want EOF from the cut", err)
	}
	if count := testFront.front.hijackedConnCount(); count != 0 {
		t.Fatalf("%d tracked tunnels after Close, want 0", count)
	}
}

// After BeginDrain, the drain deadline cuts what outlived the drain on its
// own: the hijacked connections, the forwarders and the body store.
func TestFrontDrainDeadlineCutsWhatOutlivedTheDrain(t *testing.T) {
	settings := testFrontSettings()
	settings.Http.DrainTimeout = time.Nanosecond
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	testFront.connectHandler.serve = func(w http.ResponseWriter, r *http.Request) {
		w.(http.Hijacker).Hijack()
	}
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	testFront.front.HttpsHandler("eth1").ServeHTTP(&testHijackResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             serverConn,
	}, newTestRequest("GET", "connect.service.example", "/", true))

	testFront.front.BeginDrain()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case <-testFront.front.cutDone:
	case <-ctx.Done():
		t.Fatal("the drain deadline never cut")
	}
	if _, err := clientConn.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("the tunnel read %v, want EOF from the cut", err)
	}
	for name, forwarder := range testFront.forwarders {
		if closes := forwarder.closes(); closes != 1 {
			t.Errorf("%s forwarder closed %d times, want 1", name, closes)
		}
	}
	if closes := testFront.bodyStore.closes(); closes != 1 {
		t.Errorf("body store closed %d times, want 1", closes)
	}
}

// The request counter's labels: the service the request is for, the front's
// own answers on its names included, and the block that answered, as the
// forwarder names it or as the location fixes it, only from the route table;
// "local" for connect in-process; "-" for the lb's own routes and for a
// block the service doesn't have.
func TestFrontMetricsLabelsAreBounded(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		recorder, ok := w.(upstreamBlockRecorder)
		if !ok {
			t.Fatal("the forwarder's writer can't record the block")
		}
		recorder.recordUpstreamBlock(strings.TrimPrefix(options.upstreamPath, "/"))
		w.WriteHeader(http.StatusOK)
	}
	handler := testFront.front.HttpsHandler("eth1")
	serveTestRequest(handler, newTestRequest("GET", "api.service.example", "/g2", true))
	serveTestRequest(handler, newTestRequest("GET", "api.service.example", "/not-a-block", true))
	serveTestRequest(handler, asTestProtocol(newTestRequest("GET", "api.service.example", "/g1", true), 2))
	serveTestRequest(handler, newTestRequest("GET", "connect.service.example", "/", true))
	serveTestRequest(handler, newTestRequest("GET", "test-lb.service.example", "/hidden-example/status", true))
	serveTestRequest(handler, newTestRequest("GET", "test-lb.service.example", "/nope", true))
	// a per-block lb route names its block, whatever the forwarder records
	serveTestRequest(handler, newTestRequest("GET", "test-lb.service.example", "/hidden-example/by/b/mcp/g1/x", true))
	// the front's own answer on a service's names is that service's
	serveTestRequest(handler, newTestRequest("GET", "api.service.example", "/status", true))

	for _, c := range []struct {
		labels []string
		count  float64
	}{
		{labels: []string{"api", "h1", "2xx", "g2"}, count: 1},
		{labels: []string{"api", "h1", "2xx", "-"}, count: 1},
		{labels: []string{"api", "h2", "2xx", "g1"}, count: 1},
		{labels: []string{"connect", "h1", "2xx", "local"}, count: 1},
		{labels: []string{"-", "h1", "2xx", "-"}, count: 1},
		{labels: []string{"-", "h1", "4xx", "-"}, count: 1},
		{labels: []string{"mcp", "h1", "2xx", "g1"}, count: 1},
		{labels: []string{"api", "h1", "4xx", "-"}, count: 1},
	} {
		if count := testCounterValue(t, testFront.metrics.requests.WithLabelValues(c.labels...)); count != c.count {
			t.Errorf("requests_total%v = %v, want %v", c.labels, count, c.count)
		}
	}
}

// The status class and protocol labels, each one of a closed set of
// constants.
func TestRouteHttpStatusClassAndProtocolLabels(t *testing.T) {
	for _, c := range []struct {
		statusCode  int
		statusClass string
	}{
		{statusCode: 0, statusClass: "none"},
		{statusCode: 100, statusClass: "1xx"},
		{statusCode: 199, statusClass: "1xx"},
		{statusCode: 200, statusClass: "2xx"},
		{statusCode: 301, statusClass: "3xx"},
		{statusCode: 404, statusClass: "4xx"},
		{statusCode: 503, statusClass: "5xx"},
		{statusCode: 599, statusClass: "5xx"},
		{statusCode: 600, statusClass: "other"},
		{statusCode: 99, statusClass: "other"},
		{statusCode: -1, statusClass: "other"},
	} {
		if statusClass := routeHttpStatusClasses[routeHttpStatusClassIndex(c.statusCode)]; statusClass != c.statusClass {
			t.Errorf("the status class of %d = %q, want %q", c.statusCode, statusClass, c.statusClass)
		}
	}
	for _, c := range []struct {
		protoMajor int
		protocol   string
	}{
		{protoMajor: 0, protocol: "other"},
		{protoMajor: 1, protocol: "h1"},
		{protoMajor: 2, protocol: "h2"},
		{protoMajor: 3, protocol: "h3"},
		{protoMajor: 4, protocol: "other"},
		{protoMajor: -1, protocol: "other"},
	} {
		if protocol := routeHttpProtocols[routeHttpProtocolIndex(c.protoMajor)]; protocol != c.protocol {
			t.Errorf("the protocol of major version %d = %q, want %q", c.protoMajor, protocol, c.protocol)
		}
	}
}

// An ended request records its metrics through the series kept for its
// label set, the vectors' own series, each resolved once: requests ending
// at once on label sets not resolved yet, the same and different ones,
// count exactly, and once resolved a request's metrics allocate nothing.
func TestFrontObserveKeepsEachLabelSetsSeries(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	front := testFront.front
	// the access log is the production one, off below V(1)
	front.accessLog = &testDisabledFrontAccessLog{}
	now := testFront.clock.now()
	newExchange := func(serviceName string, upstreamBlock string, protoMajor int, statusCode int) *routeHttpExchange {
		r := httptest.NewRequest(http.MethodGet, "/v1", nil)
		r.ProtoMajor = protoMajor
		return &routeHttpExchange{
			front:          front,
			frontInterface: front.interfaces["eth1"],
			r:              r,
			response: &responseState{
				front:         front,
				statusCode:    statusCode,
				headerTime:    now.Add(time.Millisecond),
				upstreamBlock: upstreamBlock,
			},
			startTime:   now,
			serviceName: serviceName,
		}
	}
	exchanges := []*routeHttpExchange{
		newExchange("api", "g1", 2, http.StatusOK),
		newExchange("api", "g1", 2, http.StatusOK),
		newExchange("api", "g2", 1, http.StatusNotFound),
		newExchange("api", "not-a-block", 3, http.StatusServiceUnavailable),
		newExchange("connect", routeHttpInProcessBlock, 1, http.StatusSwitchingProtocols),
		newExchange("not-a-service", "g1", 1, 0),
	}
	const observations = 100
	start := make(chan struct{})
	var observers sync.WaitGroup
	for _, exchange := range exchanges {
		observers.Go(func() {
			<-start
			for range observations {
				front.observe(exchange)
			}
		})
	}
	close(start)
	observers.Wait()
	for _, c := range []struct {
		labels []string
		count  float64
	}{
		{labels: []string{"api", "h2", "2xx", "g1"}, count: 2 * observations},
		{labels: []string{"api", "h1", "4xx", "g2"}, count: observations},
		{labels: []string{"api", "h3", "5xx", "-"}, count: observations},
		{labels: []string{"connect", "h1", "1xx", "local"}, count: observations},
		{labels: []string{"-", "h1", "none", "-"}, count: observations},
	} {
		if count := testCounterValue(t, testFront.metrics.requests.WithLabelValues(c.labels...)); count != c.count {
			t.Errorf("requests_total%v = %v, want %v", c.labels, count, c.count)
		}
		if count, _ := testResponseHeaderSeconds(t, testFront.metrics, c.labels...); float64(count) != c.count {
			t.Errorf("response_header_seconds%v count = %v, want %v", c.labels, count, c.count)
		}
	}
	// AllocsPerRun counts the whole process's allocations, so another
	// goroutine, such as an earlier test's teardown, can only add to one
	// measurement: the least of a few is the request's own
	leastAllocs := math.Inf(1)
	for range 5 {
		leastAllocs = min(leastAllocs, testing.AllocsPerRun(1000, func() {
			front.observe(exchanges[0])
		}))
	}
	if leastAllocs != 0 {
		t.Errorf("a request's metrics allocate %v times, want none", leastAllocs)
	}
}

// The count and sum of one label set of the latency histogram.
func testResponseHeaderSeconds(t *testing.T, metrics *frontMetrics, labels ...string) (uint64, float64) {
	t.Helper()
	metric := &dto.Metric{}
	if err := metrics.responseHeaderSeconds.WithLabelValues(labels...).(prometheus.Histogram).Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetHistogram().GetSampleCount(), metric.GetHistogram().GetSampleSum()
}

// D17's latency histogram observes every request once, under the requests
// counter's bounded labels (service, protocol, status class and block),
// from the request's arrival to its response head, or to the hijack of an
// upgrade: time before the head counts, and a body or a tunnel after it
// doesn't.
func TestFrontResponseHeaderSecondsTimesEachRequestToItsHead(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		w.(upstreamBlockRecorder).recordUpstreamBlock("g2")
		testFront.clock.advance(3 * time.Second)
		w.WriteHeader(http.StatusOK)
		testFront.clock.advance(time.Minute)
		io.WriteString(w, "body")
	}
	testFront.connectHandler.serve = func(w http.ResponseWriter, r *http.Request) {
		testFront.clock.advance(2 * time.Second)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		testFront.clock.advance(time.Minute)
		conn.Close()
	}
	handler := testFront.front.HttpsHandler("eth1")
	serveTestRequest(handler, newTestRequest("GET", "api.service.example", "/v1", true))
	serveTestRequest(handler, asTestProtocol(newTestRequest("GET", "test-lb.service.example", "/nope", true), 2))
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	handler.ServeHTTP(&testHijackResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             serverConn,
	}, newTestRequest("GET", "connect.service.example", "/", true))

	for _, c := range []struct {
		labels []string
		sum    float64
	}{
		{labels: []string{"api", "h1", "2xx", "g2"}, sum: 3},
		{labels: []string{"-", "h2", "4xx", "-"}, sum: 0},
		{labels: []string{"connect", "h1", "1xx", "local"}, sum: 2},
	} {
		count, sum := testResponseHeaderSeconds(t, testFront.metrics, c.labels...)
		if count != 1 || sum != c.sum {
			t.Errorf("response_header_seconds%v: %d observations summing %v s, want 1 of %v s", c.labels, count, sum, c.sum)
		}
	}
}
