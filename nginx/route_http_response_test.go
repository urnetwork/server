package nginx

// Tests of what the handlers add to responses: the CORS matrix, the security
// headers on every status, gzip as nginx's defaults, Connection: close while
// draining, and the access log.

import (
	"bytes"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The CORS headers a response must have: the echoed or wildcard origin, or
// none, and the lists, Vary in echo mode, whatever the request's Origin.
func checkCorsHeaders(t *testing.T, name string, header http.Header, allowOrigin string, vary bool) {
	t.Helper()
	if values := header.Values("Access-Control-Allow-Origin"); allowOrigin == "" && len(values) != 0 || allowOrigin != "" && !slices.Equal(values, []string{allowOrigin}) {
		t.Errorf("%s: Access-Control-Allow-Origin %q, want %q", name, values, allowOrigin)
	}
	for addedHeader, want := range map[string]string{
		"Access-Control-Allow-Methods":  "GET, POST, OPTIONS",
		"Access-Control-Allow-Headers":  responseCorsAllowHeaders,
		"Access-Control-Expose-Headers": responseCorsExposeHeaders,
	} {
		if values := header.Values(addedHeader); !slices.Equal(values, []string{want}) {
			t.Errorf("%s: %s %q, want %q", name, addedHeader, values, want)
		}
	}
	if values := header.Values("Vary"); vary && !slices.Contains(values, "Origin") || !vary && slices.Contains(values, "Origin") {
		t.Errorf("%s: Vary %q, want Origin %t", name, values, vary)
	}
	if values := header.Values("Access-Control-Allow-Credentials"); len(values) != 0 {
		t.Errorf("%s: Access-Control-Allow-Credentials %q, never sent", name, values)
	}
}

// The CORS matrix: an exact origin is echoed with Vary: Origin; any other,
// or none, gets the lists and Vary but no origin; a wildcard service allows
// any, with no Vary; an OPTIONS on a CORS service is the front's own 204,
// never forwarded, with or without an Origin; a service without CORS
// forwards OPTIONS and adds no CORS headers.
func TestRouteHttpCorsMatrix(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	testRouteService(t, routeTable, "mcp").CorsOrigins = []string{"*"}
	testFront := newTestFront(t, routeTable, testFrontSettings())
	handler := testFront.front.HttpsHandler("eth1")

	cases := []struct {
		name        string
		host        string
		origins     []string
		allowOrigin string
		vary        bool
	}{
		{name: "an allowed origin", host: "api.service.example", origins: []string{"https://app.service.example"}, allowOrigin: "https://app.service.example", vary: true},
		{name: "a case variant", host: "api.service.example", origins: []string{"https://App.service.example"}, allowOrigin: "", vary: true},
		{name: "another origin", host: "api.service.example", origins: []string{"https://evil.example"}, allowOrigin: "", vary: true},
		{name: "no origin", host: "api.service.example", origins: nil, allowOrigin: "", vary: true},
		{name: "two origins", host: "api.service.example", origins: []string{"https://app.service.example", "https://app.service.example"}, allowOrigin: "", vary: true},
		{name: "wildcard", host: "mcp.service.example", origins: []string{"https://evil.example"}, allowOrigin: "*", vary: false},
		{name: "wildcard, no origin", host: "mcp.service.example", origins: nil, allowOrigin: "*", vary: false},
	}
	for _, c := range cases {
		for _, method := range []string{"GET", "POST"} {
			r := newTestRequest(method, c.host, "/v1", true)
			for _, origin := range c.origins {
				r.Header.Add("Origin", origin)
			}
			response := serveTestRequest(handler, r)
			if response.Code != http.StatusOK {
				t.Fatalf("%s %s = %d", c.name, method, response.Code)
			}
			checkCorsHeaders(t, c.name+" "+method, response.Header(), c.allowOrigin, c.vary)
			checkSecurityHeaders(t, c.name+" "+method, response.Header())
		}

		forwardsBefore := len(testFront.forwarders["api"].recordedForwards()) + len(testFront.forwarders["mcp"].recordedForwards())
		preflight := newTestRequest("OPTIONS", c.host, "/v1/anything", true)
		for _, origin := range c.origins {
			preflight.Header.Add("Origin", origin)
		}
		preflight.Header.Set("Access-Control-Request-Method", "DELETE")
		response := serveTestRequest(handler, preflight)
		if response.Code != http.StatusNoContent {
			t.Fatalf("%s preflight = %d, want 204", c.name, response.Code)
		}
		checkCorsHeaders(t, c.name+" preflight", response.Header(), c.allowOrigin, c.vary)
		checkSecurityHeaders(t, c.name+" preflight", response.Header())
		if maxAge := response.Header().Get("Access-Control-Max-Age"); maxAge != "1728000" {
			t.Errorf("%s preflight: Access-Control-Max-Age %q", c.name, maxAge)
		}
		if contentType := response.Header().Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
			t.Errorf("%s preflight: Content-Type %q", c.name, contentType)
		}
		if response.Body.Len() != 0 {
			t.Errorf("%s preflight has a body", c.name)
		}
		forwardsAfter := len(testFront.forwarders["api"].recordedForwards()) + len(testFront.forwarders["mcp"].recordedForwards())
		if forwardsAfter != forwardsBefore {
			t.Errorf("%s preflight was forwarded", c.name)
		}
	}

	// connect has no cors_origins: OPTIONS reaches it, and no CORS header
	// is added
	response := serveTestRequest(handler, newTestRequest("OPTIONS", "connect.service.example", "/", true))
	if response.Code != http.StatusOK || len(testFront.connectHandler.recordedRequests()) != 1 {
		t.Fatalf("OPTIONS without CORS = %d, want it served by the service", response.Code)
	}
	if values := response.Header().Values("Access-Control-Allow-Methods"); len(values) != 0 {
		t.Errorf("a service without CORS got %q", values)
	}
}

