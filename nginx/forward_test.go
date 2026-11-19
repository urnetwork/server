package nginx

// Tests of nginx's request contract and the response filtering: what a
// block receives for a client's request, and what the client receives for
// a block's response.

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// Sends raw request bytes to the front and reads the final response,
// skipping informational ones.
func rawFrontRequest(t *testing.T, front *testForwardFront, rawRequest string) (*http.Response, []byte) {
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
	group, _ := newTestGroup(t, testForwardRouteService("contract", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, testForwardOptions)

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
	group, _ := newTestGroup(t, testForwardRouteService("forwarded-for", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
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
	group, _ := newTestGroup(t, testForwardRouteService("routed", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
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
	group, _ := newTestGroup(t, testForwardRouteService("framing", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, testForwardOptions)

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

// A streamed route marks the request X-UR-Request-Buffering: off, and sends
// its body framed as the client framed it: with its length when declared,
// and chunked when its length is unknown, though it came whole with its head,
// where nginx would have buffered it and sent its length.
func TestForwardStreamedRouteHeaders(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testForwardRouteService("streamed-headers", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		return options
	})
	cases := []struct {
		framing     string
		body        string
		wantChunked bool
	}{
		{framing: "Transfer-Encoding: chunked", body: "3\r\nabc\r\n0\r\n\r\n", wantChunked: true},
		{framing: "Content-Length: 3", body: "abc", wantChunked: false},
	}
	for _, c := range cases {
		rawFrontRequest(t, front, "POST /mcp HTTP/1.1\r\nHost: streamed.service.example\r\n"+c.framing+"\r\n\r\n"+c.body)
	}
	recorded := upstream.recorded()
	if len(recorded) != len(cases) {
		t.Fatalf("upstream saw %d requests, want %d", len(recorded), len(cases))
	}
	for i, c := range cases {
		request := recorded[i]
		if got := request.header.Values(urRequestBufferingHeader); !reflect.DeepEqual(got, []string{"off"}) {
			t.Errorf("%s: X-UR-Request-Buffering %v, want off", c.framing, got)
		}
		if string(request.body) != "abc" {
			t.Errorf("%s: body %q, want abc", c.framing, request.body)
		}
		if c.wantChunked {
			if !reflect.DeepEqual(request.transferEncoding, []string{"chunked"}) || request.contentLength != -1 {
				t.Errorf("%s: sent as %v and length %d, want chunked", c.framing, request.transferEncoding, request.contentLength)
			}
		} else if request.contentLength != 3 || len(request.transferEncoding) != 0 {
			t.Errorf("%s: sent as %v and length %d, want length 3", c.framing, request.transferEncoding, request.contentLength)
		}
	}
}

// A streamed route's head goes to its block at once, before any of its body,
// as nginx sends it once its first pass finds nothing to read
// (ngx_http_read_client_request_body returns NGX_AGAIN and runs
// ngx_http_upstream_init): the block has the head of a client that sent only
// its head, with a declared and with a chunked body, and then the body as the
// client sends it.
func TestForwardStreamedHeadReachesTheBlockBeforeTheBody(t *testing.T) {
	for _, c := range []struct {
		framing string
		body    string
	}{
		{framing: "Content-Length: 5", body: "hello"},
		{framing: "Transfer-Encoding: chunked", body: "5\r\nhello\r\n0\r\n\r\n"},
	} {
		raw := newRawUpstream(t)
		group, bodyStore := newTestGroup(t, testForwardRouteService(testServiceName("streamed-head-first"), []testBlock{
			{name: "g1", weight: 1, port: raw.port},
		}), testForwardFrontSettings())
		front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
			options := testForwardOptions(r)
			options.streamed = true
			return options
		})
		clientConn, err := net.Dial("tcp", front.server.Listener.Addr().String())
		if err != nil {
			t.Fatalf("dial front: %v", err)
		}
		defer clientConn.Close()
		io.WriteString(clientConn, "POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\n"+c.framing+"\r\n\r\n")

		// the head reaches the block while the client still holds its body
		upstreamConn, _, request := raw.accept(t)
		// a bound, so a body held back fails the read rather than hangs it
		upstreamConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		io.WriteString(clientConn, c.body)
		readExactly(t, request.Body, "hello")
		readEnd(t, request.Body)
		io.WriteString(upstreamConn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		response, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s: response %v %v, want the block's 200", c.framing, response, err)
		}
		readExactly(t, response.Body, "ok")
		if recovered := front.waitServed(t); recovered != nil {
			t.Errorf("%s: serveHttp ended with %v", c.framing, recovered)
		}
		checkStoreEmpty(t, bodyStore)
	}
}

// A client that sends its body only once it has the response head, as a
// full-duplex client may, gets its block's answer: the request head went up
// at once, the block answers it before the body, and the body then streams
// on to the block while the response is relayed.
func TestForwardStreamedClientWaitingForTheResponseHead(t *testing.T) {
	raw := newRawUpstream(t)
	group, bodyStore := newTestGroup(t, testForwardRouteService(testServiceName("streamed-full-duplex"), []testBlock{
		{name: "g1", weight: 1, port: raw.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		return options
	})
	clientConn, err := net.Dial("tcp", front.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer clientConn.Close()
	io.WriteString(clientConn, "POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\nContent-Length: 5\r\n\r\n")

	upstreamConn, _, request := raw.accept(t)
	upstreamConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	// the block answers the head alone, and keeps its response open
	io.WriteString(upstreamConn, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nfirst\r\n")
	clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("a client waiting for the response head got %v %v, want the block's 200", response, err)
	}
	readExactly(t, response.Body, "first")
	// the body only now
	io.WriteString(clientConn, "hello")
	readExactly(t, request.Body, "hello")
	readEnd(t, request.Body)
	io.WriteString(upstreamConn, "4\r\nlast\r\n0\r\n\r\n")
	readExactly(t, response.Body, "last")
	readEnd(t, response.Body)
	if recovered := front.waitServed(t); recovered != nil {
		t.Errorf("serveHttp ended with %v", recovered)
	}
	checkStoreEmpty(t, bodyStore)
}

// A streamed route's body that its first read returns whole is kept, as
// nginx buffers a body its first pass reads whole: a block's 503 is retried
// on another block, which gets the whole body with its length and
// X-UR-Request-Buffering: off, as the first one did. Go's server returns a
// body it holds whole with the head in one read, whatever the route's
// client_body_buffer_size, here one byte, as nginx buffers a body it pre-read
// whole whatever that size. Nothing is kept, so the block that began taking
// the body gives the final answer, its 503 (D7), of a chunked body, though it
// came whole with its head, where nginx would retry, and of a declared body
// longer than one read, here past a ForwardSettings.CopyBufferBytes of 16.
func TestForwardStreamedBodyReadWholeFirstIsRetried(t *testing.T) {
	chunked := func(data []byte) string {
		return strconv.FormatInt(int64(len(data)), 16) + "\r\n" + string(data) + "\r\n0\r\n\r\n"
	}
	for _, c := range []struct {
		name        string
		framing     string
		data        []byte
		wantRetried bool
	}{
		{name: "declared", framing: "Content-Length: 16", data: testBodyBytes(16), wantRetried: true},
		{name: "chunked", framing: "Transfer-Encoding: chunked", data: testBodyBytes(16), wantRetried: false},
		{name: "declared past one read", framing: "Content-Length: 17", data: testBodyBytes(17), wantRetried: false},
	} {
		body := string(c.data)
		if strings.HasPrefix(c.framing, "Transfer-Encoding") {
			body = chunked(c.data)
		}
		settings := testForwardFrontSettings()
		settings.Forward.CopyBufferBytes = 16
		serviceName := testServiceName("first-read-retry")
		busy := newRecordingUpstream(t, "g1", func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		live := newRecordingUpstream(t, "g2", nil)
		group, bodyStore := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
			{name: "g1", weight: 1, port: busy.port},
			{name: "g2", weight: 1, port: live.port},
		}), settings)
		front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
			options := testForwardOptions(r)
			options.streamed = true
			options.clientBodyBufferSize = 1
			return options
		})
		response, responseBody := rawFrontRequest(t, front, "POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\n"+c.framing+"\r\n\r\n"+body)
		front.waitServed(t)
		if busyRecorded := busy.recorded(); len(busyRecorded) != 1 || !bytes.Equal(busyRecorded[0].body, c.data) {
			t.Fatalf("%s: the first block did not get the body whole", c.name)
		}
		if !c.wantRetried {
			if response.StatusCode != http.StatusServiceUnavailable || len(live.recorded()) != 0 || retryCount(serviceName, "http_503") != 0 {
				t.Errorf("%s: %d %q, retries %v, want the first block's 503 passed through", c.name, response.StatusCode, responseBody, retryCount(serviceName, "http_503"))
			}
			checkStoreEmpty(t, bodyStore)
			continue
		}
		if response.StatusCode != http.StatusOK || string(responseBody) != "g2" || retryCount(serviceName, "http_503") != 1 {
			t.Fatalf("%s: %d %q, retries %v, want g2's answer after one retry", c.name, response.StatusCode, responseBody, retryCount(serviceName, "http_503"))
		}
		for _, recorded := range []*recordedRequest{busy.recorded()[0], live.recorded()[0]} {
			if !bytes.Equal(recorded.body, c.data) || recorded.contentLength != int64(len(c.data)) || len(recorded.transferEncoding) != 0 {
				t.Errorf("%s: %s got %d bytes, length %d, %v, want the whole body with its length", c.name, recorded.block, len(recorded.body), recorded.contentLength, recorded.transferEncoding)
			}
			if got := recorded.header.Get(urRequestBufferingHeader); got != "off" {
				t.Errorf("%s: %s got X-UR-Request-Buffering %q, want off", c.name, recorded.block, got)
			}
		}
		checkStoreEmpty(t, bodyStore)
	}
}

// A body that comes only partly with its head streams, as nginx streams a
// body its first pass reads only part of: the first read returns the part,
// so nothing is kept, and the block, which has the whole body by the time it
// answers, gives the final answer, its 503 (D7).
func TestForwardStreamedBodyStillArrivingIsNotRetried(t *testing.T) {
	serviceName := testServiceName("first-read-part")
	raw := newRawUpstream(t)
	live := newRecordingUpstream(t, "g2", nil)
	group, bodyStore := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
		{name: "g2", weight: 1, port: live.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		return options
	})
	clientConn, err := net.Dial("tcp", front.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer clientConn.Close()
	io.WriteString(clientConn, "POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\nContent-Length: 20\r\n\r\nfirst-ten!")
	upstreamConn, _, request := raw.accept(t)
	upstreamConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	readExactly(t, request.Body, "first-ten!")
	io.WriteString(clientConn, "second-ten")
	readExactly(t, request.Body, "second-ten")
	io.WriteString(upstreamConn, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 4\r\n\r\nbusy")
	clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response %v %v, want the block's 503", response, err)
	}
	readExactly(t, response.Body, "busy")
	front.waitServed(t)
	if len(live.recorded()) != 0 || retryCount(serviceName, "http_503") != 0 {
		t.Errorf("a body that streamed was retried after its 503")
	}
	checkStoreEmpty(t, bodyStore)
}

// The first read keeps nothing of an h2 body: nginx runs an h2 request when
// its head arrives, before any of its body, so an h2 body streams, and a
// block that took it gives the final answer, even a 503, though the body is
// small and sent whole.
func TestForwardStreamedBodyOnH2Streams(t *testing.T) {
	serviceName := testServiceName("first-read-h2")
	busy := newRecordingUpstream(t, "g1", func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	live := newRecordingUpstream(t, "g2", nil)
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: busy.port},
		{name: "g2", weight: 1, port: live.port},
	}), testForwardFrontSettings())
	front := newTestForwardH2Front(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		return options
	})
	request, _ := http.NewRequest(http.MethodPost, front.url("/mcp"), strings.NewReader("small"))
	response, err := front.server.Client().Do(request)
	if err != nil {
		t.Fatalf("h2 request: %v", err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	front.waitServed(t)
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusServiceUnavailable || len(live.recorded()) != 0 {
		t.Errorf("HTTP/%d %d, %d requests at g2, want the first block's 503 over h2, not retried", response.ProtoMajor, response.StatusCode, len(live.recorded()))
	}
	if recorded := busy.recorded(); len(recorded) != 1 || string(recorded[0].body) != "small" {
		t.Errorf("the first block did not get the body")
	}
}

// A declared empty body on a streamed route has nothing to stream, so its
// block's 503 is retried, and each block gets Content-Length: 0, as nginx
// buffers it: on h1, and on h2 for a POST whose HEADERS frame ends its
// stream, as Go's client and browsers send an empty POST
// (ngx_http_v2_read_request_body finds stream->in_closed).
func TestForwardStreamedEmptyBodyIsRetried(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		serviceName := testServiceName("streamed-empty")
		busy := newRecordingUpstream(t, "g1", func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		live := newRecordingUpstream(t, "g2", nil)
		group, bodyStore := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
			{name: "g1", weight: 1, port: busy.port},
			{name: "g2", weight: 1, port: live.port},
		}), testForwardFrontSettings())
		options := func(r *http.Request) forwardOptions {
			options := testForwardOptions(r)
			options.streamed = true
			return options
		}
		var response *http.Response
		var responseBody []byte
		if h2 {
			front := newTestForwardH2Front(t, group, options)
			request, _ := http.NewRequest(http.MethodPost, front.url("/mcp"), http.NoBody)
			var err error
			response, err = front.server.Client().Do(request)
			if err != nil {
				t.Fatalf("h2 request: %v", err)
			}
			responseBody, _ = io.ReadAll(response.Body)
			response.Body.Close()
			front.waitServed(t)
			if response.ProtoMajor != 2 {
				t.Fatalf("the request went over HTTP/%d, want h2", response.ProtoMajor)
			}
		} else {
			front := newTestForwardFront(t, group, options)
			response, responseBody = rawFrontRequest(t, front, "POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\nContent-Length: 0\r\n\r\n")
			front.waitServed(t)
		}
		if response.StatusCode != http.StatusOK || string(responseBody) != "g2" || retryCount(serviceName, "http_503") != 1 {
			t.Fatalf("h2 %v: %d %q, retries %v, want g2's answer after one retry", h2, response.StatusCode, responseBody, retryCount(serviceName, "http_503"))
		}
		for _, recorded := range []*recordedRequest{busy.recorded()[0], live.recorded()[0]} {
			if got := recorded.header.Values(contentLengthHeader); !reflect.DeepEqual(got, []string{"0"}) || len(recorded.transferEncoding) != 0 || len(recorded.body) != 0 {
				t.Errorf("h2 %v: %s got Content-Length %v, %v and %d bytes, want an empty body of length 0", h2, recorded.block, got, recorded.transferEncoding, len(recorded.body))
			}
		}
		checkStoreEmpty(t, bodyStore)
	}
}

