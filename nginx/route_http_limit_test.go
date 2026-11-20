package nginx

// Tests of the D8 limits as the handlers apply them: nginx's phase order,
// the lb status exemptions, route zones on service names only, the zone each
// refusal is counted in, one source across two interfaces, exclusions, every
// h2 and h3 stream counted, an upgraded connection holding its slot until
// its tunnel closes, and the body cap.

import (
	"bufio"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Sets an interface's limits.
func setTestRateLimit(t *testing.T, routeTable *RouteTable, interfaceName string, requestsPerMinute float64, burst int, netConnections int, excludeSubnets ...string) {
	t.Helper()
	testRouteInterface(t, routeTable, interfaceName).RateLimit = &RouteRateLimit{
		RequestsPerMinute: requestsPerMinute,
		Burst:             burst,
		NetConnections:    netConnections,
		ExcludeSubnets:    excludeSubnets,
	}
}

// A request from a given peer.
func newTestRequestFrom(remoteAddr string, method string, host string, target string, overTls bool) *http.Request {
	r := newTestRequest(method, host, target, overTls)
	r.RemoteAddr = remoteAddr
	return r
}

// nginx's phases: the server's and the location's returns (the redirects)
// and the CORS preflight run before the limits, so they are never limited
// or charged; the deny and an unmatched path run after, so they are
// charged.
func TestRouteHttpLimitsFollowNginxPhaseOrder(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	// one request per address, then refusals for a minute
	setTestRateLimit(t, routeTable, "eth1", 1, 0, 0)
	testFront := newTestFront(t, routeTable, testFrontSettings())
	https := testFront.front.HttpsHandler("eth1")
	http80 := testFront.front.HttpHandler("eth1")

	const addr = "198.51.100.11:1000"
	for range 3 {
		if response := serveTestRequest(https, newTestRequestFrom(addr, "GET", "old-api.service.example", "/", true)); response.Code != http.StatusMovedPermanently {
			t.Fatalf("redirect alias = %d, want 301", response.Code)
		}
		if response := serveTestRequest(http80, newTestRequestFrom(addr, "GET", "api.service.example", "/", false)); response.Code != http.StatusMovedPermanently {
			t.Fatalf("port 80 service name = %d, want 301", response.Code)
		}
		if response := serveTestRequest(http80, newTestRequestFrom(addr, "GET", "test-lb.service.example", "/x", false)); response.Code != http.StatusMovedPermanently {
			t.Fatalf("port 80 lb name = %d, want 301", response.Code)
		}
		preflight := newTestRequestFrom(addr, "OPTIONS", "api.service.example", "/v1", true)
		preflight.Header.Set("Origin", "https://app.service.example")
		if response := serveTestRequest(https, preflight); response.Code != http.StatusNoContent {
			t.Fatalf("preflight = %d, want 204", response.Code)
		}
	}
	// none of those took the address's one request
	if response := serveTestRequest(https, newTestRequestFrom(addr, "GET", "api.service.example", "/v1", true)); response.Code != http.StatusOK {
		t.Fatalf("the first limited request = %d, want 200", response.Code)
	}
	response := serveTestRequest(https, newTestRequestFrom(addr, "GET", "api.service.example", "/v1", true))
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("the second = %d, want 429", response.Code)
	}
	// nginx keeps the connection after its 429, as after a 403 or a 404
	if values := response.Header().Values("Connection"); len(values) != 0 {
		t.Errorf("429: Connection %q, want none while not draining", values)
	}

	charged := []struct {
		name       string
		handler    http.Handler
		request    *http.Request
		statusCode int
	}{
		{name: "the /status deny", handler: https, request: newTestRequestFrom("198.51.100.12:1000", "GET", "api.service.example", "/status", true), statusCode: http.StatusForbidden},
		{name: "an unmatched lb path", handler: https, request: newTestRequestFrom("198.51.100.13:1000", "GET", "test-lb.service.example", "/nope", true), statusCode: http.StatusNotFound},
		{name: "port 80's deny", handler: http80, request: newTestRequestFrom("198.51.100.14:1000", "GET", "unknown.example", "/", false), statusCode: http.StatusForbidden},
	}
	for _, c := range charged {
		response := serveTestRequest(c.handler, c.request)
		if response.Code != c.statusCode {
			t.Fatalf("%s = %d, want %d", c.name, response.Code, c.statusCode)
		}
		if values := response.Header().Values("Connection"); len(values) != 0 {
			t.Errorf("%s: Connection %q, want none while not draining", c.name, values)
		}
		next := newTestRequestFrom(c.request.RemoteAddr, "GET", "api.service.example", "/v1", true)
		if response := serveTestRequest(https, next); response.Code != http.StatusTooManyRequests {
			t.Errorf("after %s, a request = %d, want 429: it was charged", c.name, response.Code)
		}
	}
	if count := testCounterValue(t, testFront.metrics.limitRefusals.WithLabelValues("request", limitDefaultZone, "api")); count != 4 {
		t.Errorf("%v request refusals counted, want 4", count)
	}
}

