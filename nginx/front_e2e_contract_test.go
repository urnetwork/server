package nginx

// End-to-end tests of nginx's request contract (§4.2, §4.4 item 2): what a
// block, or connect's in-process handler, receives for what a public client
// sent. Requests go out raw, so the bytes on the wire are exactly the test's.

import (
	"net/http"
	"strings"
	"testing"
)

// The forwarding headers a client may try to set, which the front never
// passes on: it sets X-UR-Forwarded-For from the accepted peer itself.
func e2eSpoofedHeadLines() []string {
	return []string{
		"X-UR-Forwarded-For: 203.0.113.9:1234",
		"X-Forwarded-For: 203.0.113.10",
		"X-Forwarded-Source-Port: 4444",
		"Early-Data: 1",
		"X-UR-Request-Buffering: off",
		"X_Under_Score: dropped",
		"X-Kept: kept",
	}
}

// Fails the test unless header carries the contract for a request from
// peer: X-UR-Forwarded-For is peer, the client's forwarding headers and
// Early-Data are gone, an underscore name is dropped, and X-Kept passes.
func checkE2eContractHeaders(t *testing.T, what string, header http.Header, peer string) {
	t.Helper()
	if forwardedFors := header.Values("X-UR-Forwarded-For"); len(forwardedFors) != 1 || forwardedFors[0] != peer {
		t.Errorf("%s: X-UR-Forwarded-For %q, want the accepted peer %q", what, forwardedFors, peer)
	}
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Source-Port", "Early-Data", "X-UR-Request-Buffering"} {
		if values := header.Values(name); len(values) != 0 {
			t.Errorf("%s: %s %q reached it, want it removed", what, name, values)
		}
	}
	for name := range header {
		if strings.Contains(name, "_") {
			t.Errorf("%s: the header %s reached it, want names with an underscore dropped", what, name)
		}
	}
	if kept := header.Get("X-Kept"); kept != "kept" {
		t.Errorf("%s: X-Kept %q, want it passed", what, kept)
	}
}

// A forwarded request carries nginx's contract, from either address family:
// X-UR-Forwarded-For is the accepted peer in nginx's text, an IPv6 address
// bracketed, whatever the client sent; X-Forwarded-For,
// X-Forwarded-Source-Port, X-UR-Request-Buffering and Early-Data never reach
// the block; Host is the normalized authority, lowercase without its port; a
// header whose name has an underscore is dropped; other headers pass, and
// the connection to the block is keep-alive HTTP/1.1.
func TestFrontE2eForwardsTheRequestContract(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	addrs := []string{eth1.httpsAddr}
	if eth1.httpsIpv6Addr != "" {
		addrs = append(addrs, eth1.httpsIpv6Addr)
	} else {
		t.Log("no loopback IPv6: the bracketed peer is not checked")
	}
	upstream := e2e.upstream(t, "web", "g1")
	for i, addr := range addrs {
		rawConn := e2e.dialRaw(t, addr, "web.service.example")
		headLines := append([]string{
			"GET /contract?q=1 HTTP/1.1",
			"Host: WEB.Service.Example:443",
		}, e2eSpoofedHeadLines()...)
		response, body := rawConn.exchange(t, headLines...)
		if response.StatusCode != http.StatusOK || string(body) != "web/g1" {
			t.Fatalf("%s: %d %q, want the block's answer", addr, response.StatusCode, body)
		}
		request := upstream.awaitRequests(t, i+1)[i]
		if request.host != "web.service.example" {
			t.Errorf("%s: the block got Host %q, want the normalized authority", addr, request.host)
		}
		if request.requestUri != "/contract?q=1" || request.proto != "HTTP/1.1" {
			t.Errorf("%s: the block got %q over %s", addr, request.requestUri, request.proto)
		}
		if connection := request.header.Get("Connection"); connection != "keep-alive" {
			t.Errorf("%s: the block got Connection %q, want keep-alive", addr, connection)
		}
		peer := rawConn.conn.LocalAddr().String()
		checkE2eContractHeaders(t, addr, request.header, peer)
	}
}

