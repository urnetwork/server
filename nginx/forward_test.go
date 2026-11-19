package nginx

// Tests of nginx's request contract and the response filtering: what a
// block receives for a client's request, and what the client receives for
// a block's response.

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// Sends raw request bytes to the front and reads the final response,
// skipping informational ones.
func rawFrontRequest(t *testing.T, front *testFront, rawRequest string) (*http.Response, []byte) {
	conn, err := net.Dial("tcp", front.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, rawRequest); err != nil {
		t.Fatalf("write request: %v", err)
	}
	reader := bufio.NewReader(conn)
	for {
		response, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		if response.StatusCode < http.StatusOK {
			continue
		}
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("read response body: %v", err)
		}
		return response, body
	}
}

// The whole contract on one request: Host normalized from the client's
// authority, X-UR-Forwarded-For from the accepted peer, the ingress and
// hop-by-hop headers removed, Early-Data never sent, underscore and other
// invalid names dropped, the client's other headers passed line by line, a
// chunked body sent with a regenerated Content-Length, and the path and
// query exactly as escaped.
func TestForwardRequestContract(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testRouteService("contract", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testFrontSettings())
	front := newTestFront(t, group, testForwardOptions)

	rawRequest := strings.Join([]string{
		"POST /a/b%20c/../d?x=1&y=%2F HTTP/1.1",
		"Host: API.Service.Example:8443",
		"Transfer-Encoding: chunked",
		"Content-Type: text/plain",
		"Connection: keep-alive, Upgrade, X-Hop",
		"Upgrade: websocket",
		"Keep-Alive: timeout=5",
		"Proxy-Connection: keep-alive",
		"TE: trailers",
		"Trailer: X-Trailer",
		"Expect: 100-continue",
		"X-UR-Forwarded-For: 203.0.113.99:1",
		"X-Forwarded-For: 203.0.113.98",
		"X-Forwarded-Source-Port: 9",
		"X-UR-Request-Buffering: off",
		"Early-Data: 1",
		"X_Under: dropped",
		"X.Dot: dropped",
		"X-Hop: passed",
		"X-Multi: a",
		"X-Multi: b",
		"Authorization: Bearer test-token",
		"Proxy-Authorization: Basic dGVzdA==",
		"",
		"5",
		"hello",
		"6",
		" world",
		"0",
		"",
		"",
	}, "\r\n")
	response, body := rawFrontRequest(t, front, rawRequest)
	if response.StatusCode != http.StatusOK || string(body) != "g1" {
		t.Fatalf("response %d %q, want 200 from g1", response.StatusCode, body)
	}

	recorded := upstream.recorded()
	if len(recorded) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(recorded))
	}
	request := recorded[0]
	if request.proto != "HTTP/1.1" {
		t.Errorf("upstream protocol %q, want HTTP/1.1", request.proto)
	}
	if request.requestUri != "/a/b%20c/../d?x=1&y=%2F" {
		t.Errorf("upstream request uri %q, want the escaped path and raw query", request.requestUri)
	}
	if request.host != "api.service.example" {
		t.Errorf("upstream Host %q, want the normalized authority", request.host)
	}
	if string(request.body) != "hello world" || request.contentLength != 11 || len(request.transferEncoding) != 0 {
		t.Errorf("upstream body %q length %d encoding %v, want the dechunked body with its length", request.body, request.contentLength, request.transferEncoding)
	}
	wantHeader := http.Header{
		"Connection":          {"keep-alive"},
		"X-Ur-Forwarded-For":  {"192.0.2.10:40123"},
		"Content-Length":      {"11"},
		"Content-Type":        {"text/plain"},
		"X-Hop":               {"passed"},
		"X-Multi":             {"a", "b"},
		"Authorization":       {"Bearer test-token"},
		"Proxy-Authorization": {"Basic dGVzdA=="},
	}
	if !reflect.DeepEqual(request.header, wantHeader) {
		t.Errorf("upstream headers\n%v\nwant\n%v", request.header, wantHeader)
	}
}