// On an lb name, the lb status paths skip both limits, as nginx's empty
// $limit_key does: p/status, and p/by/service/<svc>/status and
// p/by/b/<svc>/<block>/status of lb_exposed standard-status services, on
// TCP 443 and 80. The same paths on another name, and the status paths of
// other services, are limited.
func TestRouteHttpLbStatusPathsSkipTheLimits(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	setTestRateLimit(t, routeTable, "eth1", 1, 0, 1)
	testRouteService(t, routeTable, "mcp").Status = "no"
	testFront := newTestFront(t, routeTable, testFrontSettings())
	https := testFront.front.HttpsHandler("eth1")
	http80 := testFront.front.HttpHandler("eth1")

	const addr = "198.51.100.21:1000"
	exempt := []struct {
		handler http.Handler
		overTls bool
		target  string
	}{
		{handler: https, overTls: true, target: "/hidden-example/status"},
		{handler: https, overTls: true, target: "/hidden-example/by/service/api/status"},
		{handler: https, overTls: true, target: "/hidden-example/by/b/api/g1/status"},
		{handler: https, overTls: true, target: "/hidden-example/by/b/connect/beta/status"},
		{handler: http80, overTls: false, target: "/hidden-example/status"},
		{handler: http80, overTls: false, target: "/hidden-example/by/b/api/g2/status"},
	}
	for _, c := range exempt {
		for i := range 5 {
			response := serveTestRequest(c.handler, newTestRequestFrom(addr, "GET", "test-lb.service.example", c.target, c.overTls))
			if response.Code != http.StatusOK {
				t.Fatalf("%s request %d = %d, want 200: exempt", c.target, i, response.Code)
			}
		}
	}
	if response := serveTestRequest(https, newTestRequestFrom(addr, "GET", "test-lb.service.example", "/hidden-example/by/service/api/other", true)); response.Code != http.StatusOK {
		t.Fatalf("a limited lb path = %d, want 200: the exempt paths took nothing", response.Code)
	}
	if response := serveTestRequest(https, newTestRequestFrom(addr, "GET", "test-lb.service.example", "/hidden-example/by/service/api/other", true)); response.Code != http.StatusTooManyRequests {
		t.Fatalf("again = %d, want 429", response.Code)
	}

	limited := []struct {
		name    string
		request *http.Request
	}{
		{name: "a service whose status isn't standard", request: newTestRequestFrom("198.51.100.22:1000", "GET", "test-lb.service.example", "/hidden-example/by/service/mcp/status", true)},
		{name: "the lb status on another name", request: newTestRequestFrom("198.51.100.23:1000", "GET", "unknown.example", "/hidden-example/status", true)},
		{name: "a near miss", request: newTestRequestFrom("198.51.100.24:1000", "GET", "test-lb.service.example", "/hidden-example/status/", true)},
	}
	for _, c := range limited {
		serveTestRequest(https, c.request)
		again := newTestRequestFrom(c.request.RemoteAddr, c.request.Method, c.request.Host, c.request.RequestURI, true)
		if response := serveTestRequest(https, again); response.Code != http.StatusTooManyRequests {
			t.Errorf("%s, twice = %d, want 429", c.name, response.Code)
		}
	}

	// HTTP/1.0 with no host has the default name as $host, an lb name
	for i := range 3 {
		r := newTestRequestFrom("198.51.100.25:1000", "GET", "", "/hidden-example/status", true)
		r.Proto = "HTTP/1.0"
		r.ProtoMinor = 0
		if response := serveTestRequest(https, r); response.Code != http.StatusOK {
			t.Fatalf("no host, request %d = %d, want 200: exempt", i, response.Code)
		}
	}

	// nginx's map lowercases its keys and what it looks up, so a case
	// variant of an exempt path skips the limits too, though no location,
	// which matches with case, serves it: a 404 each time, never a 429
	for _, target := range []string{"/HIDDEN-EXAMPLE/STATUS", "/hidden-example/BY/B/api/g1/status", "/Hidden-Example/By/Service/API/Status"} {
		for i := range 3 {
			response := serveTestRequest(https, newTestRequestFrom("198.51.100.26:1000", "GET", "test-lb.service.example", target, true))
			if response.Code != http.StatusNotFound {
				t.Fatalf("%s request %d = %d, want 404: exempt, and served by no location", target, i, response.Code)
			}
		}
	}
}

