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
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The value of a key one side never set.
const parityAbsent = "<absent>"

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

// Whether two lb status bodies differ only by the front's "front":"lbalt"
// (LBALT-IMPL.md §2.6).
func parityLbStatusBodiesMatch(nginxBody string, frontBody string) bool {
	nginxJson, err := strconv.Unquote(nginxBody)
	if err != nil {
		return false
	}
	frontJson, err := strconv.Unquote(frontBody)
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
			if !ok || !check.hasTag("lb-status") {
				return false
			}
			if !parityLbStatusBodiesMatch(parityResponseValue(check.nginx, sequence, "body"), parityResponseValue(check.front, sequence, "body")) {
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
			_, field, ok := parityResponseKey(difference.key)
			return ok && check.hasTag("lb-status") && slices.Contains(parityStaticFileFields, field) && difference.front == parityAbsent
		},
	},
	{
		id:     "lb-status-range",
		source: "s-http parity decision: Range and conditional requests on the lb status get 200, not nginx static's 206 or 304",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, _, ok := parityResponseKey(difference.key)
			if !ok || !check.hasTag("lb-status-range") {
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
		source: "LBALT-IMPL §7 header bytes, s-connect: past the server's MaxHeaderBytes Go answers 431 where nginx answers 400",
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, field, ok := parityResponseKey(difference.key)
			if !ok || !check.hasTag("past-max-header-bytes") {
				return false
			}
			if parityResponseValue(check.nginx, sequence, "status") != "400" || parityResponseValue(check.front, sequence, "status") != "431" {
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
			_, field, ok := parityResponseKey(difference.key)
			if !ok || !check.hasTag("lb-status") || check.parityCase.request == nil || check.parityCase.request.method != "HEAD" {
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
			_, _, isResponse := parityResponseKey(difference.key)
			_, _, isUpstream := parityUpstreamKey(difference.key)
			return isResponse || isUpstream || difference.key == "upstream.count"
		},
	},

	// proposed by this harness for LBALT-IMPL §4 (parity_additions in its
	// report); until the owner accepts them they fail the run like any other
	// difference
	{
		id:       "keepalive-connection-header",
		source:   "proposed: nginx sends Connection: keep-alive on a persistent HTTP/1.1 response, which Go's server leaves implicit",
		proposed: true,
		match: func(check *parityCheck, difference *parityDifference) bool {
			sequence, field, ok := parityResponseKey(difference.key)
			return ok && field == "header.Connection" && difference.nginx == "keep-alive" && difference.front == parityAbsent &&
				parityResponseValue(check.front, sequence, "proto") == "HTTP/1.1"
		},
	},
	{
		id:       "http10-status-line",
		source:   "proposed: Go's server answers an HTTP/1.0 request with an HTTP/1.0 status line, where nginx's is HTTP/1.1",
		proposed: true,
		match: func(check *parityCheck, difference *parityDifference) bool {
			_, field, ok := parityResponseKey(difference.key)
			return ok && field == "proto" && difference.nginx == "HTTP/1.1" && difference.front == "HTTP/1.0" &&
				check.parityCase.request != nil && check.parityCase.request.proto == "HTTP/1.0"
		},
	},
	{
		id:       "tls-suite-choice",
		source:   "proposed: within nginx's suites, Go chooses AES-128-GCM where OpenSSL under ssl_prefer_server_ciphers chooses AES-256-GCM; crypto/tls ignores a server's suite order and has no TLS 1.3 suite setting",
		proposed: true,
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
		id:       "long-sni-default-certificate",
		source:   "proposed, as s-tls chose (its fix report): an SNI over 255 bytes gets the default server's certificate and routes by Host, where OpenSSL refuses the handshake with unrecognized_name",
		proposed: true,
		match: func(check *parityCheck, difference *parityDifference) bool {
			if !check.hasTag("long-sni") || parityValue(check.nginx, "conn[0].tls.error") != "tls: unrecognized name" || parityValue(check.front, "conn[0].tls.certificate") != parityLbName {
				return false
			}
			_, _, isConn := parityConnKey(difference.key)
			_, _, isResponse := parityResponseKey(difference.key)
			_, _, isUpstream := parityUpstreamKey(difference.key)
			return isConn || isResponse || isUpstream || difference.key == "upstream.count"
		},
	},
	{
		id:       "content-length-beside-chunked",
		source:   "proposed: Go's server reads a request with both Content-Length and Transfer-Encoding: chunked as chunked and drops the Content-Length before the handler sees it (RFC 9112 §6.3), and the front forwards the body re-framed, where nginx answers 400",
		proposed: true,
		match: func(check *parityCheck, difference *parityDifference) bool {
			if !check.hasTag("content-length-beside-chunked") || parityValue(check.nginx, "response[0].status") != "400" || parityValue(check.front, "response[0].status") != "200" {
				return false
			}
			_, _, isResponse := parityResponseKey(difference.key)
			_, _, isUpstream := parityUpstreamKey(difference.key)
			return isResponse || isUpstream || difference.key == "upstream.count"
		},
	},
	{
		id:       "in-process-hop-by-hop",
		source:   "proposed: connect's in-process handler sees the client's own Connection header, where a connect block behind nginx sees nginx's keep-alive and upgrade, and writes its own 101 head",
		proposed: true,
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