// X-UR-Forwarded-For carries nginx's text of the peer: ipv6 bracketed, an
// ipv4-compatible address in its dotted form, an ipv4-mapped one as ipv4, as
// the ingress helper writes it, and no zone.
func TestForwardedForAddresses(t *testing.T) {
	cases := []struct {
		clientAddrPort netip.AddrPort
		want           string
	}{
		{clientAddrPort: netip.MustParseAddrPort("192.0.2.10:40123"), want: "192.0.2.10:40123"},
		{clientAddrPort: netip.MustParseAddrPort("[2001:db8::10]:443"), want: "[2001:db8::10]:443"},
		{clientAddrPort: netip.MustParseAddrPort("[2001:db8:0:1::1]:1"), want: "[2001:db8:0:1::1]:1"},
		{clientAddrPort: netip.MustParseAddrPort("[::c000:20a]:443"), want: "[::192.0.2.10]:443"},
		{clientAddrPort: netip.MustParseAddrPort("[::ffff:192.0.2.10]:443"), want: "192.0.2.10:443"},
		{clientAddrPort: netip.AddrPortFrom(netip.MustParseAddr("fe80::1").WithZone("eth1"), 443), want: "[fe80::1]:443"},
		{clientAddrPort: netip.AddrPort{}, want: ""},
	}
	for _, c := range cases {
		if got := forwardedForValue(c.clientAddrPort); got != c.want {
			t.Errorf("forwardedForValue(%v) = %q, want %q", c.clientAddrPort, got, c.want)
		}
	}

	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testRouteService("forwarded-for", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testFrontSettings())
	front := newTestFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.clientAddrPort = netip.MustParseAddrPort("[2001:db8::10]:443")
		return options
	})
	request, _ := http.NewRequest(http.MethodGet, front.url("/"), nil)
	request.Header.Set(urForwardedForHeader, "198.51.100.1:2")
	if response, _, err := testDo(t, request); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("request: %v %v", response, err)
	}
	if got := upstream.recorded()[0].header.Values(urForwardedForHeader); !reflect.DeepEqual(got, []string{"[2001:db8::10]:443"}) {
		t.Errorf("upstream X-UR-Forwarded-For %v, want the bracketed ipv6 peer only", got)
	}
}

// nginx's ngx_inet6_ntop, against values printed by that function compiled
// from the pinned nginx source (src/core/ngx_inet.c), including the dotted
// forms netip writes as hex.
func TestNginxInet6Ntop(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{addr: "::", want: "::"},
		{addr: "::1", want: "::1"},
		{addr: "::2", want: "::2"},
		{addr: "::100", want: "::0.0.1.0"},
		{addr: "::101", want: "::101"},
		{addr: "::102", want: "::0.0.1.2"},
		{addr: "::c000:201", want: "::192.0.2.1"},
		{addr: "::ffff:c000:201", want: "::ffff:192.0.2.1"},
		{addr: "2001:db8::1", want: "2001:db8::1"},
		{addr: "2001:db8:0:1:0:0:0:1", want: "2001:db8:0:1::1"},
		{addr: "2001:db8:0:0:1:0:0:1", want: "2001:db8::1:0:0:1"},
		{addr: "2001:db8:1:2:3:4:5:6", want: "2001:db8:1:2:3:4:5:6"},
		{addr: "fe80::1", want: "fe80::1"},
		{addr: "1::", want: "1::"},
		{addr: "1:0:0:2:0:0:0:3", want: "1:0:0:2::3"},
		{addr: "0:0:1::", want: "0:0:1::"},
		{addr: "::ffff:0:0", want: "::ffff:0.0.0.0"},
		{addr: "0:0:0:0:0:1:0:0", want: "::1:0:0"},
		{addr: "2001:db8:0:0:0:0:2:1", want: "2001:db8::2:1"},
		{addr: "::1:0:0:0", want: "::1:0:0:0"},
		{addr: "0:0:0:0:1:ffff:c000:201", want: "::1:ffff:c000:201"},
		{addr: "ffff::ffff:c000:201", want: "ffff::ffff:c000:201"},
	}
	for _, c := range cases {
		if got := nginxInet6Ntop(netip.MustParseAddr(c.addr).As16()); got != c.want {
			t.Errorf("nginxInet6Ntop(%s) = %q, want %q", c.addr, got, c.want)
		}
	}
	// the remote_addr text: ipv4 dotted, a mapped address as ipv4
	if got := nginxRemoteAddrText(netip.MustParseAddr("::ffff:198.51.100.7")); got != "198.51.100.7" {
		t.Errorf("mapped remote addr text %q, want 198.51.100.7", got)
	}
	if got := nginxRemoteAddrText(netip.Addr{}); got != "" {
		t.Errorf("invalid remote addr text %q, want empty", got)
	}
}