// A service name's responses carry the security headers on every status,
// the upstream's errors, the front's 429 and a 502 included, and the CORS
// headers with the origin the rewrite phase set; the /status deny, lb
// names and redirects carry none. X-Frame-Options follows the upstream's
// CSP.
func TestRouteHttpSecurityHeadersOnEveryStatus(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	setTestRateLimit(t, routeTable, "eth1", 0, 0, 1)
	testFront := newTestFront(t, routeTable, testFrontSettings())
	heldEntered := make(chan struct{})
	heldRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseHeld := func() {
		releaseOnce.Do(func() {
			close(heldRelease)
		})
	}
	defer releaseHeld()
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		switch options.upstreamPath {
		case "/404":
			w.WriteHeader(http.StatusNotFound)
		case "/502":
			w.WriteHeader(http.StatusBadGateway)
		case "/frame":
			w.Header().Set("Content-Security-Policy", "frame-ancestors https:")
			w.WriteHeader(http.StatusOK)
		case "/hold":
			close(heldEntered)
			<-heldRelease
		default:
			w.WriteHeader(http.StatusOK)
		}
	}
	handler := testFront.front.HttpsHandler("eth1")

	request := func(target string) *http.Request {
		r := newTestRequest("GET", "api.service.example", target, true)
		r.Header.Set("Origin", "https://app.service.example")
		return r
	}
	for _, c := range []struct {
		target     string
		statusCode int
	}{
		{target: "/200", statusCode: 200},
		{target: "/404", statusCode: 404},
		{target: "/502", statusCode: 502},
	} {
		response := serveTestRequest(handler, request(c.target))
		if response.Code != c.statusCode {
			t.Fatalf("%s = %d", c.target, response.Code)
		}
		checkSecurityHeaders(t, c.target, response.Header())
		checkCorsHeaders(t, c.target, response.Header(), "https://app.service.example", true)
	}

	// the front's own 429, while another request holds the address's slot
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveTestRequest(handler, request("/hold"))
	}()
	waitTestBarrier(t, heldEntered)
	response := serveTestRequest(handler, request("/v1"))
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("over the cap = %d, want 429", response.Code)
	}
	checkSecurityHeaders(t, "429", response.Header())
	checkCorsHeaders(t, "429", response.Header(), "https://app.service.example", true)
	releaseHeld()
	<-done

	response = serveTestRequest(handler, request("/frame"))
	if values := response.Header().Values("X-Frame-Options"); len(values) != 0 {
		t.Errorf("an upstream CSP that allows framing: X-Frame-Options %q, want none", values)
	}

	for _, c := range []struct {
		name    string
		request *http.Request
	}{
		{name: "the deny", request: newTestRequest("GET", "api.service.example", "/status", true)},
		{name: "an lb route", request: newTestRequest("GET", "test-lb.service.example", "/hidden-example/by/service/api/x", true)},
		{name: "the lb status", request: newTestRequest("GET", "test-lb.service.example", "/hidden-example/status", true)},
		{name: "a redirect", request: newTestRequest("GET", "old-api.service.example", "/", true)},
	} {
		checkNoAddedHeaders(t, c.name, serveTestRequest(handler, c.request).Header())
	}
}

