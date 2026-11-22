package nginx_test

// The parity harness's comparison (parity_test.go). Each case's two
// observations are diffed key by key, and every difference must be named by
// an entry of parityAcceptances, which says where it was accepted:
// LBALT-IMPL.md §4 or §7, a unit's parity decisions, or this harness's own
// proposals (parity_additions in its report). A difference no entry names
// fails the run: it is a front bug until the owner decides otherwise.
//
// Entries match narrowly: by key, by the values on each side, by whether
// both responses were the proxy's own, and by the case's tags, so an entry
// can't hide a difference it wasn't written for.

import (
	"encoding/json"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/urnetwork/server/nginx"
)

// The value of a key one side never set.
const parityAbsent = "<absent>"

// LBALT-IMPL §7: a public server's MaxHeaderBytes is this many times
// HttpSettings.MaxRequestHeadersTotalBytes, so the front itself answers
// nginx's 400 and 414 for any head under it.
const parityMaxHeaderBytesFactor = 2

// The bytes net/http reads past MaxHeaderBytes before it answers 431, the
// bufio slop of its initial read limit.
const parityGoHeaderSlopBytes = 4 * 1024

// One key whose values differ between nginx and the front.
type parityDifference struct {
	key   string
	nginx string
	front string
}

// One case's two observations, as an acceptance reads them.
type parityCheck struct {
	parityCase *parityCase
	nginx      *parityObservation
	front      *parityObservation
}

// The keys whose values differ, in the order nginx's observation set them,
// then the front's own.
func (self *parityCheck) differences() []*parityDifference {
	keys := slices.Clone(self.nginx.keys)
	for _, key := range self.front.keys {
		if _, ok := self.nginx.keyValues[key]; !ok {
			keys = append(keys, key)
		}
	}
	differences := []*parityDifference{}
	for _, key := range keys {
		nginxValue, nginxOk := self.nginx.keyValues[key]
		frontValue, frontOk := self.front.keyValues[key]
		if nginxOk == frontOk && nginxValue == frontValue {
			continue
		}
		if !nginxOk {
			nginxValue = parityAbsent
		}
		if !frontOk {
			frontValue = parityAbsent
		}
		differences = append(differences, &parityDifference{
			key:   key,
			nginx: nginxValue,
			front: frontValue,
		})
	}
	return differences
}

// Whether the case carries a tag.
func (self *parityCheck) hasTag(tag string) bool {
	return slices.Contains(self.parityCase.tags, tag)
}

// A side's value of a key, parityAbsent when unset.
func parityValue(observation *parityObservation, key string) string {
	value, ok := observation.keyValues[key]
	if !ok {
		return parityAbsent
	}
	return value
}

var parityResponseKeyRegexp = regexp.MustCompile(`^response\[(\d+)\]\.(.+)$`)
var parityUpstreamKeyRegexp = regexp.MustCompile(`^upstream\[(\d+)\]\.(.+)$`)
var parityConnKeyRegexp = regexp.MustCompile(`^conn\[(\d+)\]\.(.+)$`)

// The sequence and field of a response key.
func parityResponseKey(key string) (string, string, bool) {
	match := parityResponseKeyRegexp.FindStringSubmatch(key)
	if match == nil {
		return "", "", false
	}
	return match[1], match[2], true
}

// The index and field of an upstream key.
func parityUpstreamKey(key string) (string, string, bool) {
	match := parityUpstreamKeyRegexp.FindStringSubmatch(key)
	if match == nil {
		return "", "", false
	}
	return match[1], match[2], true
}

// The sequence and field of a connection key.
func parityConnKey(key string) (string, string, bool) {
	match := parityConnKeyRegexp.FindStringSubmatch(key)
	if match == nil {
		return "", "", false
	}
	return match[1], match[2], true
}

// A response's value on one side.
func parityResponseValue(observation *parityObservation, sequence string, field string) string {
	return parityValue(observation, "response["+sequence+"]."+field)
}

// The request a response's sequence sent, or nil when the case sent it some
// other way, as raw bytes or raw h2 fields. Read once the case is done.
func (self *parityCheck) sequenceRequest(sequence string) *parityRequest {
	index, err := strconv.Atoi(sequence)
	if err != nil {
		return nil
	}
	if request, ok := self.nginx.sequenceRequests[index]; ok {
		return request
	}
	return self.front.sequenceRequests[index]
}

