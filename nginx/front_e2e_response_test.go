package nginx

// End-to-end tests of what the front answers itself, and adds to what it
// relays: CORS and the security headers, the hidden per-block lb routes, the
// lb status json on TCP 80 and 443, port 80's redirects, redirect aliases,
// and the /status deny on service names.

import (
	"net/http"
	"testing"
)

// Fails the test unless header has the security headers a service name's
// locations add, always.
func checkE2eSecurityHeaders(t *testing.T, what string, header http.Header) {
	t.Helper()
	expectedValues := map[string]string{
		"Strict-Transport-Security": responseStrictTransportSecurity,
		"X-Frame-Options":           responseFrameOptionsSameOrigin,
		"X-Content-Type-Options":    responseContentTypeOptions,
		"X-Xss-Protection":          responseXssProtection,
	}
	for name, value := range expectedValues {
		if values := header.Values(name); len(values) != 1 || values[0] != value {
			t.Errorf("%s: %s %q, want %q", what, name, values, value)
		}
	}
}

// A CORS preflight from a listed origin is answered by the front with 204,
// the origin echoed, the generator's methods, headers and max age, Vary:
// Origin and the security headers, and never reaches a block. An error the
// block answers carries the CORS headers and Vary: Origin as well, since
// nginx adds them always; an unlisted origin gets Vary: Origin but no
// Access-Control-Allow-Origin.
func TestFrontE2eAnswersCorsPreflightsAndErrorHeaders(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	client := e2e.newH1Client(t, eth1.httpsAddr, "api.service.example")
	origin := "https://app.service.example"

	requestCounts := e2e.requestCounts()
	preflight := newE2eRequest(t, "OPTIONS", "https://api.service.example/v1/thing", nil)
	preflight.Header.Set("Origin", origin)
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	response, body := e2eDo(t, client, preflight)
	if response.StatusCode != http.StatusNoContent || len(body) != 0 {
		t.Errorf("the preflight = %d with %d bytes, want 204 and none", response.StatusCode, len(body))
	}
	expectedValues := map[string]string{
		"Access-Control-Allow-Origin":   origin,
		"Access-Control-Allow-Methods":  responseCorsAllowMethods,
		"Access-Control-Allow-Headers":  responseCorsAllowHeaders,
		"Access-Control-Expose-Headers": responseCorsExposeHeaders,
		"Access-Control-Max-Age":        responseCorsMaxAge,
		"Vary":                          responseCorsVary,
		"Content-Type":                  responsePreflightContentType,
	}
	for name, value := range expectedValues {
		if got := response.Header.Values(name); len(got) != 1 || got[0] != value {
			t.Errorf("the preflight's %s = %q, want %q", name, got, value)
		}
	}
	checkE2eSecurityHeaders(t, "the preflight", response.Header)
	if requests := e2e.requestsSince(requestCounts); len(requests) != 0 {
		t.Errorf("the preflight reached %v", e2eRequestServices(requests))
	}

	e2e.setServiceHandler(t, "api", func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("failed"))
	})
	for _, c := range []struct {
		origin      string
		allowOrigin []string
	}{
		{origin: origin, allowOrigin: []string{origin}},
		{origin: "https://elsewhere.example", allowOrigin: nil},
	} {
		request := newE2eRequest(t, "GET", "https://api.service.example/v1/fails", nil)
		request.Header.Set("Origin", c.origin)
		response, body := e2eDo(t, client, request)
		what := "an error for origin " + c.origin
		if response.StatusCode != http.StatusInternalServerError || string(body) != "failed" {
			t.Errorf("%s = %d %q, want the block's 500", what, response.StatusCode, body)
		}
		if allowOrigin := response.Header.Values("Access-Control-Allow-Origin"); len(allowOrigin) != len(c.allowOrigin) || len(allowOrigin) == 1 && allowOrigin[0] != c.allowOrigin[0] {
			t.Errorf("%s: Access-Control-Allow-Origin %q, want %q", what, allowOrigin, c.allowOrigin)
		}
		if vary := response.Header.Values("Vary"); len(vary) != 1 || vary[0] != responseCorsVary {
			t.Errorf("%s: Vary %q, want Origin", what, vary)
		}
		if methods := response.Header.Get("Access-Control-Allow-Methods"); methods != responseCorsAllowMethods {
			t.Errorf("%s: Access-Control-Allow-Methods %q", what, methods)
		}
		checkE2eSecurityHeaders(t, what, response.Header)
	}
}

