package nginx

// Tests of the Front's assembly and lifecycle (front.go): its argument
// checks, ClassifyName, the cached per-interface handlers, the QUIC
// connection cap, Close and the drain deadline, and the metric labels.

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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
	front, err := newFront(t.Context(), routeTable, "0123abcd", &frontDependencies{
		names:             newTestFrontNames(routeTable),
		serviceForwarders: serviceForwarders,
		tlsConfig:         tlsConfig,
		quicTlsConfig:     quicTlsConfig,
		bodyStore:         &testBodyStore{},
		metrics:           newFrontMetrics(prometheus.NewRegistry()),
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
	if count := testCounterValue(t, testFront.metrics.limitRefusals.WithLabelValues("quic")); count != 1 {
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

// The request counter's labels: the routed service and the block that
// answered, as the forwarder names it or as the location fixes it, only
// from the route table; "local" for connect in-process; "-" for the front's
// own answers and for a block the service doesn't have.
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
	} {
		if count := testCounterValue(t, testFront.metrics.requests.WithLabelValues(c.labels...)); count != c.count {
			t.Errorf("requests_total%v = %v, want %v", c.labels, count, c.count)
		}
	}
}
