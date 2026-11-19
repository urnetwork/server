package nginx

// Tests of routing by authority (route_http.go, route_http_request.go): one
// case per rule of the nginx generator's servers and locations, over the
// shared route file fixture and fakes of the matcher and the forwarders.

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The status json, byte for byte, with the version the front was built
// with.
const testLbStatusBody = `{"version":"1.2.3","status":"ok","front":"lbalt"}` + "\n"

// Fails unless the response has none of the headers a service name's
// locations add.
func checkNoAddedHeaders(t *testing.T, name string, header http.Header) {
	t.Helper()
	for _, addedHeader := range []string{"Strict-Transport-Security", "X-Frame-Options", "X-Content-Type-Options", "X-XSS-Protection", "Access-Control-Allow-Methods", "Vary"} {
		if values := header.Values(addedHeader); len(values) != 0 {
			t.Errorf("%s: %s %q, want none", name, addedHeader, values)
		}
	}
}

// Fails unless the response has the security headers.
func checkSecurityHeaders(t *testing.T, name string, header http.Header) {
	t.Helper()
	for addedHeader, want := range map[string]string{
		"Strict-Transport-Security": "max-age=63072000; includeSubDomains; preload",
		"X-Frame-Options":           "SAMEORIGIN",
		"X-Content-Type-Options":    "nosniff",
		"X-XSS-Protection":          "1; mode=block",
	} {
		if values := header.Values(addedHeader); !slices.Equal(values, []string{want}) {
			t.Errorf("%s: %s %q, want %q", name, addedHeader, values, want)
		}
	}
}

// On an lb name, p/status answers the status json, exact bytes and type, to
// GET and HEAD, and 405 to other methods, with no added headers, on TCP 443
// and on TCP 80, where it never redirects.
func TestRouteHttpLbStatusAnswersTheStatusJson(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	cases := []struct {
		handler http.Handler
		overTls bool
	}{
		{handler: testFront.front.HttpsHandler("eth1"), overTls: true},
		{handler: testFront.front.HttpHandler("eth1"), overTls: false},
	}
	for _, c := range cases {
		for _, host := range []string{"test-lb.service.example", "test-lb.other.example", "TEST-LB.service.example:7080"} {
			response := serveTestRequest(c.handler, newTestRequest("GET", host, "/hidden-example/status", c.overTls))
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s (tls %t) = %d, want 200", host, c.overTls, response.Code)
			}
			if body := response.Body.String(); body != testLbStatusBody {
				t.Errorf("body %q, want %q", body, testLbStatusBody)
			}
			if values := response.Header().Values("Content-Type"); !slices.Equal(values, []string{"application/json"}) {
				t.Errorf("Content-Type %q, want exactly application/json", values)
			}
			if contentLength := response.Header().Get("Content-Length"); contentLength != "50" {
				t.Errorf("Content-Length %q, want 50", contentLength)
			}
			checkNoAddedHeaders(t, "lb status", response.Header())
		}

		head := serveTestRequest(c.handler, newTestRequest("HEAD", "test-lb.service.example", "/hidden-example/status", c.overTls))
		if head.Code != http.StatusOK || head.Header().Get("Content-Type") != "application/json" {
			t.Errorf("HEAD = %d %q, want 200 json", head.Code, head.Header().Get("Content-Type"))
		}
		for _, method := range []string{"POST", "PUT", "DELETE", "PROPFIND"} {
			response := serveTestRequest(c.handler, newTestRequest(method, "test-lb.service.example", "/hidden-example/status", c.overTls))
			if response.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s = %d, want 405", method, response.Code)
			}
			if response.Header().Get("Content-Type") == "application/json" {
				t.Errorf("%s: the json type is added only to a 200", method)
			}
		}
	}
}