// Whether a response's request was for the lb status route on an lb name,
// or on no name, which reaches the default server, an lb name's.
func (self *parityCheck) isLbStatusRequest(sequence string) bool {
	request := self.sequenceRequest(sequence)
	if request == nil || request.target != parityLbPrefix+"/status" {
		return false
	}
	switch request.host {
	case "", parityLbName, parityLbOtherName:
		return true
	default:
		return false
	}
}

// Whether both sides answered a request themselves, with the same status:
// nginx's or the front's own response, not an upstream's.
func (self *parityCheck) bothOwnResponses(sequence string) bool {
	return parityResponseValue(self.nginx, sequence, "generator") == "proxy" &&
		parityResponseValue(self.front, sequence, "generator") == "proxy" &&
		parityResponseValue(self.nginx, sequence, "status") == parityResponseValue(self.front, sequence, "status")
}

// A difference that LBALT-IMPL.md, a unit or this harness accepted.
type parityAcceptance struct {
	id string
	// where it was accepted
	source string
	// whether it names the difference
	match func(check *parityCheck, difference *parityDifference) bool
	// proposed by this harness for LBALT-IMPL.md §4, not yet accepted
	proposed bool
}

// The fields of a response the proxy wrote itself that carry its page:
// nginx's html, the front's text.
var parityOwnPageFields = []string{
	"body",
	"body.decoded",
	"framing",
	"header.Content-Type",
	"header.Content-Length",
	"header.Content-Encoding",
}

// The fields of nginx's static lb status response that its static module
// adds and the front's status json does not have.
var parityStaticFileFields = []string{
	"header.Last-Modified",
	"header.Etag",
	"header.Accept-Ranges",
}

// The always headers nginx adds to a service name's responses.
var paritySecurityHeaderFields = []string{
	"header.Strict-Transport-Security",
	"header.X-Frame-Options",
	"header.X-Content-Type-Options",
	"header.X-Xss-Protection",
}

