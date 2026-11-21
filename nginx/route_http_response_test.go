package nginx

// Tests of what the handlers add to responses: the CORS matrix, the security
// headers on every status and on a hijacked upgrade's 101, gzip as nginx's
// defaults, Connection: close while draining, the access log, and the bound
// on reading a body that the front's own answers, or connect's in-process
// ones, leave unread.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/quic-go"
	"github.com/urnetwork/quic-go/http3"
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
// HTTP/1.1 or later client offering gzip with no Via. Accept-Ranges goes,
// the ETag turns weak, and the Content-Length is suppressed with nil, so the
// server adds none. A HEAD is decided as a GET and gets the same head, and
// nothing is compressed: what a handler writes for it anyway passes as it
// is, for the server to drop.
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
		// nginx's type is the value before a ';' only when a charset
		// follows it, untrimmed, and the whole value otherwise
		{name: "a page with a charset and no space", upstream: upstream{statusCode: 200, contentType: "text/html;charset=utf-8", body: html}, gzips: true},
		{name: "a page with an uppercase charset", upstream: upstream{statusCode: 200, contentType: "Text/Html;  CHARSET=\"utf-8\"", body: html}, gzips: true},
		{name: "a page with another parameter, then a charset", upstream: upstream{statusCode: 200, contentType: "text/html;q=1; charset=utf-8", body: html}, gzips: false},
		{name: "a page with another parameter", upstream: upstream{statusCode: 200, contentType: "text/html; foo=bar", body: html}, gzips: false},
		{name: "a page with a space before its charset", upstream: upstream{statusCode: 200, contentType: "text/html ; charset=utf-8", body: html}, gzips: false},
		{name: "a page with an empty parameter", upstream: upstream{statusCode: 200, contentType: "text/html;", body: html}, gzips: false},
		{name: "a page with a parameter, then a charset", upstream: upstream{statusCode: 200, contentType: "text/html; foo=bar; charset=utf-8", body: html}, gzips: false},
		{name: "a page with a charset after an empty parameter", upstream: upstream{statusCode: 200, contentType: "text/html; ;charset=utf-8", body: html}, gzips: false},
		{name: "a 403 page", upstream: upstream{statusCode: 403, contentType: "text/html", body: html}, gzips: true},
		{name: "a 500 page", upstream: upstream{statusCode: 500, contentType: "text/html", body: html}, gzips: false},
		{name: "json", upstream: upstream{statusCode: 200, contentType: "application/json", body: html}, gzips: false},
		{name: "already encoded", upstream: upstream{statusCode: 200, contentType: "text/html", contentEncoding: "br", body: html}, gzips: false},
		{name: "under 20 bytes", upstream: upstream{statusCode: 200, contentType: "text/html", contentLength: "19", body: html[:19]}, gzips: false},
		{name: "head", upstream: upstream{statusCode: 200, contentType: "text/html", contentLength: "57", body: html}, method: "HEAD", gzips: true},
		{name: "head of unknown length", upstream: upstream{statusCode: 200, contentType: "text/html"}, method: "HEAD", gzips: true},
		{name: "head of a 404 page", upstream: upstream{statusCode: 404, contentType: "text/html", contentLength: "57"}, method: "HEAD", gzips: true},
		{name: "head under 20 bytes", upstream: upstream{statusCode: 200, contentType: "text/html", contentLength: "19"}, method: "HEAD", gzips: false},
		{name: "head of json", upstream: upstream{statusCode: 200, contentType: "application/json", contentLength: "57"}, method: "HEAD", gzips: false},
		{name: "head over http/1.0", upstream: upstream{statusCode: 200, contentType: "text/html", contentLength: "57"}, method: "HEAD", proto: "HTTP/1.0", gzips: false},
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
			if c.upstream.contentEncoding == "" && response.Body.String() != c.upstream.body {
				t.Errorf("%s: body %q, want the upstream's", c.name, response.Body.String())
			}
			if c.upstream.contentLength != "" && header.Get("Content-Length") != c.upstream.contentLength {
				t.Errorf("%s: Content-Length %q, want the upstream's %q", c.name, header.Get("Content-Length"), c.upstream.contentLength)
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
		// the servers take a nil Content-Length as one suppressed
		if values, ok := header["Content-Length"]; !ok || values != nil {
			t.Errorf("%s: Content-Length %q, want it suppressed with nil", c.name, values)
		}
		if c.method == "HEAD" {
			// no gzip stream: what the handler wrote passed as it was
			if response.Body.String() != c.upstream.body {
				t.Errorf("%s: body %q, want what the handler wrote, uncompressed", c.name, response.Body.String())
			}
			continue
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

// Over real h1, h2 and h3 servers, a HEAD of a text/html page gets the head
// a GET gets, for a forwarded name and, on h1 and h2, for connect
// in-process: Content-Encoding: gzip, a weak ETag, and no Content-Length or
// Accept-Ranges, though the handler wrote the page anyway, from which each
// server would count a Content-Length of its own, h3 even with nothing
// written. No body arrives, and the next request on the connection is read
// in step.
func TestRouteHttpGzipsAHeadOverEveryProtocol(t *testing.T) {
	const html = "<html><body>a page of text that compresses</body></html>"
	vault := newTestTlsVault(t)
	testFront := newTestFront(t, vault.routeTable, testFrontSettings())
	writePage := func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Type", "text/html; charset=utf-8")
		header.Set("ETag", `"v1"`)
		header.Set("Accept-Ranges", "bytes")
		// whatever the method, as a handler may
		io.WriteString(w, html)
	}
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		writePage(w, r)
	}
	testFront.connectHandler.serve = writePage

	check := func(what string, client *http.Client, url string, host string) {
		for _, method := range []string{"HEAD", "GET"} {
			r, err := http.NewRequestWithContext(t.Context(), method, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Host = host
			r.Header.Set("Accept-Encoding", "gzip")
			response, err := client.Do(r)
			if err != nil {
				t.Fatalf("%s %s: %v", what, method, err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatalf("%s %s: reading the body: %v", what, method, err)
			}
			header := response.Header
			if response.StatusCode != http.StatusOK || header.Get("Content-Encoding") != "gzip" {
				t.Errorf("%s %s = %d, Content-Encoding %q; want 200 gzipped", what, method, response.StatusCode, header.Get("Content-Encoding"))
				continue
			}
			if values := header.Values("Content-Length"); len(values) != 0 {
				t.Errorf("%s %s: Content-Length %q, want none", what, method, values)
			}
			if values := header.Values("Accept-Ranges"); len(values) != 0 {
				t.Errorf("%s %s: Accept-Ranges %q, want none", what, method, values)
			}
			if etag := header.Get("ETag"); etag != `W/"v1"` {
				t.Errorf("%s %s: ETag %q, want it weak", what, method, etag)
			}
			if method == "HEAD" {
				if len(body) != 0 || len(response.TransferEncoding) != 0 {
					t.Errorf("%s HEAD: body %q, Transfer-Encoding %q; want neither", what, body, response.TransferEncoding)
				}
				continue
			}
			reader, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("%s GET: %v", what, err)
			}
			if page, err := io.ReadAll(reader); err != nil || string(page) != html {
				t.Errorf("%s GET: decompressed %q, %v; want the page", what, page, err)
			}
		}
	}

	for _, enableHttp2 := range []bool{false, true} {
		server := newTestConfiguredServer(t, testFront, enableHttp2)
		client := server.Client()
		protocol := "h1"
		if enableHttp2 {
			protocol = "h2"
		}
		check(protocol+" forwarded", client, server.URL+"/page", "api.service.example")
		check(protocol+" in-process", client, server.URL+"/page", "connect.service.example")
	}

	_, quicTlsConfig := vault.newTlsConfigs(t, testFrontSettings().Tls)
	// quic-go warns once per process when it can't raise the socket buffers
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	quicTransport := &quic.Transport{
		Conn: udpConn,
	}
	listener, err := quicTransport.ListenEarly(quicTlsConfig, &quic.Config{})
	if err != nil {
		udpConn.Close()
		t.Fatal(err)
	}
	h3Server := &http3.Server{
		Handler: testFront.front.H3ApiHandler("eth1"),
	}
	go h3Server.ServeListener(listener)
	t.Cleanup(func() {
		h3Server.Close()
		listener.Close()
		quicTransport.Close()
		udpConn.Close()
	})
	h3Transport := &http3.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "api.service.example",
		},
		Dial: func(ctx context.Context, _ string, tlsConfig *tls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
			return quic.DialAddr(ctx, udpConn.LocalAddr().String(), tlsConfig, quicConfig)
		},
	}
	t.Cleanup(func() {
		h3Transport.Close()
	})
	check("h3 forwarded", &http.Client{Transport: h3Transport, Timeout: testTlsExchangeTimeout}, "https://api.service.example/page", "api.service.example")
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