// On an lb name over TCP 443, p/by/service/<svc>/ forwards to an lb_exposed
// service with the matched part replaced by /, p/by/b/<svc>/<block>/ to that
// block only, and the service's route_request_bodies have exact locations
// under both. Everything else is a 404, and nothing has added headers.
func TestRouteHttpLbRoutesForwardWithThePrefixStripped(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	testRouteService(t, routeTable, "mcp").LbExposed = false
	testFront := newTestFront(t, routeTable, testFrontSettings())
	handler := testFront.front.HttpsHandler("eth1")
	settings := testFront.front.settings

	cases := []struct {
		target               string
		service              string
		blockName            string
		upstreamPath         string
		streamed             bool
		websocket            bool
		clientBodyTimeout    time.Duration
		clientBodyBufferSize int64
	}{
		{target: "/hidden-example/by/service/api/v1/x?q=%20", service: "api", upstreamPath: "/v1/x?q=%20", clientBodyTimeout: settings.Body.ClientBodyTimeout, clientBodyBufferSize: settings.Body.MemoryPrefixBytes},
		{target: "/hidden-example/by/service/api/", service: "api", upstreamPath: "/", clientBodyTimeout: settings.Body.ClientBodyTimeout, clientBodyBufferSize: settings.Body.MemoryPrefixBytes},
		{target: "/hidden-example//by/service/api/a%20b/../c", service: "api", upstreamPath: "/c", clientBodyTimeout: settings.Body.ClientBodyTimeout, clientBodyBufferSize: settings.Body.MemoryPrefixBytes},
		{target: "/hidden-example/by/service/api/a%20b", service: "api", upstreamPath: "/a%20b", clientBodyTimeout: settings.Body.ClientBodyTimeout, clientBodyBufferSize: settings.Body.MemoryPrefixBytes},
		{target: "/hidden-example/by/b/api/g2/status", service: "api", blockName: "g2", upstreamPath: "/status", clientBodyTimeout: settings.Body.ClientBodyTimeout, clientBodyBufferSize: settings.Body.MemoryPrefixBytes},
		{target: "/hidden-example/by/service/api/upload/artifact?x", service: "api", upstreamPath: "/upload/artifact?x", clientBodyTimeout: 60 * time.Second, clientBodyBufferSize: 65536},
		{target: "/hidden-example/by/b/api/g1/upload/artifact", service: "api", blockName: "g1", upstreamPath: "/upload/artifact", clientBodyTimeout: 60 * time.Second, clientBodyBufferSize: 65536},
		// the connect blocks' own status, forwarded, never in-process
		{target: "/hidden-example/by/b/connect/beta/status", service: "connect", blockName: "beta", upstreamPath: "/status", websocket: true, clientBodyTimeout: settings.Body.ClientBodyTimeout, clientBodyBufferSize: settings.Body.MemoryPrefixBytes},
	}
	for _, c := range cases {
		response := serveTestRequest(handler, newTestRequest("GET", "test-lb.service.example", c.target, true))
		if response.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", c.target, response.Code)
		}
		checkNoAddedHeaders(t, c.target, response.Header())
		forwards := testFront.forwarders[c.service].recordedForwards()
		if len(forwards) == 0 {
			t.Fatalf("%s reached no %s forwarder", c.target, c.service)
		}
		forward := forwards[len(forwards)-1]
		readTimeout := settings.Forward.ProxyReadTimeout
		if c.websocket || c.streamed {
			readTimeout = settings.Forward.StreamTimeout
		}
		want := forwardOptions{
			interfaceName:        "eth1",
			clientAddrPort:       forward.options.clientAddrPort,
			blockName:            c.blockName,
			upstreamPath:         c.upstreamPath,
			streamed:             c.streamed,
			websocket:            c.websocket,
			clientBodyTimeout:    c.clientBodyTimeout,
			clientBodyBufferSize: c.clientBodyBufferSize,
			readTimeout:          readTimeout,
		}
		// the tunnel hook is checked by the tunnel tests, not here
		options := forward.options
		options.tunnelOpened = nil
		if !reflect.DeepEqual(options, want) {
			t.Errorf("%s: options %+v, want %+v", c.target, options, want)
		}
		if forward.host != "test-lb.service.example" {
			t.Errorf("%s: Host %q, want the lb name", c.target, forward.host)
		}
		if clientAddrPort := forward.options.clientAddrPort.String(); clientAddrPort != "198.51.100.7:40001" {
			t.Errorf("%s: client %s, want the accepted peer", c.target, clientAddrPort)
		}
	}
	if len(testFront.connectHandler.recordedRequests()) != 0 {
		t.Error("an lb route reached the in-process connect handler")
	}

	for _, target := range []string{
		"/",
		"/hidden-example",
		"/hidden-example/",
		"/hidden-example/by/service/api",
		"/hidden-example/by/service/nope/x",
		"/hidden-example/by/b/api/g9/x",
		// not lb_exposed
		"/hidden-example/by/service/mcp/x",
		"/hidden-example/by/b/mcp/g1/x",
		"/by/service/api/x",
	} {
		response := serveTestRequest(handler, newTestRequest("GET", "test-lb.service.example", target, true))
		if response.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", target, response.Code)
		}
		checkNoAddedHeaders(t, target, response.Header())
	}
	if forwards := testFront.forwarders["mcp"].recordedForwards(); len(forwards) != 0 {
		t.Errorf("a service that isn't lb_exposed got %d forwards", len(forwards))
	}
}

// A streamable service's lb routes stream, and so does a streamable_paths
// path, with the rest of the uri sent upstream; other paths stay buffered.
func TestRouteHttpLbRoutesStreamTheStreamablePaths(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	testRouteService(t, routeTable, "api").StreamablePaths = []string{"/stream/[a-z]+"}
	testFront := newTestFront(t, routeTable, testFrontSettings())
	handler := testFront.front.HttpsHandler("eth1")

	cases := []struct {
		target       string
		service      string
		upstreamPath string
		streamed     bool
	}{
		{target: "/hidden-example/by/service/mcp/x", service: "mcp", upstreamPath: "/x", streamed: true},
		{target: "/hidden-example/by/service/api/stream/abc?y", service: "api", upstreamPath: "/stream/abc?y", streamed: true},
		{target: "/hidden-example/by/b/api/g1/stream/abc", service: "api", upstreamPath: "/stream/abc", streamed: true},
		{target: "/hidden-example/by/service/api/stream/ABC", service: "api", upstreamPath: "/stream/ABC", streamed: false},
		{target: "/hidden-example/by/service/api/stream/abc/x", service: "api", upstreamPath: "/stream/abc/x", streamed: false},
	}
	for _, c := range cases {
		response := serveTestRequest(handler, newTestRequest("POST", "test-lb.service.example", c.target, true))
		if response.Code != http.StatusOK {
			t.Fatalf("%s = %d", c.target, response.Code)
		}
		forwards := testFront.forwarders[c.service].recordedForwards()
		forward := forwards[len(forwards)-1]
		if forward.options.upstreamPath != c.upstreamPath || forward.options.streamed != c.streamed {
			t.Errorf("%s: (%q, streamed %t), want (%q, %t)", c.target, forward.options.upstreamPath, forward.options.streamed, c.upstreamPath, c.streamed)
		}
		wantReadTimeout := testFront.front.settings.Forward.ProxyReadTimeout
		if c.streamed {
			wantReadTimeout = testFront.front.settings.Forward.StreamTimeout
		}
		if forward.options.readTimeout != wantReadTimeout {
			t.Errorf("%s: read timeout %s, want %s", c.target, forward.options.readTimeout, wantReadTimeout)
		}
	}
}