// The hidden per-block lb routes reach exactly the named block and pass its
// answer on, its X-UR-Front-* readiness headers included: on TCP 443 under
// /by/b/<service>/<block>/ and on TCP 80 at its status path, where the
// answer is labeled application/json. A block's 503 is its own result and
// is never retried on another block (D7). /by/service/<service>/ forwards to
// any block with the matched prefix replaced by /.
func TestFrontE2eHiddenBlockRoutesReportTheirBlock(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	for key, upstream := range e2e.upstreams {
		block := upstream.block
		upstream.setHandler(func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
			w.Header().Set("X-UR-Front-Generation", "test-connect-"+block)
			w.Header().Set("X-UR-Front-Ready", "1")
			w.Header().Set("Content-Type", "text/plain")
			if request.requestUri == "/status" && r.Header.Get("X-Test-Unavailable") != "" {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			w.Write([]byte(key))
		})
	}
	httpsClient := e2e.newH1Client(t, eth1.httpsAddr, "test-lb.service.example")
	httpClient := e2e.newHttpClient(t, eth1.httpAddr)
	cases := []struct {
		client *http.Client
		url    string
		// the block that answers, as <service>/<block>
		block       string
		requestUri  string
		status      int
		contentType string
		unavailable bool
	}{
		{client: httpsClient, url: "https://test-lb.service.example/hidden-example/by/b/connect/beta/status", block: "connect/beta", requestUri: "/status", status: http.StatusOK, contentType: "text/plain"},
		{client: httpsClient, url: "https://test-lb.service.example/hidden-example/by/b/connect/g1/status", block: "connect/g1", requestUri: "/status", status: http.StatusOK, contentType: "text/plain"},
		{client: httpsClient, url: "https://test-lb.service.example/hidden-example/by/b/api/g2/status", block: "api/g2", requestUri: "/status", status: http.StatusServiceUnavailable, contentType: "text/plain", unavailable: true},
		{client: httpsClient, url: "https://test-lb.service.example/hidden-example/by/b/api/g1/v1/x?y=1", block: "api/g1", requestUri: "/v1/x?y=1", status: http.StatusOK, contentType: "text/plain"},
		{client: httpClient, url: "http://test-lb.service.example/hidden-example/by/b/connect/beta/status", block: "connect/beta", requestUri: "/status", status: http.StatusOK, contentType: "application/json"},
		{client: httpClient, url: "http://test-lb.other.example/hidden-example/by/b/api/g2/status", block: "api/g2", requestUri: "/status", status: http.StatusOK, contentType: "application/json"},
		{client: httpClient, url: "http://test-lb.service.example/hidden-example/by/b/api/g1/status", block: "api/g1", requestUri: "/status", status: http.StatusServiceUnavailable, contentType: "text/plain", unavailable: true},
	}
	for _, c := range cases {
		requestCounts := e2e.requestCounts()
		request := newE2eRequest(t, "GET", c.url, nil)
		if c.unavailable {
			request.Header.Set("X-Test-Unavailable", "1")
		}
		response, body := e2eDo(t, c.client, request)
		if response.StatusCode != c.status || string(body) != c.block {
			t.Errorf("%s: %d %q, want %d from %s", c.url, response.StatusCode, body, c.status, c.block)
		}
		if response.Header.Get("X-UR-Front-Generation") != "test-connect-"+e2e.upstreams[c.block].block || response.Header.Get("X-UR-Front-Ready") != "1" {
			t.Errorf("%s: X-UR-Front-Generation %q, X-UR-Front-Ready %q, want the block's", c.url, response.Header.Get("X-UR-Front-Generation"), response.Header.Get("X-UR-Front-Ready"))
		}
		if contentType := response.Header.Values("Content-Type"); len(contentType) != 1 || contentType[0] != c.contentType {
			t.Errorf("%s: Content-Type %q, want %q", c.url, contentType, c.contentType)
		}
		requests := e2e.requestsSince(requestCounts)
		if len(requests) != 1 || requests[0].service+"/"+requests[0].block != c.block || requests[0].requestUri != c.requestUri {
			t.Errorf("%s: the blocks got %v, want only %s, for %s", c.url, e2eRequestServices(requests), c.block, c.requestUri)
		}
	}

	requestCounts := e2e.requestCounts()
	response, body := e2eDo(t, httpsClient, newE2eRequest(t, "GET", "https://test-lb.service.example/hidden-example/by/service/web/v2/x", nil))
	requests := e2e.requestsSince(requestCounts)
	if response.StatusCode != http.StatusOK || string(body) != "web/g1" || len(requests) != 1 || requests[0].requestUri != "/v2/x" {
		t.Errorf("by/service: %d %q, the blocks got %v", response.StatusCode, body, e2eRequestServices(requests))
	}
}