// A route zone applies on the service's own names, where it replaces the
// shared zone; the same path through an lb route is charged to the shared
// zone.
func TestRouteHttpRouteZonesApplyOnServiceNamesOnly(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	setTestRateLimit(t, routeTable, "eth1", 1, 0, 0)
	testRouteService(t, routeTable, "api").RouteRateLimits[0] = &RouteRateLimitRule{
		Path:              "/connect/control",
		RequestsPerMinute: 1,
		Burst:             1,
	}
	testFront := newTestFront(t, routeTable, testFrontSettings())
	https := testFront.front.HttpsHandler("eth1")

	const addr = "198.51.100.31:1000"
	for i := range 2 {
		if response := serveTestRequest(https, newTestRequestFrom(addr, "POST", "api.service.example", "/connect/control", true)); response.Code != http.StatusOK {
			t.Fatalf("route request %d = %d, want 200", i, response.Code)
		}
	}
	if response := serveTestRequest(https, newTestRequestFrom(addr, "POST", "api.service.example", "/connect/control", true)); response.Code != http.StatusTooManyRequests {
		t.Fatalf("past the route's burst = %d, want 429", response.Code)
	}
	// the shared zone is untouched
	if response := serveTestRequest(https, newTestRequestFrom(addr, "GET", "api.service.example", "/v1", true)); response.Code != http.StatusOK {
		t.Fatalf("a shared-zone request = %d, want 200", response.Code)
	}

	const lbAddr = "198.51.100.32:1000"
	if response := serveTestRequest(https, newTestRequestFrom(lbAddr, "POST", "test-lb.service.example", "/hidden-example/by/service/api/connect/control", true)); response.Code != http.StatusOK {
		t.Fatalf("the route through an lb route = %d", response.Code)
	}
	if response := serveTestRequest(https, newTestRequestFrom(lbAddr, "POST", "test-lb.service.example", "/hidden-example/by/service/api/connect/control", true)); response.Code != http.StatusTooManyRequests {
		t.Fatalf("the route through an lb route, again = %d, want the shared zone's 429", response.Code)
	}
	if count := testCounterValue(t, testFront.metrics.limitRefusals.WithLabelValues("route", "route_api_connect_control", "api")); count != 1 {
		t.Errorf("%v route refusals counted, want 1", count)
	}
}

// Every series of the refusal counter, by its limit, zone and name_class
// labels.
func testLimitRefusalSeries(t *testing.T, metrics *frontMetrics) map[[3]string]float64 {
	t.Helper()
	collected := make(chan prometheus.Metric)
	go func() {
		metrics.limitRefusals.Collect(collected)
		close(collected)
	}()
	series := map[[3]string]float64{}
	for collectedMetric := range collected {
		metric := &dto.Metric{}
		if err := collectedMetric.Write(metric); err != nil {
			t.Fatal(err)
		}
		labels := [3]string{}
		for _, labelPair := range metric.GetLabel() {
			switch labelPair.GetName() {
			case "limit":
				labels[0] = labelPair.GetValue()
			case "zone":
				labels[1] = labelPair.GetValue()
			case "name_class":
				labels[2] = labelPair.GetValue()
			default:
				t.Errorf("the refusal counter has a label %q", labelPair.GetName())
			}
		}
		series[labels] = metric.GetCounter().GetValue()
	}
	return series
}