// An unknown authority, or none on HTTP/1.0, goes to the default server,
// the first lb name's: its routes answer, and $host is the request's own,
// or the default name when there is none.
func TestRouteHttpUnknownAuthorityGetsTheDefaultServer(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	handler := testFront.front.HttpsHandler("eth1")

	response := serveTestRequest(handler, newTestRequest("GET", "unknown.example", "/hidden-example/status", true))
	if response.Code != http.StatusOK || response.Body.String() != testLbStatusBody {
		t.Fatalf("unknown authority, lb status = %d %q", response.Code, response.Body.String())
	}
	if response := serveTestRequest(handler, newTestRequest("GET", "unknown.example", "/v1/x", true)); response.Code != http.StatusNotFound {
		t.Fatalf("unknown authority, a service path = %d, want the default server's 404", response.Code)
	}
	serveTestRequest(handler, newTestRequest("GET", "unknown.example", "/hidden-example/by/service/api/x", true))
	forwards := testFront.forwarders["api"].recordedForwards()
	if len(forwards) != 1 || forwards[0].host != "unknown.example" {
		t.Fatalf("unknown authority forwarded with Host %v, want the request's own", forwards)
	}

	noHost := newTestRequest("GET", "", "/hidden-example/by/service/api/y", true)
	noHost.Proto = "HTTP/1.0"
	noHost.ProtoMinor = 0
	if response := serveTestRequest(handler, noHost); response.Code != http.StatusOK {
		t.Fatalf("HTTP/1.0 without a host = %d, want the default server", response.Code)
	}
	forwards = testFront.forwarders["api"].recordedForwards()
	if forwards[len(forwards)-1].host != "test-lb.service.example" {
		t.Errorf("no host forwarded with Host %q, want the default server's name", forwards[len(forwards)-1].host)
	}
}

// On a standard-status service's names, p/status is denied with a 403 on
// the normalized uri, as nginx's exact location matches it: //status and
// /%73tatus are denied, /status/ and /STATUS are proxied. The 403 has no
// added headers. A service whose status isn't standard proxies /status.
func TestRouteHttpServiceStatusIsDenied(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	testRouteService(t, routeTable, "mcp").Status = "no"
	testFront := newTestFront(t, routeTable, testFrontSettings())
	handler := testFront.front.HttpsHandler("eth1")

	for _, target := range []string{"/status", "//status", "/%73tatus", "/a/../status", "/status?x=1"} {
		response := serveTestRequest(handler, newTestRequest("GET", "api.service.example", target, true))
		if response.Code != http.StatusForbidden {
			t.Errorf("api %s = %d, want 403", target, response.Code)
		}
		checkNoAddedHeaders(t, "the deny", response.Header())
		response = serveTestRequest(handler, newTestRequest("GET", "connect.service.example", target, true))
		if response.Code != http.StatusForbidden {
			t.Errorf("connect %s = %d, want 403", target, response.Code)
		}
	}
	if forwards := testFront.forwarders["api"].recordedForwards(); len(forwards) != 0 {
		t.Fatalf("a denied /status was forwarded %d times", len(forwards))
	}
	if requests := testFront.connectHandler.recordedRequests(); len(requests) != 0 {
		t.Fatalf("a denied /status reached connect %d times", len(requests))
	}

	for _, target := range []string{"/status/", "/STATUS", "/statusx"} {
		response := serveTestRequest(handler, newTestRequest("GET", "api.service.example", target, true))
		if response.Code != http.StatusOK {
			t.Errorf("api %s = %d, want it proxied", target, response.Code)
		}
	}
	if response := serveTestRequest(handler, newTestRequest("GET", "mcp.service.example", "/status", true)); response.Code != http.StatusOK {
		t.Errorf("a service whose status isn't standard: /status = %d, want it proxied", response.Code)
	}
}