// A body that waits for 100 Continue streams, as nginx streams it: nginx
// sends its 100 Continue and reads at once, before the client can have sent
// any of the body, so the first read keeps nothing, and its block's 503 is
// the final answer.
func TestForwardStreamedBodyExpectingContinueStreams(t *testing.T) {
	serviceName := testServiceName("first-read-continue")
	busy := newRecordingUpstream(t, "g1", func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	live := newRecordingUpstream(t, "g2", nil)
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: busy.port},
		{name: "g2", weight: 1, port: live.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		return options
	})
	clientConn, err := net.Dial("tcp", front.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer clientConn.Close()
	// a bound, so a response held back fails the read rather than hangs it
	clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	clientReader := bufio.NewReader(clientConn)
	io.WriteString(clientConn, "POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\nExpect: 100-continue\r\nContent-Length: 5\r\n\r\n")
	// the body only once the front asks for it
	if response, err := http.ReadResponse(clientReader, nil); err != nil || response.StatusCode != http.StatusContinue {
		t.Fatalf("response %v %v, want 100 Continue", response, err)
	}
	io.WriteString(clientConn, "small")
	response, err := http.ReadResponse(clientReader, nil)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response %v %v, want the first block's 503", response, err)
	}
	front.waitServed(t)
	if len(live.recorded()) != 0 || retryCount(serviceName, "http_503") != 0 {
		t.Errorf("a streamed body's 503 was retried")
	}
}

