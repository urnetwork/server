package nginx

// The contract between the front's routing and a service's upstream side
// (owner: s-forward), and nginx's request contract (§4.2), which the
// forwarder applies to every forwarded request
// (warp/warpctl/config.go:2504-2539, ngx_http_proxy_module.c):
//   - the request line is HTTP/1.1, with the path routing gives or the
//     request's own escaped path and raw query;
//   - Host is the normalized authority with no port, nginx's $host;
//   - Connection is keep-alive, or upgrade on a websocket service, whose
//     Upgrade is the client's;
//   - X-UR-Forwarded-For is the accepted peer, `addr:port` with an ipv6
//     address bracketed, in nginx's text ($warp_client_addr:$remote_port);
//   - X-UR-Request-Buffering is off on a streamed route;
//   - Content-Length and Transfer-Encoding are regenerated for the body as
//     sent;
//   - every other client header passes, except the ones nginx sets or skips
//     (Host, Connection, Proxy-Connection, Keep-Alive, TE, Expect, Upgrade,
//     Content-Length, Transfer-Encoding, X-UR-Forwarded-For,
//     X-Forwarded-For, X-Forwarded-Source-Port, X-UR-Request-Buffering,
//     Early-Data), Trailer, whose trailers are never sent, and any whose
//     name nginx's parser drops (a character outside letters, digits and
//     '-', an underscore included). So Early-Data never reaches a block,
//     as nginx's empty $ssl_early_data never does.
//
// The response passes with the headers nginx hides removed (Date, Server,
// X-Pad and its five X-Accel-* headers) and the framing headers
// regenerated; a response head over ForwardSettings.MaxUpstreamResponseHeaderBytes
// is a 502.
//
// nginx forwards a header the client names in its Connection header, and
// Proxy-Authorization, so the front does too.