// A service name forwards with nginx's request contract inputs: the
// normalized uri re-escaped, the normalized Host, the service's timeouts,
// and an exact route's own body settings.
func TestRouteHttpServiceNamesForward(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	handler := testFront.front.HttpsHandler("eth2")
	settings := testFront.front.settings

	cases := []struct {
		host                 string
		target               string
		upstreamPath         string
		clientBodyTimeout    time.Duration
		clientBodyBufferSize int64
	}{
		{host: "api.service.example", target: "/v1/x?q=1", upstreamPath: "/v1/x?q=1", clientBodyTimeout: settings.Body.ClientBodyTimeout, clientBodyBufferSize: settings.Body.MemoryPrefixBytes},
		{host: "API.Service.Example:7443", target: "/a/..//v1//x", upstreamPath: "/v1/x", clientBodyTimeout: settings.Body.ClientBodyTimeout, clientBodyBufferSize: settings.Body.MemoryPrefixBytes},
		{host: "test-api.other.example.", target: "/caf%C3%A9", upstreamPath: "/caf%C3%A9", clientBodyTimeout: settings.Body.ClientBodyTimeout, clientBodyBufferSize: settings.Body.MemoryPrefixBytes},
		{host: "api.service.example", target: "/upload/artifact?a=b", upstreamPath: "/upload/artifact?a=b", clientBodyTimeout: 60 * time.Second, clientBodyBufferSize: 65536},
		{host: "api.service.example", target: "/connect/control", upstreamPath: "/connect/control", clientBodyTimeout: settings.Body.ClientBodyTimeout, clientBodyBufferSize: settings.Body.MemoryPrefixBytes},
	}
	for _, c := range cases {
		response := serveTestRequest(handler, newTestRequest("POST", c.host, c.target, true))
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s = %d", c.host, c.target, response.Code)
		}
		forwards := testFront.forwarders["api"].recordedForwards()
		forward := forwards[len(forwards)-1]
		if forward.options.upstreamPath != c.upstreamPath {
			t.Errorf("%s %s: upstream %q, want %q", c.host, c.target, forward.options.upstreamPath, c.upstreamPath)
		}
		if forward.options.clientBodyTimeout != c.clientBodyTimeout || forward.options.clientBodyBufferSize != c.clientBodyBufferSize {
			t.Errorf("%s %s: body (%s, %d), want (%s, %d)", c.host, c.target, forward.options.clientBodyTimeout, forward.options.clientBodyBufferSize, c.clientBodyTimeout, c.clientBodyBufferSize)
		}
		if forward.options.interfaceName != "eth2" || forward.options.blockName != "" || forward.options.streamed || forward.options.websocket {
			t.Errorf("%s %s: options %+v", c.host, c.target, forward.options)
		}
		if strings.ContainsAny(forward.host, ":A") || strings.HasSuffix(forward.host, ".") {
			t.Errorf("%s: forwarded Host %q, want it normalized", c.host, forward.host)
		}
	}
}

// A service's hidden route prefix scopes its locations: the prefix is
// stripped upstream and anything outside it is a 404 with no added headers.
// With a prefix, a streamable_paths path sends the rewritten uri; without
// one, nginx's nested location sends the client's own target.
func TestRouteHttpServicePrefixesAndStreamablePaths(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	api := testRouteService(t, routeTable, "api")
	api.RoutePrefixes = []string{"/svc-hidden"}
	api.StreamablePaths = []string{"/stream/[a-z]+"}
	mcp := testRouteService(t, routeTable, "mcp")
	mcp.Streamable = false
	mcp.StreamablePaths = []string{"/events"}
	testFront := newTestFront(t, routeTable, testFrontSettings())
	handler := testFront.front.HttpsHandler("eth1")

	cases := []struct {
		host         string
		target       string
		service      string
		upstreamPath string
		streamed     bool
	}{
		{host: "api.service.example", target: "/svc-hidden/v1/x?q", service: "api", upstreamPath: "/v1/x?q", streamed: false},
		{host: "api.service.example", target: "/svc-hidden/stream/abc?q=%41", service: "api", upstreamPath: "/stream/abc?q=%41", streamed: true},
		{host: "api.service.example", target: "/svc-hidden/upload/artifact", service: "api", upstreamPath: "/upload/artifact", streamed: false},
		{host: "mcp.service.example", target: "/events?q=%41", service: "mcp", upstreamPath: "/events?q=%41", streamed: true},
		{host: "mcp.service.example", target: "//events?q=%41", service: "mcp", upstreamPath: "//events?q=%41", streamed: true},
		{host: "mcp.service.example", target: "/other", service: "mcp", upstreamPath: "/other", streamed: false},
	}
	for _, c := range cases {
		response := serveTestRequest(handler, newTestRequest("POST", c.host, c.target, true))
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s = %d", c.host, c.target, response.Code)
		}
		forwards := testFront.forwarders[c.service].recordedForwards()
		forward := forwards[len(forwards)-1]
		if forward.options.upstreamPath != c.upstreamPath || forward.options.streamed != c.streamed {
			t.Errorf("%s %s: (%q, streamed %t), want (%q, %t)", c.host, c.target, forward.options.upstreamPath, forward.options.streamed, c.upstreamPath, c.streamed)
		}
	}

	for _, target := range []string{"/v1/x", "/status", "/svc-hidden"} {
		response := serveTestRequest(handler, newTestRequest("GET", "api.service.example", target, true))
		if response.Code != http.StatusNotFound {
			t.Errorf("outside the prefix, %s = %d, want 404", target, response.Code)
		}
		checkNoAddedHeaders(t, target, response.Header())
	}
	if response := serveTestRequest(handler, newTestRequest("GET", "api.service.example", "/svc-hidden/status", true)); response.Code != http.StatusForbidden {
		t.Errorf("the prefix's /status = %d, want 403", response.Code)
	}
}