// A forwarder that hijacks writes its 101 from the header map, as a reverse
// proxy does, so the front puts the location's always headers in the map
// before the hijack. Over a real h1 server, a websocket upgrade on a
// service name gets the security and CORS headers on its 101, and one
// through an lb route gets none.
func TestRouteHttpHijackedUpgradeCarriesTheAlwaysHeaders(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	mcp := testRouteService(t, routeTable, "mcp")
	mcp.Websocket = true
	mcp.Streamable = false
	testFront := newTestFront(t, routeTable, testFrontSettings())
	testFront.forwarders["mcp"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		conn, readWriter, err := w.(http.Hijacker).Hijack()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		readWriter.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
		w.Header().Write(readWriter)
		readWriter.WriteString("\r\n")
		readWriter.Flush()
	}
	server, transport := newTestFrontServer(t, testFront, false)

	for _, c := range []struct {
		host   string
		target string
		added  bool
	}{
		{host: "mcp.service.example", target: "/socket", added: true},
		{host: "test-lb.service.example", target: "/hidden-example/by/service/mcp/socket", added: false},
	} {
		func() {
			conn, err := tls.Dial("tcp", server.Listener.Addr().String(), transport.TLSClientConfig)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nOrigin: https://app.service.example\r\n\r\n", c.target, c.host)
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("%s: %s", c.host, err)
			}
			if response.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("%s = %d, want the forwarder's 101", c.host, response.StatusCode)
			}
			if c.added {
				checkSecurityHeaders(t, c.host+" 101", response.Header)
				checkCorsHeaders(t, c.host+" 101", response.Header, "https://app.service.example", true)
			} else {
				checkNoAddedHeaders(t, c.host+" 101", response.Header)
			}
		}()
	}
	if forwards := testFront.forwarders["mcp"].recordedForwards(); len(forwards) != 2 {
		t.Errorf("%d upgrades forwarded, want 2", len(forwards))
	}
}