// nginx's gzip with its defaults: text/html the upstream didn't encode,
// statuses 200, 403 and 404, at least 20 bytes or of unknown length, to an
// HTTP/1.1 or later client offering gzip with no Via, never HEAD. Content
// Length and Accept-Ranges go and the ETag turns weak.
func TestRouteHttpGzipsTextHtml(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	const html = "<html><body>a page of text that compresses</body></html>"
	type upstream struct {
		statusCode      int
		contentType     string
		contentEncoding string
		contentLength   string
		body            string
	}
	var current upstream
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		header := w.Header()
		header.Set("Content-Type", current.contentType)
		header.Set("ETag", `"v1"`)
		header.Set("Accept-Ranges", "bytes")
		if current.contentEncoding != "" {
			header.Set("Content-Encoding", current.contentEncoding)
		}
		if current.contentLength != "" {
			header.Set("Content-Length", current.contentLength)
		}
		w.WriteHeader(current.statusCode)
		io.WriteString(w, current.body)
	}
	handler := testFront.front.HttpsHandler("eth1")

	cases := []struct {
		name     string
		upstream upstream
		method   string
		proto    string
		header   http.Header
		gzips    bool
	}{
		{name: "a page", upstream: upstream{statusCode: 200, contentType: "text/html; charset=utf-8", body: html}, gzips: true},
		{name: "a page with a length", upstream: upstream{statusCode: 200, contentType: "text/html", contentLength: "57", body: html}, gzips: true},
		{name: "a 404 page", upstream: upstream{statusCode: 404, contentType: "TEXT/HTML", body: html}, gzips: true},
		{name: "a 403 page", upstream: upstream{statusCode: 403, contentType: "text/html", body: html}, gzips: true},
		{name: "a 500 page", upstream: upstream{statusCode: 500, contentType: "text/html", body: html}, gzips: false},
		{name: "json", upstream: upstream{statusCode: 200, contentType: "application/json", body: html}, gzips: false},
		{name: "already encoded", upstream: upstream{statusCode: 200, contentType: "text/html", contentEncoding: "br", body: html}, gzips: false},
		{name: "under 20 bytes", upstream: upstream{statusCode: 200, contentType: "text/html", contentLength: "19", body: html[:19]}, gzips: false},
		{name: "head", upstream: upstream{statusCode: 200, contentType: "text/html"}, method: "HEAD", gzips: false},
		{name: "http/1.0", upstream: upstream{statusCode: 200, contentType: "text/html", body: html}, proto: "HTTP/1.0", gzips: false},
		{name: "via", upstream: upstream{statusCode: 200, contentType: "text/html", body: html}, header: http.Header{"Via": {"1.1 proxy.example"}}, gzips: false},
		{name: "no gzip offered", upstream: upstream{statusCode: 200, contentType: "text/html", body: html}, header: http.Header{"Accept-Encoding": {"br"}}, gzips: false},
		{name: "gzip refused", upstream: upstream{statusCode: 200, contentType: "text/html", body: html}, header: http.Header{"Accept-Encoding": {"gzip;q=0"}}, gzips: false},
	}
	for _, c := range cases {
		current = c.upstream
		method := c.method
		if method == "" {
			method = "GET"
		}
		r := newTestRequest(method, "api.service.example", "/page", true)
		r.Header.Set("Accept-Encoding", "gzip, deflate")
		for name, values := range c.header {
			r.Header[name] = values
		}
		if c.proto == "HTTP/1.0" {
			r.Proto = c.proto
			r.ProtoMinor = 0
		}
		response := serveTestRequest(handler, r)
		header := response.Header()
		if gzips := header.Get("Content-Encoding") == "gzip"; gzips != c.gzips {
			t.Errorf("%s: gzipped %t, want %t (Content-Encoding %q)", c.name, gzips, c.gzips, header.Get("Content-Encoding"))
			continue
		}
		if !c.gzips {
			if c.upstream.contentEncoding == "" && c.method != "HEAD" && response.Body.String() != c.upstream.body {
				t.Errorf("%s: body %q, want the upstream's", c.name, response.Body.String())
			}
			continue
		}
		if values := header.Values("Content-Length"); len(values) != 0 {
			t.Errorf("%s: Content-Length %q kept", c.name, values)
		}
		if values := header.Values("Accept-Ranges"); len(values) != 0 {
			t.Errorf("%s: Accept-Ranges %q kept", c.name, values)
		}
		if etag := header.Get("ETag"); etag != `W/"v1"` {
			t.Errorf("%s: ETag %q, want it weak", c.name, etag)
		}
		if values := header.Values("Vary"); slices.Contains(values, "Accept-Encoding") {
			t.Errorf("%s: gzip added Vary, which nginx's gzip_vary off doesn't", c.name)
		}
		reader, err := gzip.NewReader(bytes.NewReader(response.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s: %s", c.name, err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("%s: %s", c.name, err)
		}
		if string(body) != c.upstream.body {
			t.Errorf("%s: decompressed %q, want %q", c.name, body, c.upstream.body)
		}
	}
}

