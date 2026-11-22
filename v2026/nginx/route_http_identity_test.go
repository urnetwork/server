package nginx

// Tests of the front's answers to refusals and of the identity limit as the
// handlers apply them (LBALTOPT2 R15): a 429 carries Retry-After, the
// refused bucket's wait in whole seconds, on h1, h2 and h3, while the 503s
// stay as they were; and with an interface's rate_limit turning the
// identity limit on, a request of the shared zone to the api or connect is
// charged to the identity LimitSettings.IdentifyRequest verifies, under its
// address's ceiling, while every other request is charged by its address as
// before.

import (
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Sets an interface's limits with the identity limit on.
func setTestIdentityRateLimit(t *testing.T, routeTable *RouteTable, interfaceName string, requestsPerMinute float64, burst int, netConnections int, identityRequestsPerMinute float64, identityBurst int, identityAddressFactor int) {
	t.Helper()
	testRouteInterface(t, routeTable, interfaceName).RateLimit = &RouteRateLimit{
		RequestsPerMinute:         requestsPerMinute,
		Burst:                     burst,
		NetConnections:            netConnections,
		ExcludeSubnets:            []string{},
		IdentityRequestsPerMinute: identityRequestsPerMinute,
		IdentityBurst:             identityBurst,
		IdentityAddressFactor:     identityAddressFactor,
	}
}

// An IdentifyRequest that verifies "Bearer client-<n>" as client n and
// "Bearer network-<n>" as network n, and nothing else, recording the
// upstream of every call.
type testIdentifyRequest struct {
	stateLock sync.Mutex
	upstreams []NameClass
}

// See LimitSettings.IdentifyRequest.
func (self *testIdentifyRequest) identify(r *http.Request, upstream NameClass) (LimitIdentity, bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.upstreams = append(self.upstreams, upstream)
	}()
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return LimitIdentity{}, false
	}
	identity := LimitIdentity{}
	var name string
	switch {
	case strings.HasPrefix(token, "client-"):
		name = strings.TrimPrefix(token, "client-")
	case strings.HasPrefix(token, "network-"):
		name = strings.TrimPrefix(token, "network-")
		identity.Network = true
	default:
		return LimitIdentity{}, false
	}
	if len(name) != 1 {
		return LimitIdentity{}, false
	}
	identity.Id[0] = name[0]
	identity.Id[15] = 1
	return identity, true
}

// The upstreams of the calls so far.
func (self *testIdentifyRequest) calls() []NameClass {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.upstreams)
}

// A request from addr with an Authorization, when set.
func newTestAuthorizedRequest(addr string, method string, host string, target string, authorization string) *http.Request {
	r := newTestRequestFrom(addr, method, host, target, true)
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	return r
}