// An exact location streams when one of the service's streamable_paths
// matches its path whole, as the generator's exact locations do
// (streamablePathsMatch): a route_rate_limits and a route_request_bodies
// path on a service name, and a route_request_bodies path under
// p/by/service and p/by/b, each with the stream timeout and its own body
// settings. An exact path no pattern matches stays buffered.
func TestRouteHttpExactRoutesStreamTheirStreamablePaths(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	api := testRouteService(t, routeTable, "api")
	api.StreamablePaths = []string{"/connect/[a-z]+", "/upload/artifact"}
	api.RouteRequestBodies = append(api.RouteRequestBodies, &RouteRequestBody{
		Path:                     "/upload/buffered",
		ClientBodyBufferSize:     32 * 1024,
		ClientBodyTimeoutSeconds: 30,
	})
	testFront := newTestFront(t, routeTable, testFrontSettings())
	handler := testFront.front.HttpsHandler("eth1")
	settings := testFront.front.settings

	cases := []struct {
		host                 string
		target               string
		blockName            string
		upstreamPath         string
		streamed             bool
		clientBodyBufferSize int64
	}{
		// the route zone's exact location
		{host: "api.service.example", target: "/connect/control", upstreamPath: "/connect/control", streamed: true, clientBodyBufferSize: settings.Body.MemoryPrefixBytes},
		{host: "api.service.example", target: "/upload/artifact?a", upstreamPath: "/upload/artifact?a", streamed: true, clientBodyBufferSize: 65536},
		{host: "api.service.example", target: "/upload/buffered", upstreamPath: "/upload/buffered", streamed: false, clientBodyBufferSize: 32 * 1024},
		{host: "test-lb.service.example", target: "/hidden-example/by/service/api/upload/artifact", upstreamPath: "/upload/artifact", streamed: true, clientBodyBufferSize: 65536},
		{host: "test-lb.service.example", target: "/hidden-example/by/b/api/g1/upload/artifact", blockName: "g1", upstreamPath: "/upload/artifact", streamed: true, clientBodyBufferSize: 65536},
		{host: "test-lb.service.example", target: "/hidden-example/by/b/api/g2/upload/buffered", blockName: "g2", upstreamPath: "/upload/buffered", streamed: false, clientBodyBufferSize: 32 * 1024},
	}
	for _, c := range cases {
		response := serveTestRequest(handler, newTestRequest("POST", c.host, c.target, true))
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s = %d", c.host, c.target, response.Code)
		}
		forwards := testFront.forwarders["api"].recordedForwards()
		options := forwards[len(forwards)-1].options
		readTimeout := settings.Forward.ProxyReadTimeout
		if c.streamed {
			readTimeout = settings.Forward.StreamTimeout
		}
		if options.streamed != c.streamed || options.readTimeout != readTimeout {
			t.Errorf("%s %s: streamed %t, read timeout %s, want %t, %s", c.host, c.target, options.streamed, options.readTimeout, c.streamed, readTimeout)
		}
		if options.upstreamPath != c.upstreamPath || options.blockName != c.blockName || options.clientBodyBufferSize != c.clientBodyBufferSize {
			t.Errorf("%s %s: options %+v, want %q, block %q, buffer %d", c.host, c.target, options, c.upstreamPath, c.blockName, c.clientBodyBufferSize)
		}
	}
}

// A websocket service is forwarded with the websocket mode and the stream
// timeouts, on every request.
func TestRouteHttpWebsocketServiceForwardsWithItsMode(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	mcp := testRouteService(t, routeTable, "mcp")
	mcp.Websocket = true
	mcp.Streamable = false
	testFront := newTestFront(t, routeTable, testFrontSettings())

	r := newTestRequest("GET", "mcp.service.example", "/socket", true)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	serveTestRequest(testFront.front.HttpsHandler("eth1"), r)
	forwards := testFront.forwarders["mcp"].recordedForwards()
	if len(forwards) != 1 {
		t.Fatalf("%d forwards", len(forwards))
	}
	if !forwards[0].options.websocket || forwards[0].options.readTimeout != testFront.front.settings.Forward.StreamTimeout {
		t.Errorf("options %+v, want the websocket mode and the stream timeout", forwards[0].options)
	}
	if forwards[0].header.Get("Upgrade") != "websocket" {
		t.Errorf("the upgrade request lost its Upgrade: %v", forwards[0].header)
	}
}

// Connect's names, an nginx wildcard of several labels included, are served
// in-process, never forwarded, with the ingress rewrite: X-UR-Forwarded-For
// from the accepted peer over the client's, the client's forwarding headers
// and Early-Data removed, Host normalized, and the uri nginx would send.
func TestRouteHttpConnectNamesAreServedInProcess(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	handler := testFront.front.HttpsHandler("eth1")

	for _, host := range []string{"connect.service.example", "CONNECT.service.example:443", "g1.connect.service.example", "a.b.connect.service.example"} {
		r := newTestRequest("GET", host, "//x/../?a=%20", true)
		r.RemoteAddr = "[::ffff:198.51.100.9]:40002"
		r.Header.Set("X-UR-Forwarded-For", "203.0.113.9:1")
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		r.Header.Set("X-Forwarded-Source-Port", "1")
		r.Header.Set("X-UR-Request-Buffering", "off")
		r.Header.Set("Early-Data", "1")
		response := serveTestRequest(handler, r)
		if response.Code != http.StatusOK || response.Body.String() != "connect" {
			t.Fatalf("%s = %d %q, want connect's answer", host, response.Code, response.Body.String())
		}
		requests := testFront.connectHandler.recordedRequests()
		request := requests[len(requests)-1]
		normalizedHost, _ := testFront.front.names.normalizeHost(host)
		if request.host != normalizedHost {
			t.Errorf("%s: Host %q, want %q", host, request.host, normalizedHost)
		}
		if request.path != "/" || request.rawQuery != "a=%20" || request.requestUri != "/?a=%20" {
			t.Errorf("%s: (%q, %q, %q), want the normalized uri", host, request.path, request.rawQuery, request.requestUri)
		}
		if values := request.header.Values("X-UR-Forwarded-For"); !slices.Equal(values, []string{"198.51.100.9:40002"}) {
			t.Errorf("%s: X-UR-Forwarded-For %q, want the accepted peer, unmapped", host, values)
		}
		for _, removed := range []string{"X-Forwarded-For", "X-Forwarded-Source-Port", "X-UR-Request-Buffering", "Early-Data"} {
			if values := request.header.Values(removed); len(values) != 0 {
				t.Errorf("%s: %s %q reached connect", host, removed, values)
			}
		}
		checkSecurityHeaders(t, host, response.Header())
	}
	if forwards := testFront.forwarders["connect"].recordedForwards(); len(forwards) != 0 {
		t.Fatalf("connect's names were forwarded %d times", len(forwards))
	}
}