// After BeginDrain every h1 and h2 response carries Connection: close, the
// front's own included; Go's h2 server turns it into a GOAWAY. h3 gets none,
// since the field is illegal there.
func TestRouteHttpBeginDrainClosesClientConnections(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	handler := testFront.front.HttpsHandler("eth1")

	if response := serveTestRequest(handler, newTestRequest("GET", "api.service.example", "/v1", true)); response.Header().Get("Connection") != "" {
		t.Fatalf("before the drain: Connection %q", response.Header().Get("Connection"))
	}
	if testFront.front.Draining() {
		t.Fatal("draining before BeginDrain")
	}
	testFront.front.BeginDrain()
	testFront.front.BeginDrain()
	if !testFront.front.Draining() {
		t.Fatal("not draining after BeginDrain")
	}

	for _, c := range []struct {
		name       string
		handler    http.Handler
		request    *http.Request
		connection string
	}{
		{name: "h1 forwarded", handler: handler, request: newTestRequest("GET", "api.service.example", "/v1", true), connection: "close"},
		{name: "h2 forwarded", handler: handler, request: asTestProtocol(newTestRequest("GET", "api.service.example", "/v1", true), 2), connection: "close"},
		{name: "h1 in-process", handler: handler, request: newTestRequest("GET", "connect.service.example", "/", true), connection: "close"},
		{name: "h1 lb status", handler: handler, request: newTestRequest("GET", "test-lb.service.example", "/hidden-example/status", true), connection: "close"},
		{name: "h1 404", handler: handler, request: newTestRequest("GET", "test-lb.service.example", "/nope", true), connection: "close"},
		{name: "port 80 redirect", handler: testFront.front.HttpHandler("eth1"), request: newTestRequest("GET", "api.service.example", "/", false), connection: "close"},
		{name: "h3", handler: testFront.front.H3ApiHandler("eth1"), request: asTestProtocol(newTestRequest("GET", "api.service.example", "/v1", true), 3), connection: ""},
	} {
		response := serveTestRequest(c.handler, c.request)
		if connection := response.Header().Get("Connection"); connection != c.connection {
			t.Errorf("%s while draining: Connection %q, want %q", c.name, connection, c.connection)
		}
	}
}