// A refusal of a bucket answers 429 with Retry-After, the wait until the
// bucket admits, in whole seconds rounded up, on h1, h2 and h3 alike: two
// seconds a token for the shared zone, four for the route zone, and the
// longest, 60, for a refill that takes longer. A refusal of the concurrency
// cap reports no wait and says 1. The rest of the 429 is as before: its text,
// and a service name's headers. The front's 503s carry no Retry-After.
func TestRouteHttpRefusalsCarryRetryAfter(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	setTestRateLimit(t, routeTable, "eth1", 30, 0, 0)
	setTestRateLimit(t, routeTable, "eth2", 0, 0, 1)
	routeRateLimits := testRouteService(t, routeTable, "api").RouteRateLimits
	routeRateLimits[0] = &RouteRateLimitRule{
		Path:              "/connect/control",
		RequestsPerMinute: 15,
		Burst:             0,
	}
	testFront := newTestFront(t, routeTable, testFrontSettings())
	https := testFront.front.HttpsHandler("eth1")
	h3 := testFront.front.H3ApiHandler("eth1")

	checkRefusal := func(name string, response *http.Response, retryAfter string) {
		t.Helper()
		if response.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("%s = %d, want 429", name, response.StatusCode)
		}
		if values := response.Header.Values("Retry-After"); !slices.Equal(values, []string{retryAfter}) {
			t.Errorf("%s: Retry-After %q, want %q", name, values, retryAfter)
		}
		body, _ := io.ReadAll(response.Body)
		if string(body) != "Too Many Requests\n" || response.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("%s: %q %q, want the front's own 429", name, response.Header.Get("Content-Type"), body)
		}
		checkSecurityHeaders(t, name, response.Header)
	}

	for _, c := range []struct {
		name       string
		handler    http.Handler
		protoMajor int
		addr       string
	}{
		{name: "h1", handler: https, protoMajor: 1, addr: "198.51.100.101:1000"},
		{name: "h2", handler: https, protoMajor: 2, addr: "198.51.100.102:1000"},
		{name: "h3", handler: h3, protoMajor: 3, addr: "198.51.100.103:1000"},
	} {
		send := func() *http.Response {
			r := newTestRequestFrom(c.addr, "GET", "api.service.example", "/v1", true)
			if c.protoMajor != 1 {
				r = asTestProtocol(r, c.protoMajor)
			}
			return serveTestRequest(c.handler, r).Result()
		}
		if response := send(); response.StatusCode != http.StatusOK || response.Header.Get("Retry-After") != "" {
			t.Fatalf("%s first = %d, Retry-After %q", c.name, response.StatusCode, response.Header.Get("Retry-After"))
		}
		checkRefusal(c.name+" shared zone", send(), "2")
	}

	routeAddr := "198.51.100.104:1000"
	if response := serveTestRequest(https, newTestRequestFrom(routeAddr, "POST", "api.service.example", "/connect/control", true)); response.Code != http.StatusOK {
		t.Fatalf("route first = %d", response.Code)
	}
	checkRefusal("route zone", serveTestRequest(https, newTestRequestFrom(routeAddr, "POST", "api.service.example", "/connect/control", true)).Result(), "4")

	// a token every 100 minutes: the longest Retry-After
	slowRouteTable := loadTestRouteTable(t)
	setTestRateLimit(t, slowRouteTable, "eth1", 0.01, 0, 0)
	slowFront := newTestFront(t, slowRouteTable, testFrontSettings())
	slowAddr := "198.51.100.105:1000"
	serveTestRequest(slowFront.front.HttpsHandler("eth1"), newTestRequestFrom(slowAddr, "GET", "api.service.example", "/v1", true))
	checkRefusal("a slow refill", serveTestRequest(slowFront.front.HttpsHandler("eth1"), newTestRequestFrom(slowAddr, "GET", "api.service.example", "/v1", true)).Result(), "60")

	// the concurrency cap: one request held in flight on eth2
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHeld := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	defer releaseHeld()
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		if options.upstreamPath == "/hold" {
			close(entered)
			<-release
		}
		io.WriteString(w, "forwarded")
	}
	eth2 := testFront.front.HttpsHandler("eth2")
	heldCode := make(chan int, 1)
	go func() {
		heldCode <- serveTestRequest(eth2, newTestRequestFrom("198.51.100.106:1000", "GET", "api.service.example", "/hold", true)).Code
	}()
	waitTestBarrier(t, entered)
	checkRefusal("the concurrency cap", serveTestRequest(eth2, newTestRequestFrom("198.51.100.106:1001", "GET", "api.service.example", "/v1", true)).Result(), "1")
	releaseHeld()
	if code := <-heldCode; code != http.StatusOK {
		t.Fatalf("the held request = %d", code)
	}

	// the 503s are as before: a draining front's upgrade, and a closed front
	testFront.front.BeginDrain()
	upgrade := newTestRequestFrom("198.51.100.107:1000", "GET", "connect.service.example", "/", true)
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Upgrade", "websocket")
	if response := serveTestRequest(eth2, upgrade); response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "" {
		t.Errorf("an upgrade while draining = %d, Retry-After %q, want 503 and none", response.Code, response.Header().Get("Retry-After"))
	}
	testFront.front.Close()
	if response := serveTestRequest(eth2, newTestRequestFrom("198.51.100.108:1000", "GET", "api.service.example", "/v1", true)); response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "" {
		t.Errorf("a closed front = %d, Retry-After %q, want 503 and none", response.Code, response.Header().Get("Retry-After"))
	}
}