// The lb status route answers on lb names, on TCP 80 and 443: 200,
// Content-Type application/json and the status json, never a redirect, since
// Route53 counts a 3xx as healthy (D17). Port 80 answers every other
// request with a 301 to https, for a service name, an lb name's other paths
// and a redirect alias alike, and denies an unknown name. On 443 a redirect
// alias is a 301 to its target. None of these reach a block.
func TestFrontE2eLbStatusAndPort80(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	eth2 := e2e.iface(t, "eth2")
	cases := []struct {
		client   *http.Client
		url      string
		status   int
		location string
		// the status json, when set
		lbStatus bool
	}{
		{client: e2e.newHttpClient(t, eth1.httpAddr), url: "http://test-lb.service.example/hidden-example/status", status: http.StatusOK, lbStatus: true},
		{client: e2e.newHttpClient(t, eth2.httpAddr), url: "http://test-lb.other.example/hidden-example/status", status: http.StatusOK, lbStatus: true},
		{client: e2e.newH1Client(t, eth1.httpsAddr, "test-lb.service.example"), url: "https://test-lb.service.example/hidden-example/status", status: http.StatusOK, lbStatus: true},
		{client: e2e.newH1Client(t, eth2.httpsAddr, "test-lb.other.example"), url: "https://test-lb.other.example/hidden-example/status", status: http.StatusOK, lbStatus: true},
		{client: e2e.newHttpClient(t, eth1.httpAddr), url: "http://api.service.example/v1/x?y=1", status: http.StatusMovedPermanently, location: "https://api.service.example/v1/x?y=1"},
		{client: e2e.newHttpClient(t, eth1.httpAddr), url: "http://test-lb.service.example/other", status: http.StatusMovedPermanently, location: "https://test-lb.service.example/other"},
		{client: e2e.newHttpClient(t, eth1.httpAddr), url: "http://old-api.service.example/z", status: http.StatusMovedPermanently, location: "https://old-api.service.example/z"},
		{client: e2e.newHttpClient(t, eth1.httpAddr), url: "http://unknown.example/", status: http.StatusForbidden},
		{client: e2e.newH1Client(t, eth1.httpsAddr, "old-api.service.example"), url: "https://old-api.service.example/z?w", status: http.StatusMovedPermanently, location: "https://api.service.example/z?w"},
	}
	for _, c := range cases {
		requestCounts := e2e.requestCounts()
		response, body := e2eDo(t, c.client, newE2eRequest(t, "GET", c.url, nil))
		if response.StatusCode != c.status {
			t.Errorf("%s: %d, want %d", c.url, response.StatusCode, c.status)
		}
		if location := response.Header.Get("Location"); location != c.location {
			t.Errorf("%s: Location %q, want %q", c.url, location, c.location)
		}
		if c.lbStatus {
			if contentType := response.Header.Values("Content-Type"); len(contentType) != 1 || contentType[0] != "application/json" {
				t.Errorf("%s: Content-Type %q, want application/json", c.url, contentType)
			}
			if string(body) != e2eLbStatusBody {
				t.Errorf("%s: %q, want the status json", c.url, body)
			}
		}
		if requests := e2e.requestsSince(requestCounts); len(requests) != 0 {
			t.Errorf("%s: reached %v", c.url, e2eRequestServices(requests))
		}
	}
}

// /status is denied on service names whose status is standard, however the
// path is spelled, as nginx matches the normalized uri: a 403 that reaches
// no block and not connect's handler. Paths that only look like it are
// forwarded.
func TestFrontE2eDeniesStatusOnServiceNames(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	cases := []struct {
		sni    string
		target string
		status int
	}{
		{sni: "api.service.example", target: "/status", status: http.StatusForbidden},
		{sni: "api.service.example", target: "//status", status: http.StatusForbidden},
		{sni: "api.service.example", target: "/%73tatus", status: http.StatusForbidden},
		{sni: "api.service.example", target: "/x/../status?y", status: http.StatusForbidden},
		{sni: "web.service.example", target: "/status", status: http.StatusForbidden},
		{sni: "connect.service.example", target: "/status", status: http.StatusForbidden},
		{sni: "api.service.example", target: "/status/", status: http.StatusOK},
		{sni: "api.service.example", target: "/STATUS", status: http.StatusOK},
	}
	for _, c := range cases {
		requestCounts := e2e.requestCounts()
		connectRequestCount := len(e2e.connectHandler.recorded())
		rawConn := e2e.dialRaw(t, eth1.httpsAddr, c.sni)
		response, _ := rawConn.exchange(t, "GET "+c.target+" HTTP/1.1", "Host: "+c.sni)
		if response.StatusCode != c.status {
			t.Errorf("%s%s: %d, want %d", c.sni, c.target, response.StatusCode, c.status)
		}
		reached := len(e2e.requestsSince(requestCounts)) + len(e2e.connectHandler.recorded()) - connectRequestCount
		if c.status == http.StatusForbidden && reached != 0 || c.status == http.StatusOK && reached != 1 {
			t.Errorf("%s%s: reached a handler %d times", c.sni, c.target, reached)
		}
	}
}