import (
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Header names of the request contract. Protocol values.
const (
	hostHeader                = "Host"
	connectionHeader          = "Connection"
	upgradeHeader             = "Upgrade"
	contentLengthHeader       = "Content-Length"
	transferEncodingHeader    = "Transfer-Encoding"
	urForwardedForHeader      = "X-UR-Forwarded-For"
	urRequestBufferingHeader  = "X-UR-Request-Buffering"
	forwardedForHeader        = "X-Forwarded-For"
	forwardedSourcePortHeader = "X-Forwarded-Source-Port"
	earlyDataHeader           = "Early-Data"
)

// One request's forwarding options, set by routing.
type forwardOptions struct {
	// the interface the request arrived on
	interfaceName string
	// the accepted peer. X-UR-Forwarded-For is set from it, never from the
	// client's own headers, and a stateful service hashes its address.
	// Invalid leaves X-UR-Forwarded-For out, as the ingress helper does for a
	// peer it can't read, and balances a stateful service by round robin, as
	// nginx does for an empty key.
	clientAddrPort netip.AddrPort
	// the normalized authority the request was routed by, nginx's $host
	// (normalizeHost): the upstream's Host. Empty derives it from the
	// request's Host: lowercased, without its port and one trailing dot.
	host string
	// set for a per-block lb route (`/by/b/<service>/<block>/`): that block
	// only, which reports its own result and is never retried elsewhere,
	// whatever its weight. Empty means any eligible block of the service.
	blockName string
	// the path and query to send upstream, exactly: routing sets it to
	// nginx's normalized URI, with a matched prefix replaced, re-escaped, and
	// `?args`. Empty sends the request's escaped path and raw query.
	upstreamPath string
	// the body streams through unbuffered, with X-UR-Request-Buffering: off,
	// and is no longer eligible for a retry once forwarding begins, unless
	// its first read returned it whole, as nginx's first pass would have,
	// and it was kept (body_stream.go)
	streamed bool
	// the service relays websocket upgrades, and every request to it carries
	// Connection: upgrade and the stream timeouts, as nginx's websocket
	// locations do
	websocket bool
	// the longest wait between two reads of the body: a route's
	// client_body_timeout_seconds. Zero means BodySettings.ClientBodyTimeout.
	clientBodyTimeout time.Duration
	// the bytes of the body held in memory before the rest spills: a route's
	// client_body_buffer_size. Zero means BodySettings.MemoryPrefixBytes.
	clientBodyBufferSize int64
	// the upstream read timeout. Zero means ForwardSettings.StreamTimeout on
	// websocket and streamed routes and ForwardSettings.ProxyReadTimeout
	// elsewhere, as routing would set it. The send timeout follows the same
	// rule and isn't an option.
	readTimeout time.Duration
}

// A service's upstream side as routing sees it. Safe for concurrent use.
type serviceForwarder interface {
	// forwards one request and writes the upstream's response, or the front's
	// own error response (400, 413, 502, 503, 504, and 408 on h3), to w. It
	// returns when the response is complete, or for a websocket when the
	// tunnel has closed, so a caller holding a concurrency slot for the
	// request (D8) holds it for the tunnel's life; the tunnel's client side is
	// the connection w hijacks, which the caller tracks from the hijack. A
	// response the upstream cuts short after its head reached the client is
	// aborted by panicking with http.ErrAbortHandler, as
	// httputil.ReverseProxy does, so the client sees a truncated response,
	// never a complete one; so is a request whose body stalls past
	// client_body_timeout on h1 or h2, or whose client went away, which get
	// no response, as nginx gives none, and one whose streamed body fails on
	// the client's side while the response is relayed. Callers' deferred
	// cleanup runs and they must not recover it.
	serveHttp(w http.ResponseWriter, r *http.Request, options forwardOptions)
	// releases the pools and cuts the tunnels; requests in flight end with
	// their connections. A dial in flight ends with it, and it returns once
	// every such dial has, so no upstream connection outlives it.
	close()
}

// The client request headers nginx never passes: the ones its proxy module
// sets or skips by default (ngx_http_proxy_headers), the ones the lb's
// proxy_set_header lines name, and Trailer, since request trailers are
// never forwarded. Keys are canonical.
var forwardSkippedRequestHeaders = map[string]bool{
	http.CanonicalHeaderKey(hostHeader):                true,
	http.CanonicalHeaderKey(connectionHeader):          true,
	http.CanonicalHeaderKey("Proxy-Connection"):        true,
	http.CanonicalHeaderKey(contentLengthHeader):       true,
	http.CanonicalHeaderKey(transferEncodingHeader):    true,
	http.CanonicalHeaderKey("TE"):                      true,
	http.CanonicalHeaderKey("Keep-Alive"):              true,
	http.CanonicalHeaderKey("Expect"):                  true,
	http.CanonicalHeaderKey(upgradeHeader):             true,
	http.CanonicalHeaderKey("Trailer"):                 true,
	http.CanonicalHeaderKey(urForwardedForHeader):      true,
	http.CanonicalHeaderKey(forwardedForHeader):        true,
	http.CanonicalHeaderKey(forwardedSourcePortHeader): true,
	http.CanonicalHeaderKey(earlyDataHeader):           true,
	http.CanonicalHeaderKey(urRequestBufferingHeader):  true,
}

// The upstream response headers that never reach the client: the ones
// nginx's proxy_hide_header defaults hide (ngx_http_proxy_hide_headers),
// whose Date the front's server replaces, and the framing headers nginx
// regenerates or drops (ngx_http_upstream_headers_in). Upgrade passes only
// on a 101. Keys are canonical.
var forwardHiddenResponseHeaders = map[string]bool{
	http.CanonicalHeaderKey("Date"):                 true,
	http.CanonicalHeaderKey("Server"):               true,
	http.CanonicalHeaderKey("X-Pad"):                true,
	http.CanonicalHeaderKey("X-Accel-Expires"):      true,
	http.CanonicalHeaderKey("X-Accel-Redirect"):     true,
	http.CanonicalHeaderKey("X-Accel-Limit-Rate"):   true,
	http.CanonicalHeaderKey("X-Accel-Buffering"):    true,
	http.CanonicalHeaderKey("X-Accel-Charset"):      true,
	http.CanonicalHeaderKey(connectionHeader):       true,
	http.CanonicalHeaderKey("Keep-Alive"):           true,
	http.CanonicalHeaderKey("Proxy-Connection"):     true,
	http.CanonicalHeaderKey(transferEncodingHeader): true,
	http.CanonicalHeaderKey(contentLengthHeader):    true,
	http.CanonicalHeaderKey("Trailer"):              true,
	http.CanonicalHeaderKey(upgradeHeader):          true,
}

// Whether nginx's request parser keeps a header of this name: only letters,
// digits and '-' (ngx_http_parse_header_line's lowcase table, and
// ngx_http_v2_validate_header with underscores_in_headers off). Any other
// name, one with an underscore included, is an invalid header nginx drops
// with ignore_invalid_headers on.
func forwardHeaderNameValid(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		c := name[i]
		switch {
		case 'a' <= c && c <= 'z':
		case 'A' <= c && c <= 'Z':
		case '0' <= c && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
}

// Whether a header value can be written on an HTTP/1.1 header line: no CR,
// LF or NUL. The front's servers refuse such values already; this keeps a
// value that slipped through from splitting the upstream request.
func forwardHeaderValueValid(value string) bool {
	return !strings.ContainsAny(value, "\r\n\x00")
}

// nginx's text of a client address, its $remote_addr (ngx_sock_ntop with no
// port): dotted decimal for ipv4, ngx_inet6_ntop for ipv6. An ipv4-mapped
// address is written as ipv4, as the front's sockets present one and as the
// ingress helper writes it; a zone is dropped, as nginx never prints one. An
// invalid address is empty.
func nginxRemoteAddrText(addr netip.Addr) string {
	if !addr.IsValid() {
		return ""
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return addr.String()
	}
	return nginxInet6Ntop(addr.As16())
}

// A port of ngx_inet6_ntop (src/core/ngx_inet.c): RFC 5952's form, the
// first longest run of two or more zero groups compressed and lowercase hex
// without leading zeros, except that an address whose first six groups are
// zero, or whose first five are zero and sixth is ffff, ends in a dotted
// ipv4 quad, as does one whose first seven groups are zero unless its last
// group is below 0x100 or ends in byte 1. So ::c000:201 is ::192.0.2.1 and
// ::102 is ::0.0.1.2, where netip writes them as hex.
func nginxInet6Ntop(p [16]byte) string {
	// the longest run of zero groups, the first on a tie; max starts at 1 so
	// a single zero group is never compressed
	zero := -1
	last := -1
	longest := 1
	run := 0
	for i := 0; i < 16; i += 2 {
		if p[i] != 0 || p[i+1] != 0 {
			if longest < run {
				zero = last
				longest = run
			}
			run = 0
			continue
		}
		if run == 0 {
			last = i
		}
		run += 1
	}
	if longest < run {
		zero = last
		longest = run
	}

	text := make([]byte, 0, len("ffff:ffff:ffff:ffff:ffff:ffff:255.255.255.255"))
	n := 16
	if zero == 0 {
		if (longest == 5 && p[10] == 0xff && p[11] == 0xff) ||
			longest == 6 ||
			(longest == 7 && p[14] != 0 && p[15] != 1) {
			n = 12
		}
		text = append(text, ':')
	}
	for i := 0; i < n; i += 2 {
		if i == zero {
			text = append(text, ':')
			i += (longest - 1) * 2
			continue
		}
		text = strconv.AppendUint(text, uint64(p[i])*256+uint64(p[i+1]), 16)
		if i < 14 {
			text = append(text, ':')
		}
	}
	if n == 12 {
		for j := 12; j < 16; j += 1 {
			text = strconv.AppendUint(text, uint64(p[j]), 10)
			if j < 15 {
				text = append(text, '.')
			}
		}
	}
	return string(text)
}

// X-UR-Forwarded-For for a peer: nginx's `$warp_client_addr:$remote_port`,
// whose map brackets any address text holding a ':'. Empty for an invalid
// peer.
func forwardedForValue(clientAddrPort netip.AddrPort) string {
	addrText := nginxRemoteAddrText(clientAddrPort.Addr())
	if addrText == "" {
		return ""
	}
	port := strconv.Itoa(int(clientAddrPort.Port()))
	if strings.Contains(addrText, ":") {
		return "[" + addrText + "]:" + port
	}
	return addrText + ":" + port
}

// The upstream Host: routing's normalized authority, or the request's Host
// lowercased, without its port and one trailing dot, as nginx's $host is for
// any host routing accepts.
func forwardHost(r *http.Request, options *forwardOptions) string {
	if options.host != "" {
		return options.host
	}
	host := r.Host
	if strings.HasPrefix(host, "[") {
		// an ipv6 literal keeps its brackets
		if end := strings.IndexByte(host, ']'); 0 <= end {
			host = host[:end+1]
		}
	} else if colon := strings.LastIndexByte(host, ':'); 0 <= colon {
		host = host[:colon]
	}
	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
}

// The request target sent upstream: routing's path, or the request's escaped
// path and raw query. A byte that can't sit in a request line is
// percent-encoded, and an empty target is "/", since nginx never sends an
// empty URI.
func forwardTarget(r *http.Request, options *forwardOptions) string {
	target := options.upstreamPath
	if target == "" {
		target = r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
	}
	if target == "" {
		return "/"
	}
	if !strings.ContainsFunc(target, func(c rune) bool { return c <= ' ' || c == 0x7f }) {
		return target
	}
	const upperHex = "0123456789ABCDEF"
	var escapedTarget strings.Builder
	for i := range len(target) {
		c := target[i]
		if c <= ' ' || c == 0x7f {
			escapedTarget.WriteByte('%')
			escapedTarget.WriteByte(upperHex[c>>4])
			escapedTarget.WriteByte(upperHex[c&0xf])
			continue
		}
		escapedTarget.WriteByte(c)
	}
	return escapedTarget.String()
}

// How a request's body is framed upstream.
type forwardBodyFraming struct {
	// a Content-Length is sent
	known bool
	// the length sent when known
	length int64
	// Transfer-Encoding: chunked is sent, for a streamed body of unknown
	// length
	chunked bool
}

// Whether the client sent a body to frame: a declared length, Content-Length
// zero included, or a body of unknown length. A request with neither, as a
// GET, gets no framing headers, as nginx's $proxy_internal_body_length is
// empty for it. On h3 every request without a content-length has a body of
// unknown length, so a buffered route frames none once its stream ends with
// no data (upstreamGroup.serveHttp).
func forwardRequestHasBody(r *http.Request) bool {
	if r.ContentLength != 0 {
		return true
	}
	_, ok := r.Header[contentLengthHeader]
	return ok
}

// Renders the upstream request line and headers, in nginx's order: Host,
// then the set headers, then the client's headers that pass, sorted by name
// since Go keeps no client order. Each client header line passes as its own
// line.
func forwardRequestHead(r *http.Request, options *forwardOptions, upgrade bool, bodyFraming forwardBodyFraming) []byte {
	head := make([]byte, 0, 1024)
	addLine := func(name string, value string) {
		head = append(head, name...)
		head = append(head, ": "...)
		head = append(head, value...)
		head = append(head, "\r\n"...)
	}

	head = append(head, r.Method...)
	head = append(head, ' ')
	head = append(head, forwardTarget(r, options)...)
	head = append(head, " HTTP/1.1\r\n"...)

	addLine(hostHeader, forwardHost(r, options))
	if options.websocket {
		addLine(connectionHeader, "upgrade")
	} else {
		addLine(connectionHeader, "keep-alive")
	}
	if forwardedFor := forwardedForValue(options.clientAddrPort); forwardedFor != "" {
		addLine(urForwardedForHeader, forwardedFor)
	}
	if options.streamed {
		addLine(urRequestBufferingHeader, "off")
	}
	if upgrade {
		addLine(upgradeHeader, r.Header.Get(upgradeHeader))
	}
	if bodyFraming.known {
		addLine(contentLengthHeader, strconv.FormatInt(bodyFraming.length, 10))
	} else if bodyFraming.chunked {
		addLine(transferEncodingHeader, "chunked")
	}

	headerNames := make([]string, 0, len(r.Header))
	for name := range r.Header {
		headerNames = append(headerNames, name)
	}
	slices.Sort(headerNames)
	for _, name := range headerNames {
		if !forwardHeaderNameValid(name) {
			continue
		}
		if forwardSkippedRequestHeaders[http.CanonicalHeaderKey(name)] {
			continue
		}
		for _, value := range r.Header[name] {
			if forwardHeaderValueValid(value) {
				addLine(name, value)
			}
		}
	}
	head = append(head, "\r\n"...)
	return head
}

// Copies an upstream response's headers into the client's, after any the
// front set already, leaving out the hidden and framing headers. A known
// body length passes as Content-Length, a HEAD's included; a status that
// can't carry a body (1xx, 204, 304) gets none.
func forwardResponseHeaders(clientHeader http.Header, response *http.Response) {
	for name, values := range response.Header {
		if forwardHiddenResponseHeaders[http.CanonicalHeaderKey(name)] {
			continue
		}
		clientHeader[name] = append(clientHeader[name], values...)
	}
	noBodyStatus := response.StatusCode < http.StatusOK ||
		response.StatusCode == http.StatusNoContent ||
		response.StatusCode == http.StatusNotModified
	if 0 <= response.ContentLength && !noBodyStatus {
		clientHeader.Set(contentLengthHeader, strconv.FormatInt(response.ContentLength, 10))
	}
}

// Writes one of the front's own error responses: the status and its text.
// nginx writes an html page; the parity harness accepts the difference
// (LBALT-IMPL.md §4).
func writeForwardError(w http.ResponseWriter, status int) {
	header := w.Header()
	header.Set("Content-Type", "text/plain; charset=utf-8")
	header.Del(contentLengthHeader)
	w.WriteHeader(status)
	w.Write([]byte(strconv.Itoa(status) + " " + http.StatusText(status) + "\n"))
}
