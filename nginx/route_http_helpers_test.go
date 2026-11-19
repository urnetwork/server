package nginx

// Fixtures for the front's tests: the shared route file fixture, and fakes
// of the parts newFront takes through interfaces, so routing, limits and
// responses are tested apart from s-tls's matcher and s-forward's upstreams.

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The shared fixture, parsed. Each call parses again, so a test may change
// the table before building a front from it.
func loadTestRouteTable(t *testing.T) *RouteTable {
	t.Helper()
	routeTable, _, err := LoadRouteTable("testdata/route_v1_example.json")
	if err != nil {
		t.Fatal(err)
	}
	return routeTable
}

// The fixture's service named name.
func testRouteService(t *testing.T, routeTable *RouteTable, name string) *RouteService {
	t.Helper()
	service, ok := routeTable.Service(name)
	if !ok {
		t.Fatalf("no service %s in the fixture", name)
	}
	return service
}

// The fixture's interface named name.
func testRouteInterface(t *testing.T, routeTable *RouteTable, name string) *RouteInterface {
	t.Helper()
	routeInterface, ok := routeTable.Interface(name)
	if !ok {
		t.Fatalf("no interface %s in the fixture", name)
	}
	return routeInterface
}

// Host normalization and name matching with nginx's semantics, standing in
// for s-tls's: lowercase, no port, one trailing dot removed; an exact name
// first, then the longest leading wildcard, which matches one or more
// labels.
type testFrontNames struct {
	exactNameMatches    map[string]nameMatch
	wildcardNameMatches map[string]nameMatch
}

// Indexes a table's served names.
func newTestFrontNames(routeTable *RouteTable) *testFrontNames {
	names := &testFrontNames{
		exactNameMatches:    map[string]nameMatch{},
		wildcardNameMatches: map[string]nameMatch{},
	}
	for _, name := range routeTable.Lb.Names {
		names.exactNameMatches[name] = nameMatch{
			service: nil,
			class:   NameClassLb,
		}
	}
	for _, service := range routeTable.Services {
		class := NameClassForwarded
		switch service.Name {
		case routeTable.Front.ConnectService:
			class = NameClassConnect
		case routeTable.Front.ApiService:
			class = NameClassApi
		}
		for _, name := range service.Names {
			nameMatch := nameMatch{
				service: service,
				class:   class,
			}
			if suffix, ok := strings.CutPrefix(name, "*."); ok {
				names.wildcardNameMatches[suffix] = nameMatch
			} else {
				names.exactNameMatches[name] = nameMatch
			}
		}
		for _, redirectAlias := range service.RedirectAliases {
			names.exactNameMatches[redirectAlias.Name] = nameMatch{
				service:        service,
				class:          NameClassRedirect,
				redirectTarget: redirectAlias.Target,
			}
		}
	}
	return names
}

// See frontNames.
func (self *testFrontNames) normalizeHost(host string) (string, bool) {
	host = strings.ToLower(host)
	if strings.HasPrefix(host, "[") {
		end := strings.Index(host, "]")
		if end < 0 {
			return "", false
		}
		host = host[:end+1]
	} else if i := strings.LastIndex(host, ":"); 0 <= i {
		host = host[:i]
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || strings.Contains(host, "..") || strings.ContainsAny(host, "/\\ \x00\t") {
		return "", false
	}
	return host, true
}

// See frontNames.
func (self *testFrontNames) match(name string) (nameMatch, bool) {
	if nameMatch, ok := self.exactNameMatches[name]; ok {
		return nameMatch, true
	}
	// the longest suffix first
	for i := 0; i < len(name); i += 1 {
		if name[i] != '.' {
			continue
		}
		if nameMatch, ok := self.wildcardNameMatches[name[i+1:]]; ok {
			return nameMatch, true
		}
	}
	return nameMatch{}, false
}

// One request a fake forwarder got.
type testForward struct {
	options forwardOptions
	method  string
	host    string
	header  http.Header
}

// A forwarder that records what it gets and answers 200 "forwarded", or as
// respond says.
type testServiceForwarder struct {
	respond func(w http.ResponseWriter, r *http.Request, options forwardOptions)
	// runs inside close, as a stuck upstream close would
	closeHook func()

	stateLock  sync.Mutex
	forwards   []*testForward
	closeCount int
}

// See serviceForwarder.
func (self *testServiceForwarder) serveHttp(w http.ResponseWriter, r *http.Request, options forwardOptions) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.forwards = append(self.forwards, &testForward{
			options: options,
			method:  r.Method,
			host:    r.Host,
			header:  r.Header.Clone(),
		})
	}()
	if self.respond != nil {
		self.respond(w, r, options)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, "forwarded")
}

// See serviceForwarder.
func (self *testServiceForwarder) close() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closeCount += 1
	}()
	if self.closeHook != nil {
		self.closeHook()
	}
}

// The forwards so far.
func (self *testServiceForwarder) recordedForwards() []*testForward {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*testForward{}, self.forwards...)
}

// The number of close calls so far.
func (self *testServiceForwarder) closes() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.closeCount
}

