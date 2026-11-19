package nginx

// Request validation (owner: s-http): what nginx checks while it reads a
// request, before any server or location applies, and so before the limits
// and without added headers:
//
//   - the method is one or more of 'A' to 'Z', '_' and '-', as nginx's
//     parsers require on h1 and h2, else 400, ahead of every check below.
//     Go's servers accept any token, "get" or "M.2" say. The rule applies on
//     h3 too, where alt took any token, so the front checks every protocol
//     alike;
//   - the request target is origin form or absolute form, and its uri
//     parses (route_http_uri.go): else 400;
//   - the request line fits HttpSettings.MaxRequestLineBytes, else 414; a
//     header fits MaxRequestHeaderBytes and the whole head
//     MaxRequestHeadersTotalBytes, else 400;
//   - a header name outside [A-Za-z0-9-] is dropped, as nginx ignores it
//     (ignore_invalid_headers on, underscores_in_headers off);
//   - the headers kept number at most MaxRequestHeaderLines, nginx's
//     max_headers, else 400: on h1 every line, Host included, and on h2 and
//     h3 every field but the pseudo-headers and Cookie, which nginx merges
//     uncounted (ngx_http_process_request_headers,
//     ngx_http_v2_state_process_header). nginx counts the lines it received,
//     and the h1 count can't tell two lines Go's server may make up from the
//     client's own: the Cache-Control: no-cache it adds beside a Pragma:
//     no-cache that came without one (fixPragmaCacheControl), and the Host
//     of an HTTP/1.0 request in absolute form, which it takes from the
//     target whether or not a Host line came. So a head of exactly
//     max_headers lines with either is a 400 nginx would serve
//     (LBALT-IMPL.md §4);
//   - a second line of a header nginx keeps once is a 400
//     (ngx_http_process_unique_header_line), and so is a second Host field
//     on h2 and h3 (ngx_http_process_host);
//   - on h2 and h3, a Host header that differs from :authority is a 400, and
//     neither is a 400;
//   - the host is valid, as nginx's $host normalization decides
//     (normalizeHost), else 400. An HTTP/1.1 request with no host is a 400;
//     HTTP/1.0 may send none;
//   - TRACE and CONNECT are 405.
//
// Go's servers refuse part of this first, with their own statuses: an h1
// head over the server's MaxHeaderBytes or MaxHeaderValueCount (431), a
// missing, malformed or repeated Host on h1 (400, as nginx answers a
// repeated one), an h2 or h3 head over its header list limit. So
// Front.ConfigureServer sets MaxHeaderBytes to a multiple of
// MaxRequestHeadersTotalBytes, and MaxHeaderValueCount to a multiple of
// MaxRequestHeaderLines, twice by default
// (HttpSettings.ServerHeaderBytesMultiple, ServerHeaderLinesMultiple), for
// the handler to apply nginx's statuses within that.
//
// Go's servers also rewrite some heads before the handler sees them, which
// the handler can't refuse as nginx does (LBALT-IMPL.md §4). Go's h1 and h2
// servers and the vendored http3 merge repeated identical Content-Length
// values into one (net/http's fixLength, the h2 server's
// newWriterAndRequest, http3's parseHeaders), where nginx answers a second
// Content-Length 400 on h1 and h2 alike (ngx_http_process_unique_header_line),
// and the front forwards the request framed by that one length. Different
// values they refuse: 400 on h1, as nginx answers, and a stream reset on h2
// (PROTOCOL_ERROR) and h3 (H3_MESSAGE_ERROR), where nginx's h2 answers 400.
// Go's h1 server also drops the Transfer-Encoding of an HTTP/1.0 request and
// unfolds an obs-folded line, each of which nginx answers 400; it answers a
// repeated Transfer-Encoding 501, which nginx answers 400; and an Expect
// other than 100-continue 417, which nginx ignores.

import (
	"net/http"
	"net/netip"
)

// The request headers nginx keeps once, answering a second line with 400
// (ngx_http_headers_in's ngx_http_process_unique_header_line). Go's servers
// merge or refuse a repeated Content-Length, and Go's h1 server takes
// Transfer-Encoding out of the header map, which the h2 server and the
// vendored http3 refuse, so those two never repeat here. Keys are canonical.
var routeRequestUniqueHeaders = []string{
	"If-Modified-Since",
	"If-Unmodified-Since",
	"If-Match",
	"If-None-Match",
	"Content-Length",
	"Content-Range",
	"If-Range",
	"Transfer-Encoding",
	"Expect",
	"Authorization",
	"Proxy-Authorization",
}

// One request as routing reads it, once validated.
type routeRequest struct {
	// nginx's $host: the normalized host, empty when the request named
	// none, which only HTTP/1.0 may do
	host string
	// the name the host matched, valid when hostMatched
	hostMatch   nameMatch
	hostMatched bool
	// the target's normalized path and its query
	uri routeUri
	// nginx's $request_uri: the raw target in origin form, for redirects
	requestUri string
	// the accepted peer, with an IPv4-mapped address unmapped
	clientAddrPort netip.AddrPort
}