// On h2 a connect name is served in-process as on h1, with a writer that
// can't hijack, as Go's h2 writer can't; connect then answers as it does to
// a request without an upgrade, as behind nginx.
func TestRouteHttpConnectNamesOverH2AreServedWithoutHijack(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	hijackable := []bool{}
	testFront.connectHandler.serve = func(w http.ResponseWriter, r *http.Request) {
		_, ok := w.(http.Hijacker)
		hijackable = append(hijackable, ok)
		w.WriteHeader(http.StatusBadRequest)
	}
	r := asTestProtocol(newTestRequest("GET", "connect.service.example", "/", true), 2)
	response := serveTestRequest(testFront.front.HttpsHandler("eth1"), r)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("h2 connect = %d, want connect's own answer", response.Code)
	}
	if !slices.Equal(hijackable, []bool{false}) {
		t.Fatalf("connect saw a Hijacker over h2: %v", hijackable)
	}
}

// A redirect alias answers 301 to its target with the client's own request
// uri, unnormalized, and nothing added.
func TestRouteHttpRedirectAliasesAnswer301(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	cases := []struct {
		overTls  bool
		target   string
		location string
	}{
		{overTls: true, target: "/a//b/../c?d=%20", location: "https://api.service.example/a//b/../c?d=%20"},
		{overTls: true, target: "/", location: "https://api.service.example/"},
	}
	for _, c := range cases {
		response := serveTestRequest(testFront.front.HttpsHandler("eth1"), newTestRequest("GET", "OLD-API.service.example", c.target, c.overTls))
		if response.Code != http.StatusMovedPermanently || response.Header().Get("Location") != c.location {
			t.Errorf("%s = %d %q, want 301 %q", c.target, response.Code, response.Header().Get("Location"), c.location)
		}
		checkNoAddedHeaders(t, "redirect", response.Header())
	}
	if forwards := testFront.forwarders["api"].recordedForwards(); len(forwards) != 0 {
		t.Errorf("a redirect alias was forwarded %d times", len(forwards))
	}
}

// TCP 80: on lb names the status json and each block's status route; a 301
// to https for every other path of an lb name and every path of a service
// name or redirect alias; a 403 for unknown names.
func TestRouteHttpPort80(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	// the block status routes exist for every service, lb_exposed or not
	testRouteService(t, routeTable, "mcp").LbExposed = false
	testFront := newTestFront(t, routeTable, testFrontSettings())
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		w.Header().Set("Content-Type", "text/plain")
		if options.blockName == "g2" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		w.Write([]byte(`{"status":"ok"}`))
	}
	handler := testFront.front.HttpHandler("eth1")

	response := serveTestRequest(handler, newTestRequest("GET", "test-lb.service.example", "/hidden-example/status", false))
	if response.Code != http.StatusOK || response.Body.String() != testLbStatusBody {
		t.Fatalf("port 80 lb status = %d %q", response.Code, response.Body.String())
	}

	response = serveTestRequest(handler, newTestRequest("GET", "test-lb.service.example", "/hidden-example/by/b/api/g1/status?x", false))
	if response.Code != http.StatusOK {
		t.Fatalf("port 80 block status = %d", response.Code)
	}
	if values := response.Header().Values("Content-Type"); !slices.Equal(values, []string{"application/json"}) {
		t.Errorf("block status Content-Type %q, want application/json on a 200", values)
	}
	forwards := testFront.forwarders["api"].recordedForwards()
	if forward := forwards[len(forwards)-1]; forward.options.blockName != "g1" || forward.options.upstreamPath != "/status?x" {
		t.Errorf("block status forwarded as %+v", forward.options)
	}
	response = serveTestRequest(handler, newTestRequest("GET", "test-lb.service.example", "/hidden-example/by/b/api/g2/status", false))
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Content-Type") != "text/plain" {
		t.Errorf("a 503 block status = %d %q, want the upstream's type kept", response.Code, response.Header().Get("Content-Type"))
	}
	if response := serveTestRequest(handler, newTestRequest("GET", "test-lb.service.example", "/hidden-example/by/b/mcp/g1/status", false)); response.Code != http.StatusOK {
		t.Errorf("a block status of a service that isn't lb_exposed = %d, want 200", response.Code)
	}

	redirects := []struct {
		host     string
		target   string
		location string
	}{
		{host: "test-lb.service.example", target: "/x//y?z=1", location: "https://test-lb.service.example/x//y?z=1"},
		{host: "test-lb.service.example", target: "/hidden-example/by/service/api/status", location: "https://test-lb.service.example/hidden-example/by/service/api/status"},
		{host: "Api.Service.Example:80", target: "/v1?a", location: "https://api.service.example/v1?a"},
		{host: "connect.service.example", target: "/", location: "https://connect.service.example/"},
		{host: "g1.connect.service.example", target: "/status", location: "https://g1.connect.service.example/status"},
		{host: "old-api.service.example", target: "/q", location: "https://old-api.service.example/q"},
	}
	for _, c := range redirects {
		response := serveTestRequest(handler, newTestRequest("GET", c.host, c.target, false))
		if response.Code != http.StatusMovedPermanently || response.Header().Get("Location") != c.location {
			t.Errorf("port 80 %s %s = %d %q, want 301 %q", c.host, c.target, response.Code, response.Header().Get("Location"), c.location)
		}
		checkNoAddedHeaders(t, "port 80 redirect", response.Header())
	}

	for _, host := range []string{"unknown.example", "192.0.2.10"} {
		if response := serveTestRequest(handler, newTestRequest("GET", host, "/hidden-example/status", false)); response.Code != http.StatusForbidden {
			t.Errorf("port 80 unknown name %s = %d, want 403", host, response.Code)
		}
	}
}