// With the identity limit on, a request whose credential verifies is
// charged to its identity: two clients behind one address each get the
// identity's budget, and a client past its own is refused in the identity
// zone with its bucket's Retry-After. The address's ceiling, the shared rate
// and burst times the factor, still bounds the address, whatever identities
// it carries. A request whose credential doesn't verify, or that carries
// none, is charged to the address's shared zone as before. A network token
// is its network's identity.
func TestRouteHttpIdentityLimitChargesVerifiedRequestsToTheirIdentity(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	// the address: two requests, then 3 a minute; a client: two, then 60 a
	// minute; the ceiling: three times the address, four at once, then 9 a
	// minute
	setTestIdentityRateLimit(t, routeTable, "eth1", 3, 1, 0, 60, 1, 3)
	identifier := &testIdentifyRequest{}
	settings := testFrontSettings()
	settings.Limit.IdentifyRequest = identifier.identify
	testFront := newTestFront(t, routeTable, settings)
	https := testFront.front.HttpsHandler("eth1")

	const addr = "198.51.100.111:1000"
	send := func(authorization string) *http.Response {
		return serveTestRequest(https, newTestAuthorizedRequest(addr, "GET", "api.service.example", "/v1", authorization)).Result()
	}
	for _, authorization := range []string{"Bearer client-1", "Bearer client-1", "Bearer client-2", "Bearer client-2"} {
		if response := send(authorization); response.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d, want its identity's budget", authorization, response.StatusCode)
		}
	}
	// client 1 is past its own burst, and the address at its ceiling: the
	// identity refuses, with the wait until both admit, the ceiling's 7 s
	if response := send("Bearer client-1"); response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "7" {
		t.Fatalf("client 1 past its burst = %d, Retry-After %q, want 429 and 7", response.StatusCode, response.Header.Get("Retry-After"))
	}
	// client 3 has its budget, but the address is at its ceiling
	if response := send("Bearer client-3"); response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "7" {
		t.Fatalf("client 3 at the address's ceiling = %d, Retry-After %q, want 429 and 7", response.StatusCode, response.Header.Get("Retry-After"))
	}
	// unverified and anonymous requests have the address's shared zone, two
	// requests, untouched by the identities
	for _, authorization := range []string{"Bearer forged", ""} {
		if response := send(authorization); response.StatusCode != http.StatusOK {
			t.Fatalf("%q = %d, want the shared zone's", authorization, response.StatusCode)
		}
	}
	if response := send("Bearer forged"); response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "20" {
		t.Fatalf("a forged token past the shared zone = %d, Retry-After %q, want 429 and 20", response.StatusCode, response.Header.Get("Retry-After"))
	}
	// a network token on another address is its network's own identity
	for i := range 2 {
		response := serveTestRequest(https, newTestAuthorizedRequest("198.51.100.112:1000", "GET", "api.service.example", "/v1", "Bearer network-1")).Result()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("network 1 request %d = %d", i, response.StatusCode)
		}
	}
	if response := serveTestRequest(https, newTestAuthorizedRequest("198.51.100.112:1000", "GET", "api.service.example", "/v1", "Bearer network-1")).Result(); response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("network 1 past its burst = %d, want 429", response.StatusCode)
	}

	want := map[[3]string]float64{
		{"identity", limitIdentityZone, "api"}: 2,
		{"ceiling", limitIdentityZone, "api"}:  1,
		{"request", limitDefaultZone, "api"}:   1,
	}
	if series := testLimitRefusalSeries(t, testFront.metrics); !maps.Equal(series, want) {
		t.Errorf("refusals %v, want %v", series, want)
	}
	if count := testFront.front.limits.identityStateCount(); count != 4 {
		t.Errorf("%d identity states, want clients 1, 2 and 3 and network 1: a forged token made none", count)
	}
}

