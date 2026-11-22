package nginx

// Benchmarks of the front's per-request allocations (LBALTOPT1.md R6): the
// status class label, the request metrics Front.observe records, the
// upstream request head, and one request through the front's TCP 443
// handler to a forwarder that answers at once. Each reports its allocations
// (allocs/op, B/op). The front has the default settings and the production
// name matcher over the route fixture, with its limits charged and the
// access log off, as production runs it below V(1); its forwarders, body
// store and TLS configs are fakes.

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// A response writer that keeps one header map and discards the body, so a
// benchmark measures the front's allocations, not a recorder's.
type benchmarkResponseWriter struct {
	header     http.Header
	statusCode int
}

// See http.ResponseWriter.
func (self *benchmarkResponseWriter) Header() http.Header {
	return self.header
}

// See http.ResponseWriter.
func (self *benchmarkResponseWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

// See http.ResponseWriter.
func (self *benchmarkResponseWriter) WriteHeader(statusCode int) {
	self.statusCode = statusCode
}

// A forwarder that answers 200 at once and records nothing.
type benchmarkServiceForwarder struct{}

// See serviceForwarder.
func (self *benchmarkServiceForwarder) serveHttp(w http.ResponseWriter, r *http.Request, options forwardOptions) {
	if recorder, ok := w.(upstreamBlockRecorder); ok {
		recorder.recordUpstreamBlock("g1")
	}
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, "ok")
}

// See serviceForwarder.
func (self *benchmarkServiceForwarder) close() {}

// A front over the route fixture, with forwarders that answer at once, its
// own metrics registry, and eth1's request limit high enough that no
// benchmark request is refused, though each is charged. It closes when the
// benchmark ends.
func newBenchmarkFront(b *testing.B) *Front {
	b.Helper()
	routeTable, _, err := LoadRouteTable("testdata/route_v1_example.json")
	if err != nil {
		b.Fatal(err)
	}
	routeInterface, ok := routeTable.Interface("eth1")
	if !ok {
		b.Fatal("no eth1 in the route fixture")
	}
	routeInterface.RateLimit.RequestsPerMinute = 1000 * 1000 * 1000
	routeInterface.RateLimit.Burst = 1000 * 1000
	serviceForwarders := map[string]serviceForwarder{}
	for _, service := range routeTable.Services {
		serviceForwarders[service.Name] = &benchmarkServiceForwarder{}
	}
	settings := DefaultFrontSettings()
	settings.Limit.PruneInterval = 0
	front, err := newFront(b.Context(), routeTable, "benchmark-revision", &frontDependencies{
		names: &matcherFrontNames{
			matcher: newNameMatcher(routeTable),
		},
		serviceForwarders: serviceForwarders,
		tlsConfig:         &tls.Config{},
		quicTlsConfig:     &tls.Config{},
		bodyStore:         &testBodyStore{},
		metrics:           newFrontMetrics(prometheus.NewRegistry()),
		accessLog:         &testDisabledFrontAccessLog{},
		now:               time.Now,
	}, &testConnectHandler{}, "1.2.3", settings)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		front.Close()
	})
	return front
}

// The status class label of a response with a head.
func BenchmarkRouteHttpStatusClass(b *testing.B) {
	b.ReportAllocs()
	statusCodes := []int{http.StatusOK, http.StatusNotFound, http.StatusSwitchingProtocols, http.StatusServiceUnavailable}
	i := 0
	statusClass := ""
	for b.Loop() {
		statusClass = routeHttpStatusClasses[routeHttpStatusClassIndex(statusCodes[i%len(statusCodes)])]
		i += 1
	}
	if statusClass == "" {
		b.Fatal("no status class")
	}
}

// The metrics of one ended request: its counter and its response head's
// latency, by service, protocol, status class and block.
func BenchmarkFrontObserve(b *testing.B) {
	front := newBenchmarkFront(b)
	r := httptest.NewRequest(http.MethodGet, "/v1", nil)
	r.ProtoMajor = 2
	now := time.Now()
	exchange := &routeHttpExchange{
		front:          front,
		frontInterface: front.interfaces["eth1"],
		r:              r,
		response: &responseState{
			front:         front,
			statusCode:    http.StatusOK,
			headerTime:    now.Add(time.Millisecond),
			upstreamBlock: "g1",
		},
		startTime:   now,
		serviceName: "api",
	}
	b.ReportAllocs()
	for b.Loop() {
		front.observe(exchange)
	}
}

// A typical api request's upstream head: a POST with a body of known
// length and a few client headers, from a documentation address.
func newBenchmarkForwardRequest() (*http.Request, *forwardOptions, forwardBodyFraming) {
	r := httptest.NewRequest(http.MethodPost, "https://api.service.example/v1/example", nil)
	r.Header.Set("Accept", "*/*")
	r.Header.Set("Accept-Encoding", "gzip")
	r.Header.Set("Authorization", "Bearer synthetic-benchmark-token")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Content-Length", "512")
	r.Header.Set("User-Agent", "benchmark-client/1.0")
	r.Header.Set("X-Example-Version", "2026.11.20")
	options := &forwardOptions{
		interfaceName:  "eth1",
		clientAddrPort: netip.MustParseAddrPort("198.51.100.7:40123"),
		host:           "api.service.example",
		upstreamPath:   "/v1/example",
	}
	return r, options, forwardBodyFraming{known: true, length: 512}
}

// One upstream request head, as serveHttp builds it for each request: in a
// buffer from its group's pool, which goes back once the request is done.
func BenchmarkForwardRequestHead(b *testing.B) {
	group := newUpstreamGroup(testRouteTable(), &RouteService{
		Name:      "benchmark",
		Keepalive: &RouteKeepalive{},
	}, nil, DefaultFrontSettings())
	b.Cleanup(group.close)
	r, options, bodyFraming := newBenchmarkForwardRequest()
	b.ReportAllocs()
	for b.Loop() {
		requestHead := group.requestHeads.Get().(*[]byte)
		forwardRequestHead((*requestHead)[:0], r, options, false, bodyFraming)
		group.requestHeads.Put(requestHead)
	}
}

// One h2 GET for an api name through the front's TCP 443 handler: routing,
// the limits, the response's headers and the request's metrics, to a
// forwarder that answers at once.
func BenchmarkFrontServeForwardedRequest(b *testing.B) {
	front := newBenchmarkFront(b)
	handler := front.HttpsHandler("eth1")
	r := httptest.NewRequest(http.MethodGet, "https://api.service.example/v1/example", nil)
	r.Proto = "HTTP/2.0"
	r.ProtoMajor = 2
	r.ProtoMinor = 0
	r.RemoteAddr = "198.51.100.7:40123"
	r.TLS = &tls.ConnectionState{
		ServerName: "api.service.example",
	}
	w := &benchmarkResponseWriter{
		header: http.Header{},
	}
	b.ReportAllocs()
	for b.Loop() {
		clear(w.header)
		handler.ServeHTTP(w, r)
		if w.statusCode != http.StatusOK {
			b.Fatalf("the request = %d, want 200", w.statusCode)
		}
	}
}