// nginx's names for route zones: route_, the service, _, and the path past
// its leading / with every rune but an ASCII letter or digit as _.
func TestLimitRouteZoneName(t *testing.T) {
	for _, c := range []struct {
		serviceName string
		path        string
		name        string
	}{
		{serviceName: "api", path: "/connect/control", name: "route_api_connect_control"},
		{serviceName: "api", path: "/auth/login-with~code.v2", name: "route_api_auth_login_with_code_v2"},
		{serviceName: "mcp", path: "/x", name: "route_mcp_x"},
		{serviceName: "api", path: "/é", name: "route_api__"},
	} {
		if name := limitRouteZoneName(c.serviceName, c.path); name != c.name {
			t.Errorf("limitRouteZoneName(%q, %q) = %q, want %q", c.serviceName, c.path, name, c.name)
		}
	}
}

// A refusal is counted with the zone its request was charged to, whichever
// limit refused it: a route zone by nginx's name for it, on the route's exact
// path on the service's names, for its own bucket, the concurrency cap and
// the state cap alike; the default zone everywhere else, the same route
// through an lb route and a case variant of its path included. A refused QUIC
// connection counts in the default zone, and one on an unknown interface, or
// of a class the front doesn't serve on QUIC, in none. Each is counted by the
// class of the name it asked for: a QUIC connection by its class, each with
// a cap of its own. Every zone label comes from the route table, none from a
// request.
func TestRouteHttpLimitRefusalsCountTheirZone(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	// two requests per address at once, then one a minute; one in flight
	setTestRateLimit(t, routeTable, "eth1", 1, 1, 1)
	testRouteService(t, routeTable, "api").RouteRateLimits[0] = &RouteRateLimitRule{
		Path:              "/connect/control",
		RequestsPerMinute: 1,
		Burst:             1,
	}
	settings := testFrontSettings()
	settings.Limit.MaxAddressStates = 2
	testFront := newTestFront(t, routeTable, settings)
	https := testFront.front.HttpsHandler("eth1")

	// the forwards of two addresses are held, each in flight until released
	heldAddrs := map[string]bool{
		"198.51.100.53": true,
		"198.51.100.54": true,
	}
	entered := make(chan struct{}, len(heldAddrs))
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHeld := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	defer releaseHeld()
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		if heldAddrs[options.clientAddrPort.Addr().String()] {
			entered <- struct{}{}
			<-release
		}
		io.WriteString(w, "forwarded")
	}
	// sends requests from one address until the last, which must be refused
	refuseLast := func(name string, addr string, method string, host string, target string, count int) {
		for i := range count {
			response := serveTestRequest(https, newTestRequestFrom(addr+":1000", method, host, target, true))
			if i < count-1 && response.Code != http.StatusOK {
				t.Fatalf("%s: request %d = %d, want 200", name, i, response.Code)
			}
			if i == count-1 && response.Code != http.StatusTooManyRequests {
				t.Fatalf("%s: request %d = %d, want 429", name, i, response.Code)
			}
		}
	}

	refuseLast("the shared zone's bucket", "198.51.100.51", "GET", "api.service.example", "/v1", 3)
	refuseLast("the route zone's bucket", "198.51.100.52", "POST", "api.service.example", "/connect/control", 3)
	refuseLast("the route through an lb route", "198.51.100.55", "POST", "test-lb.service.example", "/hidden-example/by/service/api/connect/control", 3)
	refuseLast("a case variant of the route", "198.51.100.56", "POST", "api.service.example", "/CONNECT/CONTROL", 3)

	// one request in flight on the route, and one on the shared zone: each
	// address's next is refused by the cap in its own zone
	heldCodes := make(chan int, len(heldAddrs))
	for _, held := range []struct {
		addr   string
		target string
	}{
		{addr: "198.51.100.53", target: "/connect/control"},
		{addr: "198.51.100.54", target: "/v1"},
	} {
		go func() {
			heldCodes <- serveTestRequest(https, newTestRequestFrom(held.addr+":1000", "POST", "api.service.example", held.target, true)).Code
		}()
		waitTestBarrier(t, entered)
		if response := serveTestRequest(https, newTestRequestFrom(held.addr+":1001", "POST", "api.service.example", held.target, true)); response.Code != http.StatusTooManyRequests {
			t.Fatalf("%s while one is in flight = %d, want 429", held.target, response.Code)
		}
	}
	// both states in flight at the cap of two: a new address has no state,
	// refused in the zone of the location it asked for
	if response := serveTestRequest(https, newTestRequestFrom("198.51.100.57:1000", "POST", "api.service.example", "/connect/control", true)); response.Code != http.StatusTooManyRequests {
		t.Fatalf("a new address with every state in flight = %d, want 429", response.Code)
	}
	releaseHeld()
	for range heldAddrs {
		if statusCode := <-heldCodes; statusCode != http.StatusOK {
			t.Fatalf("a held request = %d, want 200", statusCode)
		}
	}

	quicAddr := netip.MustParseAddr("2001:db8::57")
	quicReleases := []func(){}
	for _, nameClass := range []NameClass{NameClassConnect, NameClassApi} {
		quicRelease, ok := testFront.front.AcquireQuicConnection("eth1", quicAddr, nameClass)
		if !ok {
			t.Fatalf("the first %s QUIC connection was refused", nameClass)
		}
		quicReleases = append(quicReleases, quicRelease)
		if _, ok := testFront.front.AcquireQuicConnection("eth1", quicAddr, nameClass); ok {
			t.Fatalf("a %s QUIC connection past the cap was admitted", nameClass)
		}
	}
	for _, quicRelease := range quicReleases {
		quicRelease()
	}
	if _, ok := testFront.front.AcquireQuicConnection("eth9", quicAddr, NameClassConnect); ok {
		t.Fatal("an unknown interface admitted a QUIC connection")
	}
	if _, ok := testFront.front.AcquireQuicConnection("eth1", quicAddr, NameClassForwarded); ok {
		t.Fatal("a QUIC connection of a class the front doesn't serve on QUIC was admitted")
	}

	want := map[[3]string]float64{
		{"request", limitDefaultZone, "api"}:                2,
		{"request", limitDefaultZone, "lb"}:                 1,
		{"route", "route_api_connect_control", "api"}:       1,
		{"concurrency", "route_api_connect_control", "api"}: 1,
		{"concurrency", limitDefaultZone, "api"}:            1,
		{"state", "route_api_connect_control", "api"}:       1,
		{"quic", limitDefaultZone, "connect"}:               1,
		{"quic", limitDefaultZone, "api"}:                   1,
		{"interface", routeHttpNoLabel, "connect"}:          1,
		{"class", routeHttpNoLabel, "forwarded"}:            1,
	}
	if series := testLimitRefusalSeries(t, testFront.metrics); !maps.Equal(series, want) {
		t.Errorf("refusals by limit, zone and name class %v, want %v", series, want)
	}
}