// A body the store has no room to keep streams as it would without the
// first read keeping it: it isn't refused, as a buffered body over the
// budget is, and its block's 503 is the final answer.
func TestForwardStreamedBodyOverTheBudgetStreams(t *testing.T) {
	settings := testForwardFrontSettings()
	settings.Body.MemoryBudgetBytes = 1
	serviceName := testServiceName("first-read-budget")
	busy := newRecordingUpstream(t, "g1", func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	live := newRecordingUpstream(t, "g2", nil)
	group, bodyStore := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: busy.port},
		{name: "g2", weight: 1, port: live.port},
	}), settings)
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		return options
	})
	response, _ := rawFrontRequest(t, front, "POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\nContent-Length: 5\r\n\r\nsmall")
	front.waitServed(t)
	if response.StatusCode != http.StatusServiceUnavailable || bodyRefusalCount(serviceName, bodyRefusalMemoryBudget) != 0 {
		t.Errorf("%d, budget refusals %v, want the block's 503", response.StatusCode, bodyRefusalCount(serviceName, bodyRefusalMemoryBudget))
	}
	if recorded := busy.recorded(); len(recorded) != 1 || string(recorded[0].body) != "small" {
		t.Errorf("the block did not get the body")
	}
	if len(live.recorded()) != 0 {
		t.Errorf("a body the store had no room for was retried")
	}
	checkStoreEmpty(t, bodyStore)
}