// On h1, before the front writes an answer of its own, Go's server reads
// the body the request left unread, up to 256 KiB, so a client that stops
// sending its body would hold the answer, and the request, for as long as
// it stays connected. The front bounds that read by LingeringTimeout, as
// nginx bounds its lingering reads: a 404, a 403 and a 301 to a request
// whose body stalls after its first bytes arrive after LingeringTimeout,
// with Connection: close, and the connection closes. A non-positive
// LingeringTimeout cuts the read at once. A body sent whole is read and the
// connection kept.
func TestRouteHttpOwnAnswerBoundsTheReadOfAnUnreadBody(t *testing.T) {
	const lingeringTimeout = 200 * time.Millisecond
	const answerBound = 3 * time.Second
	settings := testFrontSettings()
	settings.Http.LingeringTimeout = lingeringTimeout
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	server := newTestConfiguredServer(t, testFront, false)

	cases := []struct {
		host       string
		target     string
		statusCode int
	}{
		{host: "test-lb.service.example", target: "/hidden-example/missing", statusCode: http.StatusNotFound},
		{host: "api.service.example", target: "/status", statusCode: http.StatusForbidden},
		{host: "old-api.service.example", target: "/v1", statusCode: http.StatusMovedPermanently},
	}
	for _, c := range cases {
		client := dialTestServerH1Client(t, server.Listener.Addr().String())
		client.conn.SetDeadline(time.Now().Add(answerBound))
		start := time.Now()
		// 10 of the 1000 bytes the request declares
		fmt.Fprintf(client.conn, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Length: 1000\r\n\r\n0123456789", c.target, c.host)
		response, err := http.ReadResponse(client.reader, nil)
		if err != nil {
			t.Fatalf("%s%s with a stalled body: no answer: %v", c.host, c.target, err)
		}
		answerTime := time.Since(start)
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != c.statusCode || !response.Close || answerTime < lingeringTimeout {
			t.Errorf("%s%s with a stalled body = %d, Connection close %t, after %s; want %d with Connection: close after LingeringTimeout %s", c.host, c.target, response.StatusCode, response.Close, answerTime, c.statusCode, lingeringTimeout)
		}
		if !client.closedByServer() {
			t.Errorf("%s%s with a stalled body: the connection was kept", c.host, c.target)
		}
	}

	zeroSettings := testFrontSettings()
	zeroSettings.Http.LingeringTimeout = 0
	zeroServer := newTestConfiguredServer(t, newTestFront(t, loadTestRouteTable(t), zeroSettings), false)
	zeroClient := dialTestServerH1Client(t, zeroServer.Listener.Addr().String())
	zeroClient.conn.SetDeadline(time.Now().Add(answerBound))
	fmt.Fprintf(zeroClient.conn, "POST /status HTTP/1.1\r\nHost: api.service.example\r\nContent-Length: 1000\r\n\r\n0123456789")
	response, err := http.ReadResponse(zeroClient.reader, nil)
	if err != nil {
		t.Fatalf("a stalled body with no LingeringTimeout: no answer: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || !response.Close {
		t.Errorf("a stalled body with no LingeringTimeout = %d, Connection close %t; want 403 with Connection: close", response.StatusCode, response.Close)
	}

	client := dialTestServerH1Client(t, server.Listener.Addr().String())
	for i := range 2 {
		fmt.Fprintf(client.conn, "POST /status HTTP/1.1\r\nHost: api.service.example\r\nContent-Length: 10\r\n\r\n0123456789")
		response, err := http.ReadResponse(client.reader, nil)
		if err != nil {
			t.Fatalf("request %d with a whole body: %v", i+1, err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden || response.Close {
			t.Errorf("request %d with a whole body = %d, Connection close %t; want 403 on a kept connection", i+1, response.StatusCode, response.Close)
		}
	}
}

// A response writer that hijacks into one end of a pipe, as
// testHijackResponseWriter does, and records each read deadline set through
// it, as an h1 server's writer sets its connection's. Used by one goroutine.
type testReadDeadlineResponseWriter struct {
	*testHijackResponseWriter
	readDeadlines []time.Time
}

// The read deadline http.ResponseController sets.
func (self *testReadDeadlineResponseWriter) SetReadDeadline(deadline time.Time) error {
	self.readDeadlines = append(self.readDeadlines, deadline)
	return nil
}

// When connect's in-process handler returns on h1 with the body unread,
// wholly or in part, declared or chunked, the front bounds what Go's server
// then reads of it by LingeringTimeout, as for its own answers: Go's server
// reads it before an answer that keeps the connection, or after one that
// closes it. A body read whole, a request without one, a connection connect
// hijacked, whose deadlines are its tunnel's, and h2 get no deadline.
func TestRouteHttpConnectInProcessBoundsTheReadOfAnUnreadBody(t *testing.T) {
	const lingeringTimeout = 7 * time.Second
	settings := testFrontSettings()
	settings.Http.LingeringTimeout = lingeringTimeout
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	handler := testFront.front.HttpsHandler("eth1")

	cases := []struct {
		what       string
		method     string
		protoMajor int
		// of a ten-byte body; -1 for chunked, 0 for none
		contentLength int64
		// what connect's handler does before it answers "connect", if
		// anything
		serve func(w http.ResponseWriter, r *http.Request)
		// what the client got through the writer: "connect", or nothing
		// after a hijack
		answer  string
		bounded bool
	}{
		{what: "unread", method: "POST", protoMajor: 1, contentLength: 10, answer: "connect", bounded: true},
		{what: "chunked unread", method: "POST", protoMajor: 1, contentLength: -1, answer: "connect", bounded: true},
		{what: "partly read", method: "POST", protoMajor: 1, contentLength: 10, serve: func(w http.ResponseWriter, r *http.Request) {
			io.ReadFull(r.Body, make([]byte, 4))
		}, answer: "connect", bounded: true},
		{what: "read whole", method: "POST", protoMajor: 1, contentLength: 10, serve: func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
		}, answer: "connect", bounded: false},
		{what: "no body", method: "GET", protoMajor: 1, contentLength: 0, answer: "connect", bounded: false},
		{what: "hijacked", method: "POST", protoMajor: 1, contentLength: 10, serve: func(w http.ResponseWriter, r *http.Request) {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
		}, answer: "", bounded: false},
		{what: "h2 unread", method: "POST", protoMajor: 2, contentLength: 10, answer: "connect", bounded: false},
	}
	for _, c := range cases {
		serve := c.serve
		testFront.connectHandler.serve = func(w http.ResponseWriter, r *http.Request) {
			if serve != nil {
				serve(w, r)
			}
			io.WriteString(w, "connect")
		}
		r := asTestProtocol(newTestRequest(c.method, "connect.service.example", "/upload", true), c.protoMajor)
		if c.contentLength != 0 {
			r.Body = io.NopCloser(strings.NewReader("0123456789"))
			r.ContentLength = c.contentLength
		}
		serverConn, clientConn := net.Pipe()
		writer := &testReadDeadlineResponseWriter{
			testHijackResponseWriter: &testHijackResponseWriter{
				ResponseRecorder: httptest.NewRecorder(),
				conn:             serverConn,
			},
			readDeadlines: []time.Time{},
		}
		requestCount := len(testFront.connectHandler.recordedRequests())
		startTime := time.Now()
		handler.ServeHTTP(writer, r)
		endTime := time.Now()
		serverConn.Close()
		clientConn.Close()

		if len(testFront.connectHandler.recordedRequests()) != requestCount+1 || writer.Body.String() != c.answer {
			t.Errorf("%s: answered %q, want %q from connect", c.what, writer.Body.String(), c.answer)
		}
		if !c.bounded {
			if len(writer.readDeadlines) != 0 {
				t.Errorf("%s: read deadlines %v set, want none", c.what, writer.readDeadlines)
			}
			continue
		}
		if len(writer.readDeadlines) != 1 || writer.readDeadlines[0].Before(startTime.Add(lingeringTimeout)) || endTime.Add(lingeringTimeout).Before(writer.readDeadlines[0]) {
			t.Errorf("%s: read deadlines %v set, want one LingeringTimeout %s after connect returned", c.what, writer.readDeadlines, lingeringTimeout)
		}
	}
}