// One source on two interfaces has a quota on each, shared by every service
// of an interface, and an excluded source has none at all.
func TestRouteHttpLimitsArePerInterfaceAndSkipExcludedSubnets(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	setTestRateLimit(t, routeTable, "eth1", 1, 0, 1, "203.0.113.0/24")
	setTestRateLimit(t, routeTable, "eth2", 1, 0, 1, "203.0.113.0/24")
	testFront := newTestFront(t, routeTable, testFrontSettings())

	const addr = "198.51.100.41:1000"
	if response := serveTestRequest(testFront.front.HttpsHandler("eth1"), newTestRequestFrom(addr, "GET", "api.service.example", "/v1", true)); response.Code != http.StatusOK {
		t.Fatalf("eth1 = %d", response.Code)
	}
	if response := serveTestRequest(testFront.front.HttpsHandler("eth1"), newTestRequestFrom(addr, "GET", "api.service.example", "/v1", true)); response.Code != http.StatusTooManyRequests {
		t.Fatalf("eth1 again = %d, want 429", response.Code)
	}
	// the shared zone is the interface's, across services, as nginx's
	// standardlimit is
	if response := serveTestRequest(testFront.front.HttpsHandler("eth1"), newTestRequestFrom(addr, "GET", "mcp.service.example", "/v1", true)); response.Code != http.StatusTooManyRequests {
		t.Fatalf("another service on eth1 = %d, want 429: one quota per interface", response.Code)
	}
	if response := serveTestRequest(testFront.front.HttpsHandler("eth2"), newTestRequestFrom(addr, "GET", "api.service.example", "/v1", true)); response.Code != http.StatusOK {
		t.Fatalf("the same source on eth2 = %d, want its own quota", response.Code)
	}

	for i := range 10 {
		if response := serveTestRequest(testFront.front.HttpsHandler("eth1"), newTestRequestFrom("203.0.113.5:1000", "GET", "api.service.example", "/v1", true)); response.Code != http.StatusOK {
			t.Fatalf("excluded, request %d = %d, want 200", i, response.Code)
		}
	}
}