// A kept body's retry outlives the exchange it leaves: that exchange's end
// doesn't interrupt a body read whole. An interrupt's past read deadline
// would end Go's server's background read of the client's connection, which
// it starts at the body's end, and with it the request, and the retry. The
// first block answers 503 while the front's write of the body to it holds,
// so that exchange ends before its write.
func TestForwardKeptBodyOutlivesTheExchangeItLeaves(t *testing.T) {
	serviceName := testServiceName("kept-body-retry")
	raw := newRawUpstream(t)
	live := newRecordingUpstream(t, "g2", nil)
	group, bodyStore := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
		{name: "g2", weight: 1, port: live.port},
	}), testForwardFrontSettings())
	rawAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(raw.port))
	heldConns := make(chan *holdMarkedWriteConn, 1)
	wrapGroupDials(group, func(conn net.Conn, address string) net.Conn {
		if address != rawAddress {
			return conn
		}
		holdConn := newHoldMarkedWriteConn(t, conn, []byte("kept-body"))
		heldConns <- holdConn
		return holdConn
	})
	writers := make(chan *pastReadDeadlineWriter, 1)
	front := newTestForwardFrontWrapping(t, group, func(w http.ResponseWriter) http.ResponseWriter {
		writer := newPastReadDeadlineWriter(w)
		writers <- writer
		return writer
	}, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		return options
	})
	clientConn, err := net.Dial("tcp", front.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer clientConn.Close()
	io.WriteString(clientConn, "POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\nContent-Length: 9\r\n\r\nkept-body")
	writer := receiveBounded(t, writers, "the request's writer")
	holdConn := receiveBounded(t, heldConns, "dial to g1")
	// the first read kept the body before its write to g1 began
	waitClosed(t, holdConn.held, "the held body write")
	conn, _, _ := raw.accept(t)
	io.WriteString(conn, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n")

	clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("response %v, want g2's 200", err)
	}
	responseBody, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(responseBody) != "g2" || retryCount(serviceName, "http_503") != 1 {
		t.Errorf("%d %q, retries %v, want g2's answer after one retry", response.StatusCode, responseBody, retryCount(serviceName, "http_503"))
	}
	if recorded := live.recorded(); len(recorded) != 1 || string(recorded[0].body) != "kept-body" || recorded[0].contentLength != 9 {
		t.Errorf("g2 did not get the kept body with its length")
	}
	front.waitServed(t)
	select {
	case <-writer.pastDeadlines:
		t.Errorf("the client's connection got a past read deadline with its body read whole")
	default:
	}
	checkStoreEmpty(t, bodyStore)
}

// A streamed body that stalls past client_body_timeout before its block
// answers gets no response, as nginx terminates the request
// (ngx_http_upstream_read_request_handler): the block's request, which has
// the body's start, is cut, the client's connection closes with nothing
// written, and the refusal is counted.
func TestForwardStreamedBodyTimeoutEndsTheRequest(t *testing.T) {
	serviceName := testServiceName("streamed-timeout")
	raw := newRawUpstream(t)
	group, bodyStore := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		options.clientBodyTimeout = 50 * time.Millisecond
		return options
	})
	clientConn, err := net.Dial("tcp", front.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer clientConn.Close()
	io.WriteString(clientConn, "POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\nContent-Length: 100\r\n\r\nfirst-ten!")

	upstreamConn, _, request := raw.accept(t)
	// a bound, so a request held open fails the read rather than hangs it
	upstreamConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	readExactly(t, request.Body, "first-ten!")
	// the client sends no more
	if rest, err := io.ReadAll(request.Body); err == nil {
		t.Errorf("the block read its request whole, %q more, want it cut", rest)
	}
	clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if answered, err := io.ReadAll(clientConn); len(answered) != 0 || (err != nil && !isClosedError(err)) {
		t.Errorf("the stalled body was answered %q %v, want the connection closed with nothing written", answered, err)
	}
	if recovered := front.waitServed(t); recovered != http.ErrAbortHandler {
		t.Errorf("serveHttp ended with %v, want http.ErrAbortHandler", recovered)
	}
	if bodyRefusalCount(serviceName, bodyRefusalTimeout) != 1 || attemptCount(serviceName, "g1", "client") != 1 {
		t.Errorf("refusals %v, client attempts %v, want the timeout counted once", bodyRefusalCount(serviceName, bodyRefusalTimeout), attemptCount(serviceName, "g1", "client"))
	}
	checkStoreEmpty(t, bodyStore)
}