// Routing's normalized host and path win over the request's own, and are
// sent exactly.
func TestForwardRoutedHostAndPath(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testRouteService("routed", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testFrontSettings())
	front := newTestFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.host = "api.service.example"
		options.upstreamPath = "/v1/x%2Fy?z=1"
		return options
	})
	request, _ := http.NewRequest(http.MethodGet, front.url("/hidden-example/by/service/routed/v1/x%2Fy?z=1"), nil)
	request.Host = "Other.Service.Example:7443"
	if response, _, err := testDo(t, request); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("request: %v %v", response, err)
	}
	recorded := upstream.recorded()[0]
	if recorded.host != "api.service.example" || recorded.requestUri != "/v1/x%2Fy?z=1" {
		t.Errorf("upstream Host %q uri %q, want routing's", recorded.host, recorded.requestUri)
	}
}

// The Host derived from a request's authority, as nginx's $host is for any
// host routing accepts.
func TestForwardHost(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{host: "API.Service.Example", want: "api.service.example"},
		{host: "api.service.example:8443", want: "api.service.example"},
		{host: "api.service.example.", want: "api.service.example"},
		{host: "api.service.example.:443", want: "api.service.example"},
		{host: "[2001:DB8::1]:443", want: "[2001:db8::1]"},
		{host: "192.0.2.1:80", want: "192.0.2.1"},
	}
	for _, c := range cases {
		r := &http.Request{Host: c.host}
		if got := forwardHost(r, &forwardOptions{}); got != c.want {
			t.Errorf("forwardHost(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

// A request with no body gets no framing headers, and a declared zero
// length is sent as one, as nginx's $proxy_internal_body_length.
func TestForwardBodyFramingHeaders(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testRouteService("framing", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testFrontSettings())
	front := newTestFront(t, group, testForwardOptions)

	rawFrontRequest(t, front, "GET / HTTP/1.1\r\nHost: framing.service.example\r\n\r\n")
	rawFrontRequest(t, front, "POST / HTTP/1.1\r\nHost: framing.service.example\r\nContent-Length: 0\r\n\r\n")
	recorded := upstream.recorded()
	if len(recorded) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(recorded))
	}
	if _, ok := recorded[0].header[contentLengthHeader]; ok || len(recorded[0].transferEncoding) != 0 {
		t.Errorf("a GET was framed: %v %v", recorded[0].header, recorded[0].transferEncoding)
	}
	if got := recorded[1].header.Values(contentLengthHeader); !reflect.DeepEqual(got, []string{"0"}) {
		t.Errorf("a declared empty body sent Content-Length %v, want 0", got)
	}
}

// A streamed route marks the request and streams the body: chunked when its
// length is unknown, with its length when declared.
func TestForwardStreamedRouteHeaders(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testRouteService("streamed-headers", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testFrontSettings())
	front := newTestFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		return options
	})
	rawFrontRequest(t, front, "POST /mcp HTTP/1.1\r\nHost: streamed.service.example\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\n\r\n")
	rawFrontRequest(t, front, "POST /mcp HTTP/1.1\r\nHost: streamed.service.example\r\nContent-Length: 3\r\n\r\nxyz")
	recorded := upstream.recorded()
	if len(recorded) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(recorded))
	}
	for _, request := range recorded {
		if got := request.header.Values(urRequestBufferingHeader); !reflect.DeepEqual(got, []string{"off"}) {
			t.Errorf("streamed request X-UR-Request-Buffering %v, want off", got)
		}
	}
	if !reflect.DeepEqual(recorded[0].transferEncoding, []string{"chunked"}) || string(recorded[0].body) != "abc" {
		t.Errorf("unknown length sent as %v %q, want chunked abc", recorded[0].transferEncoding, recorded[0].body)
	}
	if recorded[1].contentLength != 3 || len(recorded[1].transferEncoding) != 0 || string(recorded[1].body) != "xyz" {
		t.Errorf("declared length sent as %d %v %q, want 3 xyz", recorded[1].contentLength, recorded[1].transferEncoding, recorded[1].body)
	}
}