// While draining, an h1 upgrade is refused with 503 instead of starting a
// tunnel that would hold the old generation for the whole drain, for a
// forwarded websocket and for connect in-process alike; an ordinary request
// is still served, with Connection: close.
func TestRouteHttpBeginDrainRefusesNewUpgrades(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	mcp := testRouteService(t, routeTable, "mcp")
	mcp.Websocket = true
	mcp.Streamable = false
	testFront := newTestFront(t, routeTable, testFrontSettings())
	handler := testFront.front.HttpsHandler("eth1")
	upgrade := func(host string) *http.Request {
		r := newTestRequest("GET", host, "/", true)
		r.Header.Set("Connection", "keep-alive, Upgrade")
		r.Header.Set("Upgrade", "websocket")
		return r
	}

	// before the drain an upgrade goes through
	if response := serveTestRequest(handler, upgrade("mcp.service.example")); response.Code != http.StatusOK {
		t.Fatalf("an upgrade before the drain = %d", response.Code)
	}
	testFront.front.BeginDrain()
	for _, host := range []string{"mcp.service.example", "connect.service.example"} {
		response := serveTestRequest(handler, upgrade(host))
		if response.Code != http.StatusServiceUnavailable || response.Header().Get("Connection") != "close" {
			t.Errorf("%s: an upgrade while draining = %d %q, want 503 and close", host, response.Code, response.Header().Get("Connection"))
		}
	}
	if forwards := testFront.forwarders["mcp"].recordedForwards(); len(forwards) != 1 {
		t.Errorf("%d mcp forwards, want only the one before the drain", len(forwards))
	}
	if requests := testFront.connectHandler.recordedRequests(); len(requests) != 0 {
		t.Errorf("an upgrade reached connect while draining")
	}
	response := serveTestRequest(handler, newTestRequest("GET", "mcp.service.example", "/", true))
	if response.Code != http.StatusOK || response.Header().Get("Connection") != "close" {
		t.Errorf("an ordinary request while draining = %d %q, want 200 and close", response.Code, response.Header().Get("Connection"))
	}
}

// A response writer that records the write deadlines set on it, as an h1
// server's writer takes them for its connection.
type testDeadlineResponseWriter struct {
	*httptest.ResponseRecorder
	writeDeadlines []time.Time
}

// See http.ResponseController.
func (self *testDeadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	self.writeDeadlines = append(self.writeDeadlines, deadline)
	return nil
}

// A connection that records the write deadlines set on it.
type testDeadlineConn struct {
	net.Conn
	writeDeadlines []time.Time
}

// See net.Conn.
func (self *testDeadlineConn) SetWriteDeadline(deadline time.Time) error {
	self.writeDeadlines = append(self.writeDeadlines, deadline)
	return self.Conn.SetWriteDeadline(deadline)
}

// send_timeout: the client write deadline moves to SendTimeout from now when
// an h1 request starts, at the head and before every write and flush, so it
// bounds the wait between two writes and never the whole response; a hijack
// clears it for the tunnel's owner; a zero SendTimeout sets none.
func TestRouteHttpSendTimeoutBoundsEachWrite(t *testing.T) {
	settings := testFrontSettings()
	settings.Http.SendTimeout = 30 * time.Second
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		w.WriteHeader(http.StatusOK)
		for range 3 {
			io.WriteString(w, "chunk")
			http.NewResponseController(w).Flush()
		}
	}
	handler := testFront.front.HttpsHandler("eth1")

	before := time.Now()
	writer := &testDeadlineResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
	}
	handler.ServeHTTP(writer, newTestRequest("GET", "api.service.example", "/v1", true))
	after := time.Now()
	// the start, the head, and a write and a flush per chunk
	if len(writer.writeDeadlines) != 2+2*3 {
		t.Fatalf("%d write deadlines, want 8: %v", len(writer.writeDeadlines), writer.writeDeadlines)
	}
	for _, deadline := range writer.writeDeadlines {
		if deadline.Before(before.Add(30*time.Second)) || after.Add(30*time.Second).Before(deadline) {
			t.Errorf("write deadline %s, want SendTimeout from the write", deadline)
		}
	}

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	deadlineConn := &testDeadlineConn{
		Conn: serverConn,
	}
	testFront.connectHandler.serve = func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	handler.ServeHTTP(&testHijackResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             deadlineConn,
	}, newTestRequest("GET", "connect.service.example", "/", true))
	if len(deadlineConn.writeDeadlines) != 1 || !deadlineConn.writeDeadlines[0].IsZero() {
		t.Errorf("the hijacked connection's write deadlines %v, want one clear", deadlineConn.writeDeadlines)
	}

	settings.Http.SendTimeout = 0
	writer = &testDeadlineResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
	}
	handler.ServeHTTP(writer, newTestRequest("GET", "api.service.example", "/v1", true))
	if len(writer.writeDeadlines) != 0 {
		t.Errorf("SendTimeout 0 set %d write deadlines", len(writer.writeDeadlines))
	}
}