// A client body that passes its first bytes, then fails its next read when
// the test says, with the error given: a read past its deadline, as Go's
// server reports one, or a broken body. The read that fails signals as it
// waits. Read by one goroutine at a time, as a request body is.
type failingBody struct {
	body      io.ReadCloser
	passBytes int
	err       error
	waiting   chan struct{}
	fail      chan struct{}
	readBytes int
}

// Passes the first bytes through, then waits for the test and fails.
func (self *failingBody) Read(p []byte) (int, error) {
	if self.readBytes < self.passBytes {
		n, err := self.body.Read(p[:min(len(p), self.passBytes-self.readBytes)])
		self.readBytes += n
		return n, err
	}
	self.waiting <- struct{}{}
	<-self.fail
	return 0, self.err
}

// Closes the body underneath.
func (self *failingBody) Close() error {
	return self.body.Close()
}

// A streamed body that fails on the client's side while its block's response
// is relayed ends that response too, as nginx ends the request: a body that
// stalled past client_body_timeout terminates it, and a broken one ends it
// short of its last bytes. The block's connection is cut, the client's
// response stops before its end, and the refusal is counted by its reason.
// The client's body fails when the test lets it, once the client has read
// the head and the first part, so the order is fixed.
func TestForwardStreamedBodyFailureCutsTheRelayedResponse(t *testing.T) {
	for _, c := range []struct {
		name       string
		err        error
		wantReason string
	}{
		{name: "timed out", err: os.ErrDeadlineExceeded, wantReason: bodyRefusalTimeout},
		{name: "broken", err: io.ErrUnexpectedEOF, wantReason: bodyRefusalIncomplete},
	} {
		serviceName := testServiceName("streamed-failure-relayed")
		raw := newRawUpstream(t)
		group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
			{name: "g1", weight: 1, port: raw.port},
		}), testForwardFrontSettings())
		body := &failingBody{
			passBytes: 10,
			err:       c.err,
			waiting:   make(chan struct{}, 1),
			fail:      make(chan struct{}),
		}
		failOnce := sync.OnceFunc(func() { close(body.fail) })
		// a failed test lets the read go
		defer failOnce()
		front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
			options := testForwardOptions(r)
			options.streamed = true
			body.body = r.Body
			r.Body = body
			return options
		})
		clientConn, err := net.Dial("tcp", front.server.Listener.Addr().String())
		if err != nil {
			t.Fatalf("dial front: %v", err)
		}
		defer clientConn.Close()
		io.WriteString(clientConn, "POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\nContent-Length: 100\r\n\r\nfirst-ten!")

		upstreamConn, _, request := raw.accept(t)
		upstreamConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		readExactly(t, request.Body, "first-ten!")
		// the block answers before the body is whole, and keeps its response
		// open
		io.WriteString(upstreamConn, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nfirst\r\n")
		clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		response, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s: response %v %v, want the block's 200", c.name, response, err)
		}
		readExactly(t, response.Body, "first")
		receiveBounded(t, body.waiting, "the client's next body read")
		failOnce()

		// each end sees its connection closed, neither its own read deadline
		if rest, err := io.ReadAll(response.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%s: the response ended with %q more and %v, want it cut short", c.name, rest, err)
		}
		if rest, err := io.ReadAll(request.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%s: the block's request ended with %q more and %v, want it cut short", c.name, rest, err)
		}
		if recovered := front.waitServed(t); recovered != http.ErrAbortHandler {
			t.Errorf("%s: serveHttp ended with %v, want http.ErrAbortHandler", c.name, recovered)
		}
		if bodyRefusalCount(serviceName, c.wantReason) != 1 {
			t.Errorf("%s: the %s refusal was not counted", c.name, c.wantReason)
		}
	}
}

// On h2 a body that stalls past client_body_timeout resets its stream, and
// only its stream, as nginx resets it: a buffered body read before any
// block, and a streamed one its block has the start of. Go's server resets
// with INTERNAL_ERROR, since it takes no code from a handler; nginx's is
// PROTOCOL_ERROR. The connection serves the next stream.
func TestForwardBodyTimeoutResetsTheH2Stream(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		serviceName := testServiceName("h2-body-timeout")
		upstream := newRecordingUpstream(t, "g1", nil)
		group, bodyStore := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
			{name: "g1", weight: 1, port: upstream.port},
		}), testForwardFrontSettings())
		front := newTestForwardH2Front(t, group, func(r *http.Request) forwardOptions {
			options := testForwardOptions(r)
			options.streamed = streamed
			options.clientBodyTimeout = 50 * time.Millisecond
			return options
		})
		clientConn := front.h2ClientConn(t)

		bodyReader, bodyWriter := io.Pipe()
		defer bodyWriter.Close()
		go io.WriteString(bodyWriter, "first-ten!")
		request, _ := http.NewRequestWithContext(testContext(t), http.MethodPost, front.url("/upload"), bodyReader)
		response, err := clientConn.RoundTrip(request)
		var streamError http2.StreamError
		if !errors.As(err, &streamError) || streamError.Code != http2.ErrCodeInternal {
			if response != nil {
				response.Body.Close()
			}
			t.Fatalf("streamed %v: %v %v, want the stream reset with INTERNAL_ERROR", streamed, response, err)
		}
		if recovered := front.waitServed(t); recovered != http.ErrAbortHandler {
			t.Errorf("streamed %v: serveHttp ended with %v, want http.ErrAbortHandler", streamed, recovered)
		}
		if bodyRefusalCount(serviceName, bodyRefusalTimeout) != 1 {
			t.Errorf("streamed %v: the timeout was not counted", streamed)
		}

		next, _ := http.NewRequestWithContext(testContext(t), http.MethodGet, front.url("/next"), nil)
		nextResponse, err := clientConn.RoundTrip(next)
		if err != nil {
			t.Fatalf("streamed %v: the next stream: %v", streamed, err)
		}
		nextBody, _ := io.ReadAll(nextResponse.Body)
		nextResponse.Body.Close()
		front.waitServed(t)
		if nextResponse.StatusCode != http.StatusOK || string(nextBody) != "g1" {
			t.Errorf("streamed %v: the next stream got %d %q, want the block's answer on the same connection", streamed, nextResponse.StatusCode, nextBody)
		}
		for _, recorded := range upstream.recorded() {
			if recorded.method == http.MethodPost && recorded.bodyErr == nil {
				t.Errorf("streamed %v: the block got the stalled body whole", streamed)
			}
		}
		checkStoreEmpty(t, bodyStore)
	}
}