// A websocket service's requests carry Connection: upgrade, and the client's
// Upgrade only when it sent one, as nginx's websocket locations do.
func TestForwardWebsocketServiceHeaders(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testRouteService("websocket-headers", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testFrontSettings())
	front := newTestFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.websocket = true
		return options
	})
	rawFrontRequest(t, front, "GET /plain HTTP/1.1\r\nHost: ws.service.example\r\n\r\n")
	rawFrontRequest(t, front, "GET /socket HTTP/1.1\r\nHost: ws.service.example\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	recorded := upstream.recorded()
	if len(recorded) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(recorded))
	}
	for _, request := range recorded {
		if got := request.header.Values(connectionHeader); !reflect.DeepEqual(got, []string{"upgrade"}) {
			t.Errorf("%s Connection %v, want upgrade", request.requestUri, got)
		}
	}
	if got := recorded[0].header.Values(upgradeHeader); len(got) != 0 {
		t.Errorf("a plain request sent Upgrade %v", got)
	}
	if got := recorded[1].header.Values(upgradeHeader); !reflect.DeepEqual(got, []string{"websocket"}) {
		t.Errorf("an upgrade request sent Upgrade %v, want websocket", got)
	}
}

// The response loses the headers nginx hides (Date and Server replaced,
// X-Pad and its five X-Accel-* headers) and its framing headers; the rest
// pass, another X-Accel-* header included.
func TestForwardResponseHiddenHeaders(t *testing.T) {
	upstreamDate := "Mon, 01 Jan 2001 00:00:00 GMT"
	upstream := newRecordingUpstream(t, "g1", func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
		header := w.Header()
		header.Set("Server", "upstream-server")
		header.Set("Date", upstreamDate)
		header.Set("X-Pad", "avoid browser bug")
		header.Set("X-Accel-Expires", "1")
		header.Set("X-Accel-Redirect", "/internal")
		header.Set("X-Accel-Limit-Rate", "1")
		header.Set("X-Accel-Buffering", "no")
		header.Set("X-Accel-Charset", "utf-8")
		header.Set("X-Accel-Other", "passed")
		header.Set("Keep-Alive", "timeout=1")
		header.Add("X-Upstream", "a")
		header.Add("X-Upstream", "b")
		io.WriteString(w, "body")
	})
	group, _ := newTestGroup(t, testRouteService("hidden", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testFrontSettings())
	front := newTestFront(t, group, testForwardOptions)
	request, _ := http.NewRequest(http.MethodGet, front.url("/"), nil)
	response, body, err := testDo(t, request)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "body" {
		t.Fatalf("response %v %q %v", response, body, err)
	}
	for _, hidden := range []string{"Server", "X-Pad", "X-Accel-Expires", "X-Accel-Redirect", "X-Accel-Limit-Rate", "X-Accel-Buffering", "X-Accel-Charset", "Keep-Alive"} {
		if values := response.Header.Values(hidden); len(values) != 0 {
			t.Errorf("hidden header %s reached the client: %v", hidden, values)
		}
	}
	if date := response.Header.Get("Date"); date == "" || date == upstreamDate {
		t.Errorf("Date %q, want the front's own", date)
	}
	if got := response.Header.Values("X-Upstream"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("X-Upstream %v, want both values", got)
	}
	if got := response.Header.Get("X-Accel-Other"); got != "passed" {
		t.Errorf("X-Accel-Other %q, want it passed", got)
	}
	if response.ContentLength != 4 {
		t.Errorf("Content-Length %d, want the upstream's 4", response.ContentLength)
	}
}