// Every h2 stream is a request with a slot of its own: with a cap of one, a
// second stream on the same connection, while the first is in flight, is a
// 429.
func TestRouteHttpEveryH2StreamTakesASlot(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	setTestRateLimit(t, routeTable, "eth1", 0, 0, 1)
	testFront := newTestFront(t, routeTable, testFrontSettings())
	entered := make(chan struct{})
	release := make(chan struct{})
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		if r.URL.Path == "/hold" {
			close(entered)
			<-release
		}
		io.WriteString(w, "forwarded")
	}

	server := httptest.NewUnstartedServer(testFront.front.HttpsHandler("eth1"))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	// a failed check still ends the held request, which the server's close
	// waits for
	var releaseOnce sync.Once
	releaseHeld := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	defer releaseHeld()
	client := server.Client()

	send := func(path string, gotConn func(httptrace.GotConnInfo)) (*http.Response, error) {
		r, err := http.NewRequestWithContext(t.Context(), "GET", server.URL+path, nil)
		if err != nil {
			return nil, err
		}
		r.Host = "api.service.example"
		if gotConn != nil {
			r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{
				GotConn: gotConn,
			}))
		}
		return client.Do(r)
	}

	type held struct {
		response *http.Response
		err      error
	}
	heldResponse := make(chan held, 1)
	go func() {
		response, err := send("/hold", nil)
		heldResponse <- held{
			response: response,
			err:      err,
		}
	}()
	waitTestBarrier(t, entered)

	reused := false
	response, err := send("/second", func(gotConnInfo httptrace.GotConnInfo) {
		reused = gotConnInfo.Reused
	})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.ProtoMajor != 2 || !reused {
		t.Fatalf("the second stream went over HTTP/%d, reused %t: want the first's h2 connection", response.ProtoMajor, reused)
	}
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a second stream while the first is in flight = %d, want 429", response.StatusCode)
	}

	releaseHeld()
	first := <-heldResponse
	if first.err != nil {
		t.Fatal(first.err)
	}
	first.response.Body.Close()
	if first.response.StatusCode != http.StatusOK {
		t.Fatalf("the first stream = %d", first.response.StatusCode)
	}
	response, err = send("/third", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("after the first ended = %d, want 200", response.StatusCode)
	}
}

// Every h3 request is a stream with a slot of its own, as on h2.
func TestRouteHttpEveryH3StreamTakesASlot(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	setTestRateLimit(t, routeTable, "eth1", 0, 0, 1)
	testFront := newTestFront(t, routeTable, testFrontSettings())
	entered := make(chan struct{})
	release := make(chan struct{})
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		if options.upstreamPath == "/hold" {
			close(entered)
			<-release
		}
		io.WriteString(w, "forwarded")
	}
	handler := testFront.front.H3ApiHandler("eth1")
	var releaseOnce sync.Once
	releaseHeld := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	defer releaseHeld()

	done := make(chan int, 1)
	go func() {
		done <- serveTestRequest(handler, asTestProtocol(newTestRequest("GET", "api.service.example", "/hold", true), 3)).Code
	}()
	waitTestBarrier(t, entered)
	if response := serveTestRequest(handler, asTestProtocol(newTestRequest("GET", "api.service.example", "/second", true), 3)); response.Code != http.StatusTooManyRequests {
		t.Fatalf("a second h3 stream while the first is in flight = %d, want 429", response.Code)
	}
	releaseHeld()
	if statusCode := <-done; statusCode != http.StatusOK {
		t.Fatalf("the first stream = %d", statusCode)
	}
}