// A streamed body's client_body_timeout bounds only the wait for the
// client, as nginx's read timer runs only while it waits on the client: a
// block slow to take the body, which holds back the front's next read of the
// client, doesn't time out a client that sent what it had. On h2, Go's
// server fails a stream's body once its read deadline passes, a read pending
// or not, so each read's deadline is cleared once the read returns. The
// block takes nothing of the body for three of the client's timeouts.
func TestForwardStreamedBodyTimeoutWaitsOnlyForTheClient(t *testing.T) {
	const clientBodyTimeout = 100 * time.Millisecond
	raw := newRawUpstream(t)
	group, _ := newTestGroup(t, testForwardRouteService(testServiceName("streamed-timeout-client-only"), []testBlock{
		{name: "g1", weight: 1, port: raw.port},
	}), testForwardFrontSettings())
	heldConns := make(chan *holdMarkedWriteConn, 1)
	wrapGroupDials(group, func(conn net.Conn, address string) net.Conn {
		holdConn := newHoldMarkedWriteConn(t, conn, []byte("first"))
		heldConns <- holdConn
		return holdConn
	})
	front := newTestForwardH2Front(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		options.clientBodyTimeout = clientBodyTimeout
		return options
	})
	clientConn := front.h2ClientConn(t)
	bodyReader, bodyWriter := io.Pipe()
	defer bodyWriter.Close()
	go io.WriteString(bodyWriter, "first")
	request, _ := http.NewRequestWithContext(testContext(t), http.MethodPost, front.url("/upload"), bodyReader)
	responses := make(chan string, 1)
	go func() {
		response, err := clientConn.RoundTrip(request)
		if err != nil {
			responses <- err.Error()
			return
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		responses <- strconv.Itoa(response.StatusCode) + " " + string(body)
	}()
	holdConn := receiveBounded(t, heldConns, "dial")
	upstreamConn, _, upstreamRequest := raw.accept(t)
	upstreamConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	waitClosed(t, holdConn.held, "the held write of the body's start")
	// the time under test: the block takes nothing past the timeout
	time.Sleep(3 * clientBodyTimeout)
	// the client sends the rest while the write still holds
	go func() {
		io.WriteString(bodyWriter, "second")
		bodyWriter.Close()
	}()
	holdConn.release()

	readExactly(t, upstreamRequest.Body, "first")
	readExactly(t, upstreamRequest.Body, "second")
	if rest, err := io.ReadAll(upstreamRequest.Body); err != nil || len(rest) != 0 {
		t.Fatalf("the block read %q %v past the body, want its end", rest, err)
	}
	io.WriteString(upstreamConn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	if response := receiveBounded(t, responses, "response"); response != "200 ok" {
		t.Errorf("response %q, want the block's 200", response)
	}
	if recovered := front.waitServed(t); recovered != nil {
		t.Errorf("serveHttp ended with %v", recovered)
	}
}

// A websocket service's requests carry Connection: upgrade, and the client's
// Upgrade only when it sent one, as nginx's websocket locations do.
func TestForwardWebsocketServiceHeaders(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testForwardRouteService("websocket-headers", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
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
	group, _ := newTestGroup(t, testForwardRouteService("hidden", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, testForwardOptions)
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
	settings := testForwardFrontSettings()
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
		serviceName := testServiceName("head-limit-" + strings.Repeat("x", c.headBytes-maxHeadBytes))
		group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
			{name: "g1", weight: 1, port: raw.port},
			{name: "g2", weight: 1, port: other.port},
		}), settings)
		// the first pick of two idle blocks of equal weight is the first
		front := newTestForwardFront(t, group, testForwardOptions)
		responses := make(chan *http.Response, 1)
		go func() {
			request, _ := http.NewRequest(http.MethodGet, front.url("/"), nil)
			response, _, _ := testDo(t, request)
			responses <- response
		}()
		conn, _, _ := raw.accept(t)
		io.WriteString(conn, headOfSize(c.headBytes)+"ok")
		response := receiveBounded(t, responses, "response")
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
	// held handlers end on any exit, so the servers' cleanup never waits
	releaseHeld := sync.OnceFunc(func() { close(release) })
	defer releaseHeld()
	upstream := newRecordingUpstream(t, "g1", func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: late\n\n")
	})
	group, _ := newTestGroup(t, testForwardRouteService("head-first", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, testForwardOptions)
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
	releaseHeld()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "data: late\n\n" {
		t.Errorf("body %q %v", body, err)
	}
	front.waitServed(t)
}