// A response head of exactly MaxUpstreamResponseHeaderBytes passes; one byte
// more is a 502, never retried, as nginx's "upstream sent too big header".
func TestForwardResponseHeadLimit(t *testing.T) {
	settings := testFrontSettings()
	maxHeadBytes := settings.Forward.MaxUpstreamResponseHeaderBytes
	headOfSize := func(size int) string {
		prefix := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nX-Fill: "
		suffix := "\r\n\r\n"
		return prefix + strings.Repeat("f", size-len(prefix)-len(suffix)) + suffix
	}

	for _, c := range []struct {
		headBytes  int
		wantStatus int
	}{
		{headBytes: maxHeadBytes, wantStatus: http.StatusOK},
		{headBytes: maxHeadBytes + 1, wantStatus: http.StatusBadGateway},
	} {
		raw := newRawUpstream(t)
		other := newRecordingUpstream(t, "g2", nil)
		serviceName := "head-limit-" + strings.Repeat("x", c.headBytes-maxHeadBytes)
		group, _ := newTestGroup(t, testRouteService(serviceName, []testBlock{
			{name: "g1", weight: 1, port: raw.port},
			{name: "g2", weight: 1, port: other.port},
		}), settings)
		// the first pick of two idle blocks of equal weight is the first
		front := newTestFront(t, group, testForwardOptions)
		responses := make(chan *http.Response, 1)
		go func() {
			request, _ := http.NewRequest(http.MethodGet, front.url("/"), nil)
			response, _, _ := testDo(t, request)
			responses <- response
		}()
		conn, _, _ := raw.accept(t)
		io.WriteString(conn, headOfSize(c.headBytes)+"ok")
		response := <-responses
		if response == nil || response.StatusCode != c.wantStatus {
			t.Fatalf("head of %d bytes: response %v, want %d", c.headBytes, response, c.wantStatus)
		}
		if len(other.recorded()) != 0 {
			t.Errorf("head of %d bytes was retried on another block", c.headBytes)
		}
		if c.wantStatus == http.StatusBadGateway && attemptCount(serviceName, "g1", "invalid_header") != 1 {
			t.Errorf("an oversized head was not counted as invalid_header")
		}
	}
}

// The upstream request line keeps a target routing hands over exactly, and
// escapes bytes that can't sit in a request line.
func TestForwardTarget(t *testing.T) {
	cases := []struct {
		upstreamPath string
		want         string
	}{
		{upstreamPath: "/v1/a?b=c", want: "/v1/a?b=c"},
		{upstreamPath: "/a b?c d", want: "/a%20b?c%20d"},
		{upstreamPath: "/tab\there", want: "/tab%09here"},
	}
	for _, c := range cases {
		r, _ := http.NewRequest(http.MethodGet, "http://api.service.example/", nil)
		if got := forwardTarget(r, &forwardOptions{upstreamPath: c.upstreamPath}); got != c.want {
			t.Errorf("forwardTarget(%q) = %q, want %q", c.upstreamPath, got, c.want)
		}
	}
	r := &http.Request{URL: &url.URL{}}
	if got := forwardTarget(r, &forwardOptions{}); got != "/" {
		t.Errorf("an empty target %q, want /", got)
	}
}

// The response head is sent before any body arrives, as nginx sends it
// unbuffered, so a client of a streaming response sees it at once.
func TestForwardResponseHeadSentBeforeBody(t *testing.T) {
	release := make(chan struct{})
	upstream := newRecordingUpstream(t, "g1", func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: late\n\n")
	})
	group, _ := newTestGroup(t, testRouteService("head-first", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testFrontSettings())
	front := newTestFront(t, group, testForwardOptions)
	request, _ := http.NewRequest(http.MethodGet, front.url("/events"), nil)
	response, err := testClient().Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer response.Body.Close()
	// the head arrived while the upstream still holds its body
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("head %d %v", response.StatusCode, response.Header)
	}
	close(release)
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "data: late\n\n" {
		t.Errorf("body %q %v", body, err)
	}
	front.waitServed(t)
}