// The identity limit asks IdentifyRequest only for a request it could
// charge: one of the shared zone, on an interface whose rate_limit turns it
// on, to the api or connect, with the upstream's class, through the lb's
// hidden routes too. A forwarded service, a route zone, an lb status path
// and an interface without the identity limit never ask, and are charged by
// their address alone.
func TestRouteHttpIdentityLimitIdentifiesOnlyApiAndConnectRequests(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	setTestIdentityRateLimit(t, routeTable, "eth1", 0, 0, 0, 60, 10, 2)
	setTestRateLimit(t, routeTable, "eth2", 0, 0, 0)
	identifier := &testIdentifyRequest{}
	settings := testFrontSettings()
	settings.Limit.IdentifyRequest = identifier.identify
	testFront := newTestFront(t, routeTable, settings)
	const authorization = "Bearer client-1"

	for _, c := range []struct {
		name          string
		interfaceName string
		method        string
		host          string
		target        string
		want          []NameClass
	}{
		{name: "api", interfaceName: "eth1", method: "GET", host: "api.service.example", target: "/v1", want: []NameClass{NameClassApi}},
		{name: "connect in-process", interfaceName: "eth1", method: "GET", host: "connect.service.example", target: "/", want: []NameClass{NameClassConnect}},
		{name: "the api through an lb route", interfaceName: "eth1", method: "GET", host: "test-lb.service.example", target: "/hidden-example/by/service/api/v1", want: []NameClass{NameClassApi}},
		{name: "a forwarded service", interfaceName: "eth1", method: "GET", host: "mcp.service.example", target: "/v1", want: nil},
		{name: "a route zone", interfaceName: "eth1", method: "POST", host: "api.service.example", target: "/connect/control", want: nil},
		{name: "the lb status", interfaceName: "eth1", method: "GET", host: "test-lb.service.example", target: "/hidden-example/status", want: nil},
		{name: "an interface without the identity limit", interfaceName: "eth2", method: "GET", host: "api.service.example", target: "/v1", want: nil},
	} {
		before := len(identifier.calls())
		response := serveTestRequest(testFront.front.HttpsHandler(c.interfaceName), newTestAuthorizedRequest("198.51.100.121:1000", c.method, c.host, c.target, authorization))
		if response.Code != http.StatusOK {
			t.Fatalf("%s = %d", c.name, response.Code)
		}
		if calls := identifier.calls()[before:]; !slices.Equal(calls, c.want) && !(len(calls) == 0 && len(c.want) == 0) {
			t.Errorf("%s: identified for %v, want %v", c.name, calls, c.want)
		}
	}
	// on h3, the api handler identifies as the api
	before := len(identifier.calls())
	r := asTestProtocol(newTestAuthorizedRequest("198.51.100.121:1000", "GET", "api.service.example", "/v1", authorization), 3)
	if response := serveTestRequest(testFront.front.H3ApiHandler("eth1"), r); response.Code != http.StatusOK {
		t.Fatalf("h3 = %d", response.Code)
	}
	if calls := identifier.calls()[before:]; !slices.Equal(calls, []NameClass{NameClassApi}) {
		t.Errorf("h3: identified for %v, want the api", calls)
	}
}

// Off, the identity limit changes nothing: the same requests, credentials
// and all, through a front whose route table leaves it off but whose
// settings can identify, get exactly the answers of a front that can't
// identify, IdentifyRequest is never asked, and no identity state is made.
func TestRouteHttpIdentityLimitOffChangesNothing(t *testing.T) {
	newFrontWith := func(identifyRequest func(r *http.Request, upstream NameClass) (LimitIdentity, bool)) *testFront {
		routeTable := loadTestRouteTable(t)
		setTestRateLimit(t, routeTable, "eth1", 2, 1, 1)
		settings := testFrontSettings()
		settings.Limit.IdentifyRequest = identifyRequest
		return newTestFront(t, routeTable, settings)
	}
	identifier := &testIdentifyRequest{}
	withHook := newFrontWith(identifier.identify)
	withoutHook := newFrontWith(nil)

	type answer struct {
		code       int
		retryAfter string
	}
	answers := func(testFront *testFront) []answer {
		answers := []answer{}
		for i := range 12 {
			authorization := []string{"Bearer client-1", "Bearer client-2", "", "Bearer forged"}[i%4]
			host := []string{"api.service.example", "connect.service.example"}[i%2]
			response := serveTestRequest(testFront.front.HttpsHandler("eth1"), newTestAuthorizedRequest("198.51.100.131:1000", "GET", host, "/v1", authorization))
			answers = append(answers, answer{code: response.Code, retryAfter: response.Header().Get("Retry-After")})
			testFront.clock.advance(7 * time.Second)
		}
		return answers
	}
	got := answers(withHook)
	want := answers(withoutHook)
	if !slices.Equal(got, want) {
		t.Fatalf("with the identity limit off, answers %v, want %v", got, want)
	}
	if !slices.ContainsFunc(want, func(a answer) bool { return a.code == http.StatusTooManyRequests }) || !slices.ContainsFunc(want, func(a answer) bool { return a.code == http.StatusOK }) {
		t.Fatalf("answers %v: the sequence must both admit and refuse to compare", want)
	}
	if calls := identifier.calls(); len(calls) != 0 {
		t.Fatalf("identified %d times with the identity limit off", len(calls))
	}
	if count := withHook.front.limits.identityStateCount(); count != 0 {
		t.Fatalf("%d identity states with the identity limit off", count)
	}
	if !maps.Equal(testLimitRefusalSeries(t, withHook.metrics), testLimitRefusalSeries(t, withoutHook.metrics)) {
		t.Fatalf("refusals %v, want %v", testLimitRefusalSeries(t, withHook.metrics), testLimitRefusalSeries(t, withoutHook.metrics))
	}
}