// The read timeout is armed only once the request is sent whole, as nginx
// arms proxy_read_timeout: an upload the block takes in longer than the
// timeout still gets its answer. The hold is the subject here, so the test
// waits out several read timeouts while the block stops reading.
func TestForwardReadTimeoutArmsAfterRequestSent(t *testing.T) {
	settings := testForwardFrontSettings()
	settings.Forward.ProxyReadTimeout = 100 * time.Millisecond
	gotPart := make(chan struct{})
	release := make(chan struct{})
	// held handlers end on any exit, so the servers' cleanup never waits
	releaseHeld := sync.OnceFunc(func() { close(release) })
	defer releaseHeld()
	upstream := newRecordingUpstream(t, "g1", nil)
	upstream.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadFull(r.Body, make([]byte, 1024))
		close(gotPart)
		<-release
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "done")
	})
	serviceName := testServiceName("read-timeout-armed")
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), settings)
	front := newTestForwardFront(t, group, testForwardOptions)
	type result struct {
		response *http.Response
		body     []byte
		err      error
	}
	results := make(chan result, 1)
	go func() {
		// the body limit, more than loopback buffers hold, so the request
		// is still being sent while the block holds
		request, _ := http.NewRequest(http.MethodPost, front.url("/upload"), bytes.NewReader(testBodyBytes(16*1024*1024)))
		response, body, err := testDo(t, request)
		results <- result{response: response, body: body, err: err}
	}()
	<-gotPart
	time.Sleep(3 * settings.Forward.ProxyReadTimeout)
	releaseHeld()
	got := receiveBounded(t, results, "result")
	if got.err != nil || got.response.StatusCode != http.StatusOK || string(got.body) != "done" {
		t.Fatalf("slow upload: %v %q %v, want 200", got.response, got.body, got.err)
	}
	if attemptCount(serviceName, "g1", "timeout") != 0 {
		t.Errorf("the read timed out while the request was being sent")
	}
}

// A known length passes as Content-Length, a HEAD's included, but a status
// that can't carry a body gets none.
func TestForwardResponseContentLength(t *testing.T) {
	cases := []struct {
		status        int
		contentLength int64
		want          string
	}{
		{status: http.StatusOK, contentLength: 5, want: "5"},
		{status: http.StatusOK, contentLength: -1, want: ""},
		{status: http.StatusNoContent, contentLength: 0, want: ""},
		{status: http.StatusNotModified, contentLength: 0, want: ""},
		{status: http.StatusSwitchingProtocols, contentLength: 0, want: ""},
	}
	for _, c := range cases {
		header := http.Header{}
		forwardResponseHeaders(header, &http.Response{
			StatusCode:    c.status,
			ContentLength: c.contentLength,
			Header:        http.Header{contentLengthHeader: {"1234"}},
		})
		if got := header.Get(contentLengthHeader); got != c.want {
			t.Errorf("status %d length %d: Content-Length %q, want %q", c.status, c.contentLength, got, c.want)
		}
	}
}

// Through an h2 front: a buffered body and a streamed body of unknown
// length are forwarded as HTTP/1.1, with the Host the stream's authority
// gives, and a response relayed back, so the per-stream deadlines and
// flushes work on h2 as on h1.
func TestForwardHttp2Client(t *testing.T) {
	upstream := newRecordingUpstream(t, "g1", nil)
	group, bodyStore := newTestGroup(t, testForwardRouteService("h2", []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	protos := make(chan string, 4)
	front := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protos <- r.Proto
		options := testForwardOptions(r)
		options.streamed = r.URL.Path == "/streamed"
		group.serveHttp(w, r, options)
	}))
	front.EnableHTTP2 = true
	front.StartTLS()
	defer front.Close()
	client := front.Client()

	request, _ := http.NewRequest(http.MethodPost, front.URL+"/buffered", bytes.NewReader(testBodyBytes(70*1024)))
	request.Host = "H2.Service.Example"
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("buffered request: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "g1" || <-protos != "HTTP/2.0" {
		t.Fatalf("buffered response %d %q", response.StatusCode, body)
	}

	bodyReader, bodyWriter := io.Pipe()
	go func() {
		bodyWriter.Write([]byte("streamed "))
		bodyWriter.Write([]byte("body"))
		bodyWriter.Close()
	}()
	request, _ = http.NewRequest(http.MethodPost, front.URL+"/streamed", bodyReader)
	request.Host = "h2.service.example"
	response, err = client.Do(request)
	if err != nil {
		t.Fatalf("streamed request: %v", err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || <-protos != "HTTP/2.0" {
		t.Fatalf("streamed response %d", response.StatusCode)
	}

	recorded := upstream.recorded()
	if len(recorded) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(recorded))
	}
	if recorded[0].proto != "HTTP/1.1" || recorded[0].host != "h2.service.example" || !bytes.Equal(recorded[0].body, testBodyBytes(70*1024)) || recorded[0].contentLength != 70*1024 {
		t.Errorf("buffered upstream request %s %s %d bytes", recorded[0].proto, recorded[0].host, len(recorded[0].body))
	}
	if !reflect.DeepEqual(recorded[1].transferEncoding, []string{"chunked"}) || string(recorded[1].body) != "streamed body" {
		t.Errorf("streamed upstream request %v %q", recorded[1].transferEncoding, recorded[1].body)
	}
	checkStoreEmpty(t, bodyStore)
}