// Validates a request as nginx does before choosing a location, dropping
// the header names nginx ignores and an h2 or h3 Host header that agrees
// with :authority. A non-zero status is the answer when the request is
// refused.
func readRouteRequest(r *http.Request, names frontNames, httpSettings *HttpSettings) (*routeRequest, int) {
	peerAddrPort, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		// a server always sets the peer; one the front can't read is never
		// served, so nothing acts for an address it doesn't know
		return nil, http.StatusInternalServerError
	}
	clientAddrPort := netip.AddrPortFrom(peerAddrPort.Addr().Unmap().WithZone(""), peerAddrPort.Port())

	// nginx parses the method first, so a long request line with an invalid
	// method is a 400, not a 414
	if !validRouteMethod(r.Method) {
		return nil, http.StatusBadRequest
	}

	requestLineBytes := len(r.Method) + len(" ") + len(r.RequestURI) + len(" ") + len(r.Proto) + len("\r\n")
	if 0 < httpSettings.MaxRequestLineBytes && httpSettings.MaxRequestLineBytes < requestLineBytes {
		return nil, http.StatusRequestURITooLong
	}

	request := &routeRequest{
		clientAddrPort: clientAddrPort,
	}
	if r.Method != http.MethodConnect {
		requestUri, ok := routeRequestUri(r.RequestURI)
		if !ok {
			return nil, http.StatusBadRequest
		}
		uri, ok := parseRouteUri(requestUri)
		if !ok {
			return nil, http.StatusBadRequest
		}
		request.requestUri = requestUri
		request.uri = uri
	}

	headBytes := requestLineBytes + len("\r\n")
	addHeaderLine := func(name string, value string) bool {
		headerLineBytes := len(name) + len(": ") + len(value) + len("\r\n")
		headBytes += headerLineBytes
		return httpSettings.MaxRequestHeaderBytes <= 0 || headerLineBytes <= httpSettings.MaxRequestHeaderBytes
	}
	if r.ProtoMajor == 1 && r.Host != "" {
		// Go's h1 server moves Host out of the header map
		if !addHeaderLine("Host", r.Host) {
			return nil, http.StatusBadRequest
		}
	}
	for name, values := range r.Header {
		for _, value := range values {
			if !addHeaderLine(name, value) {
				return nil, http.StatusBadRequest
			}
		}
	}
	if 0 < httpSettings.MaxRequestHeadersTotalBytes && httpSettings.MaxRequestHeadersTotalBytes < headBytes {
		return nil, http.StatusBadRequest
	}

	for name := range r.Header {
		if !validRouteHeaderName(name) {
			delete(r.Header, name)
		}
	}

	if maxRequestHeaderLines := httpSettings.MaxRequestHeaderLines; 0 < maxRequestHeaderLines {
		headerLineCount := 0
		if r.ProtoMajor == 1 {
			// the lines Go's h1 server takes out of the header map. An
			// absolute target's host counts as a Host line whether or not
			// one came, as a Cache-Control the server added does (the file
			// comment)
			if r.Host != "" {
				headerLineCount += 1
			}
			headerLineCount += len(r.TransferEncoding)
		}
		for name, values := range r.Header {
			if 2 <= r.ProtoMajor && name == "Cookie" {
				continue
			}
			headerLineCount += len(values)
		}
		if maxRequestHeaderLines < headerLineCount {
			return nil, http.StatusBadRequest
		}
	}

	for _, name := range routeRequestUniqueHeaders {
		if 1 < len(r.Header[name]) {
			return nil, http.StatusBadRequest
		}
	}

	if 2 <= r.ProtoMajor {
		// Go keeps a Host header on h2 and h3, and sets r.Host from it only
		// when there is no :authority. nginx refuses a second one, as on h1
		if 1 < len(r.Header.Values("Host")) {
			return nil, http.StatusBadRequest
		}
		for _, host := range r.Header.Values("Host") {
			if host != r.Host {
				return nil, http.StatusBadRequest
			}
		}
		r.Header.Del("Host")
	}

	if r.Host != "" {
		host, ok := names.normalizeHost(r.Host)
		if !ok {
			return nil, http.StatusBadRequest
		}
		request.host = host
		request.hostMatch, request.hostMatched = names.match(host)
	} else if r.ProtoAtLeast(1, 1) {
		return nil, http.StatusBadRequest
	}

	switch r.Method {
	case http.MethodTrace, http.MethodConnect:
		return nil, http.StatusMethodNotAllowed
	}
	return request, 0
}

// Whether nginx's parser takes a method: one or more of 'A' to 'Z', '_' and
// '-' (ngx_http_parse_request_line, ngx_http_v2_parse_method).
func validRouteMethod(method string) bool {
	if method == "" {
		return false
	}
	for i := 0; i < len(method); i += 1 {
		c := method[i]
		if !('A' <= c && c <= 'Z' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// Whether nginx keeps a request header of this name: only letters, digits
// and '-' (ngx_http_parse_header_line with underscores_in_headers off).
func validRouteHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i += 1 {
		c := name[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