// One request the fake connect handler got.
type testConnectRequest struct {
	host       string
	path       string
	rawQuery   string
	requestUri string
	header     http.Header
}

// The in-process connect handler for tests: it records what it gets and
// answers 200 "connect", or as serve says.
type testConnectHandler struct {
	serve func(w http.ResponseWriter, r *http.Request)

	stateLock sync.Mutex
	requests  []*testConnectRequest
}

// See http.Handler.
func (self *testConnectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, &testConnectRequest{
			host:       r.Host,
			path:       r.URL.Path,
			rawQuery:   r.URL.RawQuery,
			requestUri: r.RequestURI,
			header:     r.Header.Clone(),
		})
	}()
	if self.serve != nil {
		self.serve(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, "connect")
}

// The requests so far.
func (self *testConnectHandler) recordedRequests() []*testConnectRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*testConnectRequest{}, self.requests...)
}

// A body store that counts its closes.
type testBodyStore struct {
	closeErr error

	stateLock  sync.Mutex
	closeCount int
}

// See frontBodyStore.
func (self *testBodyStore) close() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closeCount += 1
	return self.closeErr
}

// The number of close calls so far.
func (self *testBodyStore) closes() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.closeCount
}

// An access log that keeps its lines.
type testFrontAccessLog struct {
	stateLock sync.Mutex
	lines     []string
}

// See frontAccessLog.
func (self *testFrontAccessLog) enabled() bool {
	return true
}

// See frontAccessLog.
func (self *testFrontAccessLog) write(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

// The lines so far.
func (self *testFrontAccessLog) writtenLines() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.lines...)
}

// A clock tests move by hand.
type testClock struct {
	stateLock sync.Mutex
	time      time.Time
}

// A clock at a fixed instant.
func newTestClock() *testClock {
	return &testClock{
		time: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC),
	}
}

// The clock's time.
func (self *testClock) now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.time
}

// Moves the clock on.
func (self *testClock) advance(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.time = self.time.Add(duration)
}

// A front over fakes, with the fakes at hand.
type testFront struct {
	front          *Front
	routeTable     *RouteTable
	forwarders     map[string]*testServiceForwarder
	connectHandler *testConnectHandler
	bodyStore      *testBodyStore
	accessLog      *testFrontAccessLog
	metrics        *frontMetrics
	clock          *testClock
}

// Builds a front over fakes from a table and settings. It closes when the
// test ends.
func newTestFront(t *testing.T, routeTable *RouteTable, settings *FrontSettings) *testFront {
	t.Helper()
	testFront := &testFront{
		routeTable:     routeTable,
		forwarders:     map[string]*testServiceForwarder{},
		connectHandler: &testConnectHandler{},
		bodyStore:      &testBodyStore{},
		accessLog:      &testFrontAccessLog{},
		metrics:        newFrontMetrics(prometheus.NewRegistry()),
		clock:          newTestClock(),
	}
	serviceForwarders := map[string]serviceForwarder{}
	for _, service := range routeTable.Services {
		forwarder := &testServiceForwarder{}
		testFront.forwarders[service.Name] = forwarder
		serviceForwarders[service.Name] = forwarder
	}
	front, err := newFront(t.Context(), routeTable, "test-revision", &frontDependencies{
		names:             newTestFrontNames(routeTable),
		serviceForwarders: serviceForwarders,
		tlsConfig:         &tls.Config{},
		quicTlsConfig:     &tls.Config{},
		bodyStore:         testFront.bodyStore,
		metrics:           testFront.metrics,
		accessLog:         testFront.accessLog,
		now:               testFront.clock.now,
	}, testFront.connectHandler, "1.2.3", settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		front.Close()
	})
	testFront.front = front
	return testFront
}

// Front settings for tests: the defaults, with no sweep and every request
// logged.
func testFrontSettings() *FrontSettings {
	settings := DefaultFrontSettings()
	settings.Limit.PruneInterval = 0
	settings.Http.AccessLogSampleInterval = 1
	settings.Http.ForcedCloseTimeout = 10 * time.Second
	return settings
}

// A request for a handler: origin form, from a documentation address, over
// TLS when tls is set, as HTTP/1.1.
func newTestRequest(method string, host string, target string, overTls bool) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	r.RemoteAddr = "198.51.100.7:40001"
	if overTls {
		r.TLS = &tls.ConnectionState{
			ServerName: host,
		}
	} else {
		r.TLS = nil
	}
	return r
}

// The request as h2 or h3 sends it: no Connection, and the protocol set.
func asTestProtocol(r *http.Request, protoMajor int) *http.Request {
	r.ProtoMajor = protoMajor
	r.ProtoMinor = 0
	switch protoMajor {
	case 2:
		r.Proto = "HTTP/2.0"
	case 3:
		r.Proto = "HTTP/3.0"
	}
	return r
}

// Serves one request through a handler.
func serveTestRequest(handler http.Handler, r *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, r)
	return recorder
}

// A counter's value.
func testCounterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	return testutil.ToFloat64(counter)
}