// h3 serves api authorities only: a request for any other authority on an
// api connection is a 421, so it can't reach another service, and /status
// is alt's 404.
func TestRouteHttpH3ServesOnlyApiAuthorities(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	handler := testFront.front.H3ApiHandler("eth1")

	for _, host := range []string{"api.service.example", "test-api.other.example"} {
		response := serveTestRequest(handler, asTestProtocol(newTestRequest("GET", host, "/v1/x", true), 3))
		if response.Code != http.StatusOK {
			t.Errorf("h3 %s = %d, want it forwarded", host, response.Code)
		}
	}
	if forwards := testFront.forwarders["api"].recordedForwards(); len(forwards) != 2 {
		t.Fatalf("%d api forwards, want 2", len(forwards))
	}
	for _, host := range []string{"connect.service.example", "mcp.service.example", "test-lb.service.example", "old-api.service.example", "unknown.example"} {
		response := serveTestRequest(handler, asTestProtocol(newTestRequest("GET", host, "/hidden-example/status", true), 3))
		if response.Code != http.StatusMisdirectedRequest {
			t.Errorf("h3 %s = %d, want 421", host, response.Code)
		}
	}
	for _, target := range []string{"/status", "//status"} {
		response := serveTestRequest(handler, asTestProtocol(newTestRequest("GET", "api.service.example", target, true), 3))
		if response.Code != http.StatusNotFound {
			t.Errorf("h3 api %s = %d, want 404", target, response.Code)
		}
	}
	if len(testFront.forwarders["mcp"].recordedForwards()) != 0 || len(testFront.connectHandler.recordedRequests()) != 0 {
		t.Error("an h3 request reached a service other than the api")
	}
}