// The access log never holds a client address, from the peer or a header,
// or a hidden route prefix, wherever the client put it; it honors
// no_access_log and no_success_access_log, and samples.
func TestRouteHttpAccessLogKeepsAddressesAndPrefixesOut(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	testRouteService(t, routeTable, "mcp").RoutePrefixes = []string{"/mcp-hidden"}
	routeTable.NoAccessLog = []string{"mcp.service.example"}
	testFront := newTestFront(t, routeTable, testFrontSettings())
	https := testFront.front.HttpsHandler("eth1")
	http80 := testFront.front.HttpHandler("eth1")

	secrets := []string{"198.51.100.71", "40071", "203.0.113.9", "2001:db8::71", "hidden-example", "mcp-hidden"}
	send := func(handler http.Handler, remoteAddr string, host string, target string, overTls bool) {
		r := newTestRequestFrom(remoteAddr, "GET", host, target, overTls)
		r.Header.Set("X-UR-Forwarded-For", "203.0.113.9:1")
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		r.Header.Set("User-Agent", "agent/1 hidden-example")
		serveTestRequest(handler, r)
	}
	send(https, "198.51.100.71:40071", "test-lb.service.example", "/hidden-example/by/service/api/x", true)
	send(https, "198.51.100.71:40071", "test-lb.service.example", "/hidden-example/status", true)
	send(https, "[2001:db8::71]:40071", "unknown.example", "/x/hidden-example/y", true)
	send(http80, "198.51.100.71:40071", "test-lb.service.example", "/hidden-example/by/b/api/g1/status", false)
	send(https, "198.51.100.71:40071", "hidden-example.service.example", "/", true)
	lines := testFront.accessLog.writtenLines()
	if len(lines) != 5 {
		t.Fatalf("%d lines, want 5: %q", len(lines), lines)
	}
	for _, line := range lines {
		for _, secret := range secrets {
			if strings.Contains(line, secret) {
				t.Errorf("line %q holds %q", line, secret)
			}
		}
	}
	if !strings.Contains(lines[0], `"GET /<prefix>/by/service/api/x HTTP/1.1" 200`) || !strings.Contains(lines[0], " eth1 ") || !strings.Contains(lines[0], " api ") {
		t.Errorf("line %q, want the interface, the redacted request, the status and the service", lines[0])
	}

	// no_access_log: no line at all; no_success_access_log: no line for a
	// 2xx on an exact path of the service's exact names
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		if strings.HasSuffix(options.upstreamPath, "?fail") {
			w.WriteHeader(http.StatusBadGateway)
		}
	}
	before := len(testFront.accessLog.writtenLines())
	send(https, "198.51.100.72:1", "mcp.service.example", "/mcp-hidden/x", true)
	// refused before routing, but the host is still known
	serveTestRequest(https, newTestRequestFrom("198.51.100.72:1", "TRACE", "MCP.service.example:443", "/", true))
	send(https, "198.51.100.72:1", "api.service.example", "/hello", true)
	send(https, "198.51.100.72:1", "test-api.other.example", "/connect/control", true)
	if after := len(testFront.accessLog.writtenLines()); after != before {
		t.Errorf("%d lines for quiet requests, want 0: %q", after-before, testFront.accessLog.writtenLines()[before:])
	}
	send(https, "198.51.100.72:1", "api.service.example", "/hello?fail", true)
	send(https, "198.51.100.72:1", "api.service.example", "/hello/", true)
	send(https, "198.51.100.72:1", "test-lb.service.example", "/hidden-example/by/service/api/hello", true)
	if after := len(testFront.accessLog.writtenLines()); after != before+3 {
		t.Errorf("%d lines, want 3: an error, a near path and an lb route stay logged", after-before)
	}

	// one in every two
	testFront.front.settings.Http.AccessLogSampleInterval = 2
	before = len(testFront.accessLog.writtenLines())
	for range 6 {
		send(https, "198.51.100.73:1", "api.service.example", "/v1", true)
	}
	if after := len(testFront.accessLog.writtenLines()); after != before+3 {
		t.Errorf("%d lines of 6 at one in two, want 3", after-before)
	}
}