// A response writer that hijacks into one end of a pipe, as an h1 server's
// writer hijacks its connection.
type testHijackResponseWriter struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

// See http.Hijacker.
func (self *testHijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return self.conn, bufio.NewReadWriter(bufio.NewReader(self.conn), bufio.NewWriter(self.conn)), nil
}

// An upgraded connection holds its request's slot after its handler has
// returned, until the tunnel closes, for connect served in-process and for
// a forwarded websocket alike.
func TestRouteHttpUpgradeHoldsItsSlotUntilTheTunnelCloses(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	setTestRateLimit(t, routeTable, "eth1", 0, 0, 1)
	mcp := testRouteService(t, routeTable, "mcp")
	mcp.Websocket = true
	mcp.Streamable = false
	testFront := newTestFront(t, routeTable, testFrontSettings())

	// each takes the tunnel and returns at once, as a handler that hands it
	// to other goroutines does
	var tunnel net.Conn
	hijack := func(w http.ResponseWriter) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("the writer over an h1 writer can't hijack")
		}
		conn, readWriter, err := hijacker.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		readWriter.WriteString("HTTP/1.1 101 Switching Protocols\r\n\r\n")
		tunnel = conn
	}
	testFront.connectHandler.serve = func(w http.ResponseWriter, r *http.Request) {
		hijack(w)
	}
	testFront.forwarders["mcp"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		hijack(w)
	}
	handler := testFront.front.HttpsHandler("eth1")

	for _, host := range []string{"connect.service.example", "mcp.service.example"} {
		serverConn, clientConn := net.Pipe()
		defer clientConn.Close()
		upgrade := newTestRequestFrom("198.51.100.51:1000", "GET", host, "/", true)
		upgrade.Header.Set("Connection", "Upgrade")
		upgrade.Header.Set("Upgrade", "websocket")
		handler.ServeHTTP(&testHijackResponseWriter{
			ResponseRecorder: httptest.NewRecorder(),
			conn:             serverConn,
		}, upgrade)
		if tunnel == nil {
			t.Fatalf("%s: no tunnel", host)
		}

		// the handler has returned; the tunnel still holds the slot
		next := newTestRequestFrom("198.51.100.51:1000", "GET", "api.service.example", "/v1", true)
		if response := serveTestRequest(handler, next); response.Code != http.StatusTooManyRequests {
			t.Fatalf("%s: while the tunnel is open = %d, want 429", host, response.Code)
		}
		if count := testFront.front.hijackedConnCount(); count != 1 {
			t.Fatalf("%s: %d tracked tunnels, want 1", host, count)
		}

		tunnel.Close()
		tunnel = nil
		next = newTestRequestFrom("198.51.100.51:1000", "GET", "api.service.example", "/v1", true)
		if response := serveTestRequest(handler, next); response.Code != http.StatusOK {
			t.Fatalf("%s: once the tunnel closed = %d, want 200", host, response.Code)
		}
		if count := testFront.front.hijackedConnCount(); count != 0 {
			t.Fatalf("%s: %d tracked tunnels after the close, want 0", host, count)
		}
	}
}