// nginx's request validation, before any server: a method outside
// [A-Z_-]+, an invalid Host, TRACE and CONNECT, a request line or headers
// over the limits, a target nginx can't parse, and an h2 or h3 :authority
// that differs from Host. A refused request is never forwarded, and on h1
// the front's own 400 and 414 close the connection, as nginx's do; h2 and
// h3 get no Connection header, and 405 keeps the connection. Header names
// nginx ignores are dropped.
func TestRouteHttpRequestValidation(t *testing.T) {
	testFront := newTestFront(t, loadTestRouteTable(t), testFrontSettings())
	handler := testFront.front.HttpsHandler("eth1")

	cases := []struct {
		name       string
		request    func() *http.Request
		statusCode int
		// Connection: close
		closes bool
	}{
		{name: "a lowercase method", request: func() *http.Request { return newTestRequest("get", "api.service.example", "/v1", true) }, statusCode: 400, closes: true},
		{name: "a mixed case method", request: func() *http.Request { return newTestRequest("Get", "api.service.example", "/v1", true) }, statusCode: 400, closes: true},
		{name: "a method with a digit", request: func() *http.Request { return newTestRequest("M2", "api.service.example", "/v1", true) }, statusCode: 400, closes: true},
		{name: "a method with a dot", request: func() *http.Request { return newTestRequest("FOO.BAR", "api.service.example", "/v1", true) }, statusCode: 400, closes: true},
		{name: "an invalid method on a request line over 8 KiB", request: func() *http.Request {
			return newTestRequest("get", "api.service.example", "/"+strings.Repeat("a", 8*1024), true)
		}, statusCode: 400, closes: true},
		{name: "an h2 lowercase method", request: func() *http.Request {
			return asTestProtocol(newTestRequest("post", "api.service.example", "/v1", true), 2)
		}, statusCode: 400, closes: false},
		{name: "invalid host", request: func() *http.Request { return newTestRequest("GET", "api..service.example", "/", true) }, statusCode: 400, closes: true},
		{name: "host with a slash", request: func() *http.Request { return newTestRequest("GET", "api.service.example/x", "/", true) }, statusCode: 400, closes: true},
		{name: "trace", request: func() *http.Request { return newTestRequest("TRACE", "api.service.example", "/", true) }, statusCode: 405, closes: false},
		{name: "connect", request: func() *http.Request {
			r := newTestRequest("CONNECT", "api.service.example", "/", true)
			r.RequestURI = "api.service.example:443"
			return r
		}, statusCode: 405, closes: false},
		{name: "asterisk", request: func() *http.Request {
			r := newTestRequest("OPTIONS", "api.service.example", "/", true)
			r.RequestURI = "*"
			return r
		}, statusCode: 400, closes: true},
		{name: "bad escape", request: func() *http.Request {
			r := newTestRequest("GET", "api.service.example", "/", true)
			r.RequestURI = "/%zz"
			return r
		}, statusCode: 400, closes: true},
		{name: "above the root", request: func() *http.Request { return newTestRequest("GET", "api.service.example", "/a/../..", true) }, statusCode: 400, closes: true},
		{name: "request line over 8 KiB", request: func() *http.Request {
			return newTestRequest("GET", "api.service.example", "/"+strings.Repeat("a", 8*1024), true)
		}, statusCode: 414, closes: true},
		{name: "a header over 8 KiB", request: func() *http.Request {
			r := newTestRequest("GET", "api.service.example", "/", true)
			r.Header.Set("X-Big", strings.Repeat("a", 8*1024))
			return r
		}, statusCode: 400, closes: true},
		{name: "headers over 64 KiB", request: func() *http.Request {
			r := newTestRequest("GET", "api.service.example", "/", true)
			for i := range 10 {
				r.Header.Set("X-Big-"+string(rune('a'+i)), strings.Repeat("a", 7*1024))
			}
			return r
		}, statusCode: 400, closes: true},
		{name: "h2 authority and a different Host", request: func() *http.Request {
			r := asTestProtocol(newTestRequest("GET", "api.service.example", "/", true), 2)
			r.Header.Set("Host", "mcp.service.example")
			return r
		}, statusCode: 400, closes: false},
		{name: "h3 authority and a different Host", request: func() *http.Request {
			r := asTestProtocol(newTestRequest("GET", "api.service.example", "/", true), 3)
			r.Header.Set("Host", "connect.service.example")
			return r
		}, statusCode: 400, closes: false},
		{name: "h2 with neither", request: func() *http.Request {
			return asTestProtocol(newTestRequest("GET", "", "/", true), 2)
		}, statusCode: 400, closes: false},
	}
	for _, c := range cases {
		response := serveTestRequest(handler, c.request())
		if response.Code != c.statusCode {
			t.Errorf("%s = %d, want %d", c.name, response.Code, c.statusCode)
		}
		checkNoAddedHeaders(t, c.name, response.Header())
		if values := response.Header().Values("Connection"); c.closes && !slices.Equal(values, []string{"close"}) || !c.closes && len(values) != 0 {
			t.Errorf("%s: Connection %q, want close %t", c.name, values, c.closes)
		}
	}
	if forwards := testFront.forwarders["api"].recordedForwards(); len(forwards) != 0 {
		t.Errorf("%d refused requests were forwarded", len(forwards))
	}

	// what nginx accepts: any method of [A-Z_-]+, a Host equal to the h2
	// :authority, and a header just under the limit
	accepted := []*http.Request{
		newTestRequest("FOO_BAR-X", "api.service.example", "/v1", true),
		func() *http.Request {
			r := asTestProtocol(newTestRequest("GET", "api.service.example", "/", true), 2)
			r.Header.Set("Host", "api.service.example")
			return r
		}(),
		func() *http.Request {
			r := newTestRequest("GET", "api.service.example", "/", true)
			r.Header.Set("X-Big", strings.Repeat("a", 8*1024-len("X-Big: \r\n")))
			return r
		}(),
	}
	for _, r := range accepted {
		method := r.Method
		if response := serveTestRequest(handler, r); response.Code != http.StatusOK || response.Header().Get("Connection") != "" {
			t.Errorf("%s %s = %d %q, want it forwarded", method, r.URL.Path, response.Code, response.Header().Get("Connection"))
		}
	}
	if forwards := testFront.forwarders["api"].recordedForwards(); len(forwards) != len(accepted) || forwards[0].method != "FOO_BAR-X" {
		t.Errorf("forwards %d, want the %d accepted requests", len(forwards), len(accepted))
	}
	h3 := testFront.front.H3ApiHandler("eth1")
	if response := serveTestRequest(h3, asTestProtocol(newTestRequest("get", "api.service.example", "/v1", true), 3)); response.Code != http.StatusBadRequest {
		t.Errorf("an h3 lowercase method = %d, want 400", response.Code)
	}

	r := asTestProtocol(newTestRequest("GET", "api.service.example", "/headers", true), 2)
	r.Header.Set("Host", "api.service.example")
	r.Header["X_underscore"] = []string{"1"}
	r.Header["X.dot"] = []string{"1"}
	r.Header["X-Kept-9"] = []string{"1"}
	serveTestRequest(handler, r)
	forwards := testFront.forwarders["api"].recordedForwards()
	header := forwards[len(forwards)-1].header
	for _, name := range []string{"X_underscore", "X.dot", "Host"} {
		if _, ok := header[name]; ok {
			t.Errorf("%s reached the forwarder", name)
		}
	}
	if header.Get("X-Kept-9") != "1" {
		t.Errorf("a valid header was dropped: %v", header)
	}
}