// A streamed body goes on to the block as it arrives, as nginx's
// proxy_request_buffering off sends it: the block has the first part of a
// declared and of a chunked body while the client still holds the rest. The
// first pass read that part, and found the body not yet whole, so it streams
// with no wait for the rest, framed as the client framed it.
func TestForwardStreamedBodyPassesThrough(t *testing.T) {
	cases := []struct {
		framing     string
		firstPart   string
		rest        string
		wantChunked bool
	}{
		{framing: "Content-Length: 20", firstPart: "first-ten!", rest: "second-ten", wantChunked: false},
		{framing: "Transfer-Encoding: chunked", firstPart: "a\r\nfirst-ten!\r\n", rest: "a\r\nsecond-ten\r\n0\r\n\r\n", wantChunked: true},
	}
	for _, c := range cases {
		raw := newRawUpstream(t)
		group, _ := newTestGroup(t, testForwardRouteService(testServiceName("streamed-pass-through"), []testBlock{
			{name: "g1", weight: 1, port: raw.port},
		}), testForwardFrontSettings())
		front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
			options := testForwardOptions(r)
			options.streamed = true
			return options
		})
		clientConn, err := net.Dial("tcp", front.server.Listener.Addr().String())
		if err != nil {
			t.Fatalf("dial front: %v", err)
		}
		defer clientConn.Close()
		io.WriteString(clientConn, "POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\n"+c.framing+"\r\n\r\n"+c.firstPart)

		upstreamConn, _, request := raw.accept(t)
		// a bound, so a body held back fails the read rather than hangs it
		upstreamConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		if chunked := reflect.DeepEqual(request.TransferEncoding, []string{"chunked"}); chunked != c.wantChunked || (!chunked && request.ContentLength != 20) {
			t.Errorf("%s: the block got %v and length %d", c.framing, request.TransferEncoding, request.ContentLength)
		}
		readExactly(t, request.Body, "first-ten!")
		io.WriteString(clientConn, c.rest)
		readExactly(t, request.Body, "second-ten")
		if rest, err := io.ReadAll(request.Body); err != nil || len(rest) != 0 {
			t.Fatalf("%s: the block read %q %v past the body", c.framing, rest, err)
		}
		io.WriteString(upstreamConn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		response, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s: response %v %v, want 200", c.framing, response, err)
		}
		front.waitServed(t)
	}
}

// A client that stops reading a relayed response is cut once
// HttpSettings.SendTimeout passes without progress, nginx's send_timeout:
// the handler aborts and the upstream connection is closed, rather than both
// waiting on the client.
func TestForwardSendTimeoutCutsStalledClient(t *testing.T) {
	settings := testForwardFrontSettings()
	settings.Http.SendTimeout = 200 * time.Millisecond
	raw := newRawUpstream(t)
	group, _ := newTestGroup(t, testForwardRouteService(testServiceName("send-timeout-client"), []testBlock{
		{name: "g1", weight: 1, port: raw.port},
	}), settings)
	front := newTestForwardFront(t, group, testForwardOptions)
	clientConn, err := net.Dial("tcp", front.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer clientConn.Close()
	io.WriteString(clientConn, "GET /download HTTP/1.1\r\nHost: download.service.example\r\n\r\n")

	upstreamConn, _, _ := raw.accept(t)
	// far more than loopback buffers hold, sent as fast as the front takes it
	chunk := make([]byte, 64*1024)
	chunkCount := 1024
	upstreamErrs := make(chan error, 1)
	go func() {
		_, err := io.WriteString(upstreamConn, "HTTP/1.1 200 OK\r\nContent-Length: "+strconv.Itoa(chunkCount*len(chunk))+"\r\n\r\n")
		for i := 0; err == nil && i < chunkCount; i += 1 {
			_, err = upstreamConn.Write(chunk)
		}
		upstreamErrs <- err
	}()
	// the client takes the head, then nothing more
	response, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response %v %v, want a 200 head", response, err)
	}
	if recovered := front.waitServed(t); recovered != http.ErrAbortHandler {
		t.Errorf("serveHttp ended with %v, want http.ErrAbortHandler", recovered)
	}
	if err := receiveBounded(t, upstreamErrs, "end of the block's writes"); err == nil {
		t.Errorf("the block's whole response was taken for a client that read none of it")
	}
}

// A body refused before it is read, here a local 503 over the memory budget,
// is answered within HttpSettings.LingeringTimeout though the client stalls
// mid-body: Go's server reads what remains of a body before it writes the
// answer, and lingering_timeout bounds that read.
func TestForwardRefusalAnswersWithinLingeringTimeout(t *testing.T) {
	settings := testForwardFrontSettings()
	settings.Body.MemoryBudgetBytes = 1
	settings.Http.LingeringTimeout = 200 * time.Millisecond
	serviceName := testServiceName("refusal-lingering")
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), settings)
	front := newTestForwardFront(t, group, testForwardOptions)
	clientConn, err := net.Dial("tcp", front.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer clientConn.Close()
	// one chunk of a body of unknown length, then nothing more
	io.WriteString(clientConn, "POST /upload HTTP/1.1\r\nHost: upload.service.example\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n")
	// a bound, so an answer held back fails the read rather than hangs it
	clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response %v %v, want the local 503", response, err)
	}
	front.waitServed(t)
	if bodyRefusalCount(serviceName, bodyRefusalMemoryBudget) != 1 || len(upstream.recorded()) != 0 {
		t.Errorf("the refusal was not counted, or reached the block")
	}
}