// A declared body over client_max_body_size is a 413 at find-config, before
// the rewrite phase and the limits: never charged, before a preflight, and
// with the location's always headers but no Access-Control-Allow-Origin,
// which the rewrite phase sets. On h1 it closes the connection, as nginx's
// 413 does; h2 gets no Connection header.
func TestRouteHttpDeclaredBodyOverTheCapIs413BeforeTheLimits(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	setTestRateLimit(t, routeTable, "eth1", 1, 0, 0)
	testFront := newTestFront(t, routeTable, testFrontSettings())
	handler := testFront.front.HttpsHandler("eth1")
	maxBodyBytes := testFront.front.settings.Body.MaxBodyBytes

	const addr = "198.51.100.61:1000"
	for _, c := range []struct {
		method     string
		protoMajor int
		connection []string
	}{
		{method: "POST", protoMajor: 1, connection: []string{"close"}},
		{method: "OPTIONS", protoMajor: 1, connection: []string{"close"}},
		{method: "POST", protoMajor: 2, connection: nil},
	} {
		r := newTestRequestFrom(addr, c.method, "api.service.example", "/v1", true)
		if c.protoMajor == 2 {
			r = asTestProtocol(r, 2)
		}
		r.ContentLength = maxBodyBytes + 1
		r.Header.Set("Origin", "https://app.service.example")
		response := serveTestRequest(handler, r)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s over the cap = %d, want 413", c.method, response.Code)
		}
		checkSecurityHeaders(t, "413", response.Header())
		if values := response.Header().Values("Access-Control-Allow-Origin"); len(values) != 0 {
			t.Errorf("413: Access-Control-Allow-Origin %q, want none", values)
		}
		if response.Header().Get("Access-Control-Allow-Methods") == "" || response.Header().Get("Vary") != "Origin" {
			t.Errorf("413: the other CORS headers are missing: %v", response.Header())
		}
		if values := response.Header().Values("Connection"); !slices.Equal(values, c.connection) {
			t.Errorf("%s over HTTP/%d: Connection %q, want %q", c.method, c.protoMajor, values, c.connection)
		}
	}
	atCap := newTestRequestFrom(addr, "POST", "api.service.example", "/v1", true)
	atCap.ContentLength = maxBodyBytes
	if response := serveTestRequest(handler, atCap); response.Code != http.StatusOK {
		t.Fatalf("a body at the cap = %d, want 200: the 413s took nothing", response.Code)
	}
	if forwards := testFront.forwarders["api"].recordedForwards(); len(forwards) != 1 {
		t.Fatalf("%d forwards, want only the body at the cap", len(forwards))
	}
}

// Connect's names are served in-process, so the front caps a body of
// unknown length as nginx's client_max_body_size does while it reads one:
// connect's read of a chunked body over BodySettings.MaxBodyBytes fails at
// the cap with *http.MaxBytesError, on h1 and h2, and a body exactly at the
// cap is read whole. The declared-length 413 never sees such a body.
func TestRouteHttpConnectCapsABodyOfUnknownLength(t *testing.T) {
	settings := testFrontSettings()
	settings.Body.MaxBodyBytes = 1024
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	type bodyRead struct {
		byteCount int
		err       error
	}
	var lastBodyRead bodyRead
	testFront.connectHandler.serve = func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		lastBodyRead = bodyRead{
			byteCount: len(body),
			err:       err,
		}
	}
	handler := testFront.front.HttpsHandler("eth1")

	for _, c := range []struct {
		protoMajor int
		bodyBytes  int
		capped     bool
	}{
		{protoMajor: 1, bodyBytes: 1024, capped: false},
		{protoMajor: 1, bodyBytes: 1025, capped: true},
		{protoMajor: 1, bodyBytes: 64 * 1024, capped: true},
		{protoMajor: 2, bodyBytes: 1024, capped: false},
		{protoMajor: 2, bodyBytes: 1025, capped: true},
	} {
		r := newTestRequest("POST", "connect.service.example", "/", true)
		if c.protoMajor == 2 {
			r = asTestProtocol(r, 2)
		}
		r.Body = io.NopCloser(strings.NewReader(strings.Repeat("a", c.bodyBytes)))
		// chunked: no declared length
		r.ContentLength = -1
		lastBodyRead = bodyRead{}
		serveTestRequest(handler, r)

		var maxBytesError *http.MaxBytesError
		switch {
		case !c.capped && (lastBodyRead.err != nil || lastBodyRead.byteCount != c.bodyBytes):
			t.Errorf("HTTP/%d, %d bytes: read %d, %v, want the whole body", c.protoMajor, c.bodyBytes, lastBodyRead.byteCount, lastBodyRead.err)
		case c.capped && (!errors.As(lastBodyRead.err, &maxBytesError) || maxBytesError.Limit != 1024 || lastBodyRead.byteCount != 1024):
			t.Errorf("HTTP/%d, %d bytes: read %d, %v, want 1024 and the cap's error", c.protoMajor, c.bodyBytes, lastBodyRead.byteCount, lastBodyRead.err)
		}
	}
	if requests := testFront.connectHandler.recordedRequests(); len(requests) != 5 {
		t.Fatalf("%d requests reached connect, want 5: the cap is enforced while it reads", len(requests))
	}
}