// The accepted differences.
var parityAcceptances = []*parityAcceptance{
	{
		id:     "server-header",
		source: "LBALT-IMPL §4: no Server: nginx header",
		match: func(check *parityCheck, difference *parityDifference) bool {
			_, field, ok := parityResponseKey(difference.key)
			return ok && field == "header.Server" && difference.nginx == "nginx" && difference.front == parityAbsent
		},
	},
	{
		id:     "own-error-page",
		source: "LBALT-IMPL §4: no nginx html error pages, the status codes match; s-http: the front's own responses have text/plain bodies",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, field, ok := parityResponseKey(difference.key)
			if !ok || !slices.Contains(parityOwnPageFields, field) || !check.bothOwnResponses(sequence) {
				return false
			}
			status, _ := strconv.Atoi(parityResponseValue(check.nginx, sequence, "status"))
			return 300 <= status
		},
	},
	{
		id:     "slash-redirect",
		source: "LBALT-IMPL §4: no slash auto-redirect whose Location carries the internal listen port",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, _, ok := parityResponseKey(difference.key)
			if !ok || !check.hasTag("slash-redirect") {
				return false
			}
			return parityResponseValue(check.nginx, sequence, "status") == "301" &&
				strings.Contains(parityResponseValue(check.nginx, sequence, "header.Location"), ":"+strconv.Itoa(parityNginxHttpsPort)+"/") &&
				parityResponseValue(check.front, sequence, "status") == "404"
		},
	},
	{
		id:     "status-content-type",
		source: "LBALT-IMPL §4: no duplicate Content-Type on status routes",
		match: func(check *parityCheck, difference *parityDifference) bool {
			_, field, ok := parityResponseKey(difference.key)
			if !ok || field != "header.Content-Type" || difference.front != "application/json" {
				return false
			}
			nginxValues := strings.Split(difference.nginx, " | ")
			return len(nginxValues) == 2 && nginxValues[1] == "application/json"
		},
	},
	{
		id:     "lb-status-json",
		source: "LBALT-IMPL §2.6: the front's lb status json adds \"front\":\"lbalt\" (s-monitor parity decision)",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, field, ok := parityResponseKey(difference.key)
			if !ok || !check.hasTag("lb-status") || !check.isLbStatusRequest(sequence) {
				return false
			}
			// the two bodies differ only by the front's "front":"lbalt"
			bodiesMatch := func() bool {
				nginxJson, err := strconv.Unquote(parityResponseValue(check.nginx, sequence, "body"))
				if err != nil {
					return false
				}
				frontJson, err := strconv.Unquote(parityResponseValue(check.front, sequence, "body"))
				if err != nil {
					return false
				}
				nginxStatus := map[string]any{}
				frontStatus := map[string]any{}
				if json.Unmarshal([]byte(nginxJson), &nginxStatus) != nil || json.Unmarshal([]byte(frontJson), &frontStatus) != nil {
					return false
				}
				if frontStatus["front"] != "lbalt" {
					return false
				}
				delete(frontStatus, "front")
				return maps.Equal(nginxStatus, frontStatus)
			}
			if !bodiesMatch() {
				return false
			}
			switch field {
			case "body", "framing", "header.Content-Length":
				return true
			}
			return false
		},
	},
	{
		id:     "lb-status-static-file",
		source: "s-http parity decision: the lb status json has no Last-Modified, ETag or Accept-Ranges",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, field, ok := parityResponseKey(difference.key)
			return ok && check.hasTag("lb-status") && check.isLbStatusRequest(sequence) &&
				slices.Contains(parityStaticFileFields, field) && difference.front == parityAbsent
		},
	},
	{
		id:     "lb-status-range",
		source: "s-http parity decision: Range and conditional requests on the lb status get 200, not nginx static's 206 or 304",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, _, ok := parityResponseKey(difference.key)
			if !ok || !check.hasTag("lb-status-range") || !check.isLbStatusRequest(sequence) {
				return false
			}
			nginxStatus := parityResponseValue(check.nginx, sequence, "status")
			return (nginxStatus == "206" || nginxStatus == "304") && parityResponseValue(check.front, sequence, "status") == "200"
		},
	},
	{
		id:     "websocket-upstream-connection",
		source: "LBALT-IMPL §4: no duplicate Connection header to websocket upstreams (s-forward: one Connection: upgrade)",
		match: func(check *parityCheck, difference *parityDifference) bool {
			index, field, ok := parityUpstreamKey(difference.key)
			attemptIndex, _ := strconv.Atoi(index)
			return ok && field == "header.Connection" && difference.nginx == "keep-alive | upgrade" && difference.front == "upgrade" &&
				check.front.attemptIndexBlocks[attemptIndex] != parityInProcessBlock
		},
	},
	{
		id:     "in-process-switch-headers",
		source: "s-http parity decision: 101 responses written by connect in-process carry none of nginx's always headers",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, field, ok := parityResponseKey(difference.key)
			return ok && check.hasTag("in-process-upgrade") &&
				parityResponseValue(check.front, sequence, "status") == "101" &&
				slices.Contains(paritySecurityHeaderFields, field) && difference.front == parityAbsent
		},
	},
	{
		id:     "preflight-content-length",
		source: "s-http parity decision: Go's server removes the Content-Length: 0 nginx adds on a preflight's 204",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, field, ok := parityResponseKey(difference.key)
			return ok && field == "header.Content-Length" && difference.nginx == "0" && difference.front == parityAbsent &&
				parityResponseValue(check.front, sequence, "status") == "204"
		},
	},
	{
		id:     "connect-alpn",
		source: "D9 and s-tls parity decision: ALPN is http/1.1 only for connect names, where nginx offers h2",
		match: func(check *parityCheck, difference *parityDifference) bool {
			if !check.hasTag("connect-h2") {
				return false
			}
			if _, field, ok := parityConnKey(difference.key); ok {
				return field == "tls.alpn" && difference.nginx == "h2" && difference.front == "http/1.1"
			}
			if _, field, ok := parityResponseKey(difference.key); ok {
				return field == "proto" && difference.nginx == "HTTP/2.0" && difference.front == "HTTP/1.1"
			}
			return false
		},
	},
	{
		id:     "max-header-bytes-431",
		source: "LBALT-IMPL §7 header bytes: past MaxHeaderBytes, twice MaxRequestHeadersTotalBytes, and Go's read slop, Go answers 431 where nginx answers 400; inside that band the front answers 400 itself",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, field, ok := parityResponseKey(difference.key)
			if !ok || !check.hasTag("past-max-header-bytes") {
				return false
			}
			if parityResponseValue(check.nginx, sequence, "status") != "400" || parityResponseValue(check.front, sequence, "status") != "431" {
				return false
			}
			// a 431 inside the band is a server set up against §7, refusing
			// a head the front must answer itself
			request := check.sequenceRequest(sequence)
			if request == nil {
				return false
			}
			index, _ := strconv.Atoi(sequence)
			headBytes := len(request.h1Head("front/"+check.parityCase.name, index))
			bandBytes := parityMaxHeaderBytesFactor*nginx.DefaultHttpSettings().MaxRequestHeadersTotalBytes + parityGoHeaderSlopBytes
			if headBytes <= bandBytes {
				return false
			}
			return field == "status" || field == "header.Connection" || slices.Contains(parityOwnPageFields, field)
		},
	},
	{
		id:     "gzip-bytes",
		source: "s-http parity decision: gzip's compressed bytes differ from zlib's; the decompressed body and the headers match",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, field, ok := parityResponseKey(difference.key)
			if !ok || field != "body" {
				return false
			}
			return parityResponseValue(check.nginx, sequence, "header.Content-Encoding") == "gzip" &&
				parityResponseValue(check.front, sequence, "header.Content-Encoding") == "gzip" &&
				parityResponseValue(check.nginx, sequence, "body.decoded") == parityResponseValue(check.front, sequence, "body.decoded")
		},
	},
	{
		id:     "lb-status-head-length",
		source: "LBALT-IMPL §2.6: the front's lb status json adds \"front\":\"lbalt\", which a HEAD's Content-Length shows",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, field, ok := parityResponseKey(difference.key)
			if !ok || !check.hasTag("lb-status") || !check.isLbStatusRequest(sequence) || check.sequenceRequest(sequence).method != http.MethodHead {
				return false
			}
			nginxLength, nginxErr := strconv.Atoi(strings.TrimPrefix(difference.nginx, "length "))
			frontLength, frontErr := strconv.Atoi(strings.TrimPrefix(difference.front, "length "))
			return (field == "header.Content-Length" || field == "framing") && nginxErr == nil && frontErr == nil &&
				frontLength == nginxLength+len(`,"front":"lbalt"`)
		},
	},
	{
		id:     "trailer-request-header",
		source: "s-forward parity decision: the Trailer request header is dropped, where nginx passes it; neither sends trailers",
		match: func(check *parityCheck, difference *parityDifference) bool {
			_, field, ok := parityUpstreamKey(difference.key)
			return ok && field == "header.Trailer" && difference.front == parityAbsent
		},
	},
	{
		id:     "header-buffer-packing",
		source: "s-http parity decision: header limits are approximated; nginx packs whole lines into its eight 8 KiB buffers and refuses some heads under the 64 KiB the front allows",
		match: func(check *parityCheck, difference *parityDifference) bool {
			if !check.hasTag("header-packing") || parityValue(check.nginx, "response[0].status") != "400" || parityValue(check.front, "response[0].status") != "200" {
				return false
			}
			if _, _, isResponse := parityResponseKey(difference.key); isResponse {
				return true
			}
			if _, _, isUpstream := parityUpstreamKey(difference.key); !isUpstream && difference.key != "upstream.count" {
				return false
			}
			// what the front forwarded in nginx's place: the one request,
			// every header line of its own unchanged
			request := check.sequenceRequest("0")
			if request == nil || parityValue(check.front, "upstream.count") != "1" {
				return false
			}
			for _, headerLine := range request.headers {
				if parityValue(check.front, "upstream[0].header."+http.CanonicalHeaderKey(headerLine[0])) != headerLine[1] {
					return false
				}
			}
			return true
		},
	},

	// proposed by this harness (parity_additions in its report) and accepted
	// into LBALT-IMPL §4 at integration
	{
		id:     "keepalive-connection-header",
		source: "LBALT-IMPL §4: nginx sends Connection: keep-alive on a persistent HTTP/1.1 response, which Go's server leaves implicit",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, field, ok := parityResponseKey(difference.key)
			return ok && field == "header.Connection" && difference.nginx == "keep-alive" && difference.front == parityAbsent &&
				parityResponseValue(check.front, sequence, "proto") == "HTTP/1.1"
		},
	},
	{
		id:     "http10-status-line",
		source: "LBALT-IMPL §4: Go's server answers an HTTP/1.0 request with an HTTP/1.0 status line, where nginx's is HTTP/1.1",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, field, ok := parityResponseKey(difference.key)
			if !ok || field != "proto" || difference.nginx != "HTTP/1.1" || difference.front != "HTTP/1.0" {
				return false
			}
			request := check.sequenceRequest(sequence)
			return request != nil && request.proto == "HTTP/1.0"
		},
	},
	{
		id:     "tls-suite-choice",
		source: "LBALT-IMPL §4: within nginx's suites, Go chooses AES-128-GCM where OpenSSL under ssl_prefer_server_ciphers chooses AES-256-GCM; crypto/tls ignores a server's suite order and has no TLS 1.3 suite setting",
		match: func(check *parityCheck, difference *parityDifference) bool {
			_, field, ok := parityConnKey(difference.key)
			if !ok || field != "tls.cipher" || !check.hasTag("tls-suites") {
				return false
			}
			return (difference.nginx == "TLS_AES_256_GCM_SHA384" && difference.front == "TLS_AES_128_GCM_SHA256") ||
				(difference.nginx == "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384" && difference.front == "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256")
		},
	},
	{
		id:     "long-sni-default-certificate",
		source: "LBALT-IMPL §4, as s-tls chose: an SNI over 255 bytes gets the default server's certificate and routes by Host, where OpenSSL refuses the handshake with unrecognized_name",
		match: func(check *parityCheck, difference *parityDifference) bool {
			if !check.hasTag("long-sni") || parityValue(check.nginx, "conn[0].tls.error") != "tls: unrecognized name" || parityValue(check.front, "conn[0].tls.certificate") != parityLbName {
				return false
			}
			_, _, isConn := parityConnKey(difference.key)
			_, _, isResponse := parityResponseKey(difference.key)
			if isConn || isResponse {
				return true
			}
			if _, _, isUpstream := parityUpstreamKey(difference.key); !isUpstream && difference.key != "upstream.count" {
				return false
			}
			// routed by Host: the one request reached an upstream as sent
			request := check.sequenceRequest("0")
			return request != nil && parityValue(check.front, "upstream.count") == "1" &&
				parityValue(check.front, "upstream[0].host") == request.host &&
				parityValue(check.front, "upstream[0].uri") == request.target
		},
	},
	{
		id:     "content-length-beside-chunked",
		source: "LBALT-IMPL §4: Go's server reads a request with both Content-Length and Transfer-Encoding: chunked as chunked and drops the Content-Length before the handler sees it (RFC 9112 §6.3), and the front forwards the body re-framed, where nginx answers 400",
		match: func(check *parityCheck, difference *parityDifference) bool {
			if !check.hasTag("content-length-beside-chunked") || parityValue(check.nginx, "response[0].status") != "400" || parityValue(check.front, "response[0].status") != "200" {
				return false
			}
			_, _, isResponse := parityResponseKey(difference.key)
			_, _, isUpstream := parityUpstreamKey(difference.key)
			if !isResponse && !isUpstream && difference.key != "upstream.count" {
				return false
			}
			// what nothing smuggled looks like: each request the client sent
			// reached an upstream once, as its own sequence, with its body
			// re-framed by its length, not by the Content-Length beside the
			// chunks and not as the chunks' raw bytes
			sequences := slices.Sorted(maps.Keys(check.front.sequenceRequests))
			if parityValue(check.front, "upstream.count") != strconv.Itoa(len(sequences)) {
				return false
			}
			for i, sequence := range sequences {
				request := check.front.sequenceRequests[sequence]
				prefix := "upstream[" + strconv.Itoa(i) + "]."
				if parityValue(check.front, prefix+"sequence") != strconv.Itoa(sequence) ||
					parityValue(check.front, prefix+"method") != request.method ||
					parityValue(check.front, prefix+"uri") != request.target ||
					parityValue(check.front, prefix+"body") != parityValueOfBytes(request.body) ||
					parityValue(check.front, prefix+"framing") != "length "+strconv.Itoa(len(request.body)) {
					return false
				}
			}
			return true
		},
	},
	{
		id:     "obs-fold",
		source: "LBALT-IMPL §4: Go's server unfolds an obs-folded header line into the value before it (net/textproto) before any handler runs, and the front forwards the joined value, where nginx answers a header line that starts with a space 400 (ngx_http_parse_header_line)",
		match: func(check *parityCheck, difference *parityDifference) bool {
			if !check.hasTag("obs-fold") || parityValue(check.nginx, "response[0].status") != "400" || parityValue(check.front, "response[0].status") != "200" {
				return false
			}
			_, _, isResponse := parityResponseKey(difference.key)
			_, _, isUpstream := parityUpstreamKey(difference.key)
			if !isResponse && !isUpstream && difference.key != "upstream.count" {
				return false
			}
			// the one request reached an upstream once, as sent, but for a
			// folded line, whose parts went trimmed and joined by a space, as
			// textproto joins them
			request := check.sequenceRequest("0")
			if request == nil || parityValue(check.front, "upstream.count") != "1" ||
				parityValue(check.front, "upstream[0].method") != request.method ||
				parityValue(check.front, "upstream[0].uri") != request.target {
				return false
			}
			folded := false
			for _, headerLine := range request.headers {
				parts := strings.Split(headerLine[1], "\r\n")
				for i, part := range parts {
					parts[i] = strings.Trim(part, " \t")
				}
				folded = folded || 1 < len(parts)
				if parityValue(check.front, "upstream[0].header."+http.CanonicalHeaderKey(headerLine[0])) != strings.Join(parts, " ") {
					return false
				}
			}
			return folded
		},
	},
	{
		id:     "http10-transfer-encoding",
		source: "LBALT-IMPL §4: Go's server drops the Transfer-Encoding of an HTTP/1.0 request before any handler runs and reads the request as bodiless (net/http transfer.go, Issue 12785), so bytes sent as its body are the connection's next request, which the front forwards as its own, where nginx answers 400 (ngx_http_process_request_header); the front frames every request it forwards by its length, so no block reads one inside another",
		match: func(check *parityCheck, difference *parityDifference) bool {
			if !check.hasTag("http10-transfer-encoding") || parityValue(check.nginx, "response[0].status") != "400" || parityValue(check.front, "response[0].status") != "200" {
				return false
			}
			_, _, isResponse := parityResponseKey(difference.key)
			_, _, isUpstream := parityUpstreamKey(difference.key)
			if !isResponse && !isUpstream && difference.key != "upstream.count" {
				return false
			}
			// each request the front parsed reached an upstream once, as its
			// own sequence, framed by its length: the HTTP/1.0 one with no
			// body, then the one sent in its body's place
			sequences := slices.Sorted(maps.Keys(check.front.sequenceRequests))
			if len(sequences) < 2 || check.front.sequenceRequests[sequences[0]].proto != "HTTP/1.0" ||
				parityValue(check.front, "upstream.count") != strconv.Itoa(len(sequences)) {
				return false
			}
			for i, sequence := range sequences {
				request := check.front.sequenceRequests[sequence]
				prefix := "upstream[" + strconv.Itoa(i) + "]."
				if parityValue(check.front, prefix+"sequence") != strconv.Itoa(sequence) ||
					parityValue(check.front, prefix+"method") != request.method ||
					parityValue(check.front, prefix+"uri") != request.target ||
					parityValue(check.front, prefix+"body") != parityValueOfBytes(request.body) ||
					parityValue(check.front, prefix+"framing") != "length "+strconv.Itoa(len(request.body)) {
					return false
				}
			}
			return true
		},
	},
	{
		id:     "repeated-identical-content-length",
		source: "LBALT-IMPL §4: Go's h1 and h2 servers, and the vendored http3, merge repeated identical Content-Length values into one before any handler runs (net/http fixLength, the h2 server's newWriterAndRequest, http3 parseHeaders), and the front forwards the request framed by that one length, where nginx answers a second Content-Length 400 on h1 and h2 alike (ngx_http_process_unique_header_line)",
		match: func(check *parityCheck, difference *parityDifference) bool {
			if !check.hasTag("repeated-identical-content-length") || parityValue(check.nginx, "response[0].status") != "400" || parityValue(check.front, "response[0].status") != "200" {
				return false
			}
			_, _, isResponse := parityResponseKey(difference.key)
			_, _, isUpstream := parityUpstreamKey(difference.key)
			if !isResponse && !isUpstream && difference.key != "upstream.count" {
				return false
			}
			// the one request reached an upstream once, as sent, with every
			// Content-Length it sent its body's length, and was framed by
			// that one length
			request := check.sequenceRequest("0")
			if request == nil {
				return false
			}
			length := strconv.Itoa(len(request.body))
			contentLengthCount := 0
			for _, headerLine := range request.headers {
				if !strings.EqualFold(headerLine[0], "Content-Length") {
					continue
				}
				if headerLine[1] != length {
					return false
				}
				contentLengthCount += 1
			}
			return 2 <= contentLengthCount && parityValue(check.front, "upstream.count") == "1" &&
				parityValue(check.front, "upstream[0].sequence") == "0" &&
				parityValue(check.front, "upstream[0].method") == request.method &&
				parityValue(check.front, "upstream[0].uri") == request.target &&
				parityValue(check.front, "upstream[0].body") == parityValueOfBytes(request.body) &&
				parityValue(check.front, "upstream[0].framing") == "length "+length &&
				parityValue(check.front, "upstream[0].header.Content-Length") == length
		},
	},
	{
		id:     "in-process-hop-by-hop",
		source: "LBALT-IMPL §4: connect's in-process handler sees the client's own Connection header, where a connect block behind nginx sees nginx's keep-alive and upgrade, and writes its own 101 head",
		match: func(check *parityCheck, difference *parityDifference) bool {
			if index, field, ok := parityUpstreamKey(difference.key); ok {
				attemptIndex, _ := strconv.Atoi(index)
				return field == "header.Connection" && difference.nginx == "keep-alive | upgrade" &&
					check.front.attemptIndexBlocks[attemptIndex] == parityInProcessBlock
			}
			sequence, field, ok := parityResponseKey(difference.key)
			return ok && check.hasTag("in-process-upgrade") && field == "header.Connection" &&
				parityResponseValue(check.front, sequence, "status") == "101" &&
				strings.EqualFold(difference.nginx, difference.front)
		},
	},
	{
		id:     "streamed-chunked-whole",
		source: "LBALT-IMPL §4: on a streamed route a chunked body streams chunked though it came whole with its head, where nginx's first pass buffers it and sends its length",
		match: func(check *parityCheck, difference *parityDifference) bool {
			index, field, ok := parityUpstreamKey(difference.key)
			if !ok || !check.hasTag("streamed-chunked-whole") {
				return false
			}
			// the block got the body the client sent on both sides, framed
			// by its chunks on the front's and by its length on nginx's
			prefix := "upstream[" + index + "]."
			request := check.sequenceRequest(parityValue(check.front, prefix+"sequence"))
			if request == nil || !request.chunked ||
				parityValue(check.front, prefix+"body") != parityValueOfBytes(request.body) ||
				parityValue(check.nginx, prefix+"body") != parityValueOfBytes(request.body) {
				return false
			}
			length := strconv.Itoa(len(request.body))
			switch field {
			case "framing":
				return difference.front == "chunked" && difference.nginx == "length "+length
			case "header.Content-Length":
				return difference.front == parityAbsent && difference.nginx == length
			default:
				return false
			}
		},
	},
	{
		id:     "retry-after",
		source: "LBALTOPT2 R15 (a): the front's own 429 carries Retry-After, the refused bucket's wait in whole seconds from 1 to 60, where nginx's has none",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, field, ok := parityResponseKey(difference.key)
			if !ok || field != "header.Retry-After" || difference.nginx != parityAbsent || !check.bothOwnResponses(sequence) {
				return false
			}
			if parityResponseValue(check.front, sequence, "status") != "429" {
				return false
			}
			seconds, err := strconv.Atoi(difference.front)
			return err == nil && 1 <= seconds && seconds <= 60 && strconv.Itoa(seconds) == difference.front
		},
	},
}

// The first acceptance that names a difference, or nil.
func (self *parityCheck) acceptance(difference *parityDifference) *parityAcceptance {
	for _, acceptance := range parityAcceptances {
		if acceptance.match(self, difference) {
			return acceptance
		}
	}
	return nil
}