// Connect's names are served in-process with the same contract: connect's
// handler sees X-UR-Forwarded-For from the accepted peer, none of the
// client's forwarding headers or Early-Data, no underscore names, and the
// normalized authority, over TLS.
func TestFrontE2eServesConnectInProcessWithTheContract(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	rawConn := e2e.dialRaw(t, eth1.httpsAddr, "connect.service.example")
	headLines := append([]string{
		"GET /?client=1 HTTP/1.1",
		"Host: Connect.Service.Example.",
	}, e2eSpoofedHeadLines()...)
	response, body := rawConn.exchange(t, headLines...)
	if response.StatusCode != http.StatusOK || string(body) != "connect" {
		t.Fatalf("%d %q, want connect's answer", response.StatusCode, body)
	}
	connectRequests := e2e.connectHandler.recorded()
	if len(connectRequests) != 1 {
		t.Fatalf("connect got %d requests, want 1", len(connectRequests))
	}
	connectRequest := connectRequests[0]
	if connectRequest.host != "connect.service.example" || !connectRequest.overTls {
		t.Errorf("connect got Host %q, over TLS %t", connectRequest.host, connectRequest.overTls)
	}
	if connectRequest.requestUri != "/?client=1" || connectRequest.path != "/" {
		t.Errorf("connect got %q, path %q", connectRequest.requestUri, connectRequest.path)
	}
	peer := rawConn.conn.LocalAddr().String()
	if connectRequest.remoteAddr != peer {
		t.Errorf("connect got the peer %q, want %q", connectRequest.remoteAddr, peer)
	}
	checkE2eContractHeaders(t, "connect", connectRequest.header, peer)
	if requests := e2e.requestsSince(map[string]int{}); len(requests) != 0 {
		t.Errorf("blocks got %d requests for a connect name, want none", len(requests))
	}
}

// The block gets the uri nginx sends for its `location / { proxy_pass
// http://upstream/; }`: the normalized path, slashes merged, dot segments
// resolved and escapes decoded, then re-escaped as nginx escapes it, with
// the query as the client sent it. Connect's in-process handler gets the
// same.
func TestFrontE2eForwardsTheUriNginxWouldSend(t *testing.T) {
	e2e := newE2eFront(t, nil, nil)
	eth1 := e2e.iface(t, "eth1")
	cases := []struct {
		target     string
		requestUri string
	}{
		{target: "//a", requestUri: "/a"},
		{target: "/a/../b", requestUri: "/b"},
		{target: "/a%2Fb", requestUri: "/a/b"},
		{target: "/./c//d/", requestUri: "/c/d/"},
		{target: "/e%20f?x=%2F&y", requestUri: "/e%20f?x=%2F&y"},
		{target: "/g%3Fh?i", requestUri: "/g%3Fh?i"},
		{target: "/%7Ej/k%2e/../l", requestUri: "/~j/l"},
	}
	upstream := e2e.upstream(t, "web", "g1")
	rawConn := e2e.dialRaw(t, eth1.httpsAddr, "web.service.example")
	for i, c := range cases {
		response, _ := rawConn.exchange(t, "GET "+c.target+" HTTP/1.1", "Host: web.service.example")
		if response.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", c.target, response.StatusCode)
			continue
		}
		request := upstream.awaitRequests(t, i+1)[i]
		if request.requestUri != c.requestUri {
			t.Errorf("%s: the block got %q, want %q", c.target, request.requestUri, c.requestUri)
		}
	}

	connectConn := e2e.dialRaw(t, eth1.httpsAddr, "connect.service.example")
	response, _ := connectConn.exchange(t, "GET //x/../y%2Fz?w HTTP/1.1", "Host: connect.service.example")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("connect: status %d", response.StatusCode)
	}
	connectRequests := e2e.connectHandler.recorded()
	if len(connectRequests) != 1 || connectRequests[0].requestUri != "/y/z?w" || connectRequests[0].path != "/y/z" {
		t.Errorf("connect got %v, want /y/z?w", connectRequests)
	}
}
