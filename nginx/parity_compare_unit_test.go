package nginx_test

// Tests of the parity harness that run without Docker. Its comparison
// (parity_compare_test.go): every changed key is a difference, each
// acceptance names the difference it was written for and refuses its near
// misses, and a difference no entry names is left unaccepted, so the harness
// fails on it. Its recording upstreams (parity_upstream_test.go): a scenario
// meets each request's attempts as that request's own. Its timeout cases
// (parity_corpus_test.go): each straddles the timeout it is for.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/nginx"
)

// An observation with the given keys and values, in order, and the blocks
// its upstream attempts reached.
func newParityTestObservation(keyValues [][2]string, attemptBlocks ...string) *parityObservation {
	observation := newParityObservation()
	for _, keyValue := range keyValues {
		observation.set(keyValue[0], keyValue[1])
	}
	for i, block := range attemptBlocks {
		observation.attemptIndexBlocks[i] = block
	}
	return observation
}

// Every key whose value differs, or that one side never set, is a
// difference; equal keys are not.
func TestParityDifferencesNameEveryChangedKey(t *testing.T) {
	check := &parityCheck{
		parityCase: &parityCase{name: "unit"},
		nginx: newParityTestObservation([][2]string{
			{"response[0].status", "200"},
			{"response[0].header.Server", "nginx"},
			{"upstream.count", "1"},
		}),
		front: newParityTestObservation([][2]string{
			{"response[0].status", "200"},
			{"upstream.count", "2"},
			{"upstream[1].status", "200"},
		}),
	}
	differences := check.differences()
	want := []parityDifference{
		{key: "response[0].header.Server", nginx: "nginx", front: parityAbsent},
		{key: "upstream.count", nginx: "1", front: "2"},
		{key: "upstream[1].status", nginx: parityAbsent, front: "200"},
	}
	if len(differences) != len(want) {
		t.Fatalf("got %d differences, want %d: %+v", len(differences), len(want), differences)
	}
	for i, difference := range differences {
		if *difference != want[i] {
			t.Errorf("difference %d = %+v, want %+v", i, *difference, want[i])
		}
	}
}

// Each acceptance names the difference it was written for, and a near miss
// of it is either named by none or by the right other entry; a difference
// in what a client or upstream saw, such as a status or a body from the
// upstream, is named by none, so the harness fails on it.
func TestParityAcceptancesMatchOnlyTheirDifferences(t *testing.T) {
	ownPage := func(status string, generator string) [][2]string {
		return [][2]string{
			{"response[0].status", status},
			{"response[0].generator", generator},
		}
	}
	statusBody := func(body string) [][2]string {
		return [][2]string{{"response[0].body", body}}
	}
	nginxStatusBody := `"{\"version\":\"0.0.0-parity\",\"status\":\"ok\"}\n"`
	frontStatusBody := `"{\"version\":\"0.0.0-parity\",\"status\":\"ok\",\"front\":\"lbalt\"}\n"`
	lbStatus := parityGet(parityLbName, parityLbName, parityLbPrefix+"/status")
	lbStatusRange := parityWith(lbStatus, "", [2]string{"Range", "bytes=0-3"})
	apiGet := parityGet(parityApiName, parityApiName, "/one")
	// an api request whose head is count header lines of 6600 bytes
	filledHead := func(count int) *parityRequest {
		request := parityGet(parityApiName, parityApiName, "/hello")
		for i := range count {
			request = parityWith(request, "", parityLongHeader("X-Parity-Fill-"+strconv.Itoa(i), 6600))
		}
		return request
	}
	// nine 6600-byte lines, which nginx's packed buffers refuse
	packedHead := filledHead(9)
	// the front's observation of a packed head it forwarded, its fourth
	// line cut short when cut
	forwardedPackedHead := func(attemptCount int, cut bool) [][2]string {
		keyValues := [][2]string{
			{"response[0].status", "200"},
			{"upstream.count", strconv.Itoa(attemptCount)},
		}
		for i, headerLine := range packedHead.headers {
			value := headerLine[1]
			if cut && i == 3 {
				value = value[:len(value)/2]
			}
			keyValues = append(keyValues, [2]string{"upstream[0].header." + headerLine[0], value})
		}
		return keyValues
	}
	longSni := parityGet(strings.Repeat(strings.Repeat("a", 63)+".", 4)+parityUnknownName, parityApiName, "/hello")
	longSniRefused := [][2]string{{"conn[0].tls.error", "tls: unrecognized name"}}
	// the front's observation of the long sni request it routed by host
	longSniRouted := func(certificate string, host string) [][2]string {
		return [][2]string{
			{"conn[0].tls.certificate", certificate},
			{"response[0].status", "200"},
			{"upstream.count", "1"},
			{"upstream[0].host", host},
			{"upstream[0].uri", "/hello"},
		}
	}
	http10 := &parityRequest{scheme: "https", sni: parityApiName, method: http.MethodGet, target: "/hello", host: parityApiName, proto: "HTTP/1.0"}
	// a chunked body beside a Content-Length, alone and then followed by a
	// second request on its connection
	chunkedBesideLength := parityWith(parityWithBody(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, []byte("hello"), true), "", [2]string{"Content-Length", "5"}, [2]string{"Transfer-Encoding", "chunked"})
	chunkedBesideLengthKeptAlive := *chunkedBesideLength
	chunkedBesideLengthKeptAlive.keepalive = true
	secondRequest := parityGet(parityApiName, parityApiName, "/two")
	// one upstream attempt as the front's observation records it
	type forwardedAttempt struct {
		sequence int
		method   string
		uri      string
		body     string
		framing  string
	}
	// the front's observation of a case its upstreams answered 200
	forwarded := func(attempts ...forwardedAttempt) [][2]string {
		keyValues := [][2]string{
			{"response[0].status", "200"},
			{"upstream.count", strconv.Itoa(len(attempts))},
		}
		for i, attempt := range attempts {
			prefix := "upstream[" + strconv.Itoa(i) + "]."
			keyValues = append(
				keyValues,
				[2]string{prefix + "sequence", strconv.Itoa(attempt.sequence)},
				[2]string{prefix + "method", attempt.method},
				[2]string{prefix + "uri", attempt.uri},
				[2]string{prefix + "body", strconv.Quote(attempt.body)},
				[2]string{prefix + "framing", attempt.framing},
			)
		}
		return keyValues
	}
	reframed := forwardedAttempt{sequence: 0, method: http.MethodPost, uri: "/upload", body: "hello", framing: "length 5"}
	followed := forwardedAttempt{sequence: 1, method: http.MethodGet, uri: "/two", body: "", framing: "length 0"}
	cases := []struct {
		name       string
		parityCase *parityCase
		// the request each sequence sent, on both sides
		requests     []*parityRequest
		nginx        [][2]string
		front        [][2]string
		frontBlocks  []string
		difference   parityDifference
		wantId       string
		wantProposed bool
		wantUnnamed  bool
	}{
		{
			name:       "nginx's Server header",
			difference: parityDifference{key: "response[0].header.Server", nginx: "nginx", front: parityAbsent},
			wantId:     "server-header",
		},
		{
			name:        "a Server header the front sends",
			difference:  parityDifference{key: "response[0].header.Server", nginx: "nginx", front: "front"},
			wantUnnamed: true,
		},
		{
			name:       "the body of both sides' own 404",
			nginx:      ownPage("404", "proxy"),
			front:      ownPage("404", "proxy"),
			difference: parityDifference{key: "response[0].body", nginx: `"<html>"`, front: `"Not Found\n"`},
			wantId:     "own-error-page",
		},
		{
			name:        "the body of an upstream's 404 the front changed",
			nginx:       ownPage("404", "upstream"),
			front:       ownPage("404", "upstream"),
			difference:  parityDifference{key: "response[0].body", nginx: `"gone"`, front: `"Not Found\n"`},
			wantUnnamed: true,
		},
		{
			name:        "the body of the front's own 200",
			nginx:       ownPage("200", "proxy"),
			front:       ownPage("200", "proxy"),
			difference:  parityDifference{key: "response[0].body", nginx: `"a"`, front: `"b"`},
			wantUnnamed: true,
		},
		{
			name:        "a status that differs",
			nginx:       ownPage("200", "upstream"),
			front:       ownPage("503", "upstream"),
			difference:  parityDifference{key: "response[0].status", nginx: "200", front: "503"},
			wantUnnamed: true,
		},
		{
			name:       "the slash redirect nginx sends with its internal port",
			parityCase: &parityCase{name: "unit", tags: []string{"slash-redirect"}},
			nginx:      [][2]string{{"response[0].status", "301"}, {"response[0].header.Location", "https://test-lb.service.example:8443/hidden-parity/by/service/api/"}},
			front:      [][2]string{{"response[0].status", "404"}},
			difference: parityDifference{key: "response[0].status", nginx: "301", front: "404"},
			wantId:     "slash-redirect",
		},
		{
			name:        "a slash redirect the front answers 200",
			parityCase:  &parityCase{name: "unit", tags: []string{"slash-redirect"}},
			nginx:       [][2]string{{"response[0].status", "301"}, {"response[0].header.Location", "https://test-lb.service.example:8443/hidden-parity/by/service/api/"}},
			front:       [][2]string{{"response[0].status", "200"}},
			difference:  parityDifference{key: "response[0].status", nginx: "301", front: "200"},
			wantUnnamed: true,
		},
		{
			name:       "nginx's second Content-Type on a status route",
			difference: parityDifference{key: "response[0].header.Content-Type", nginx: "application/octet-stream | application/json", front: "application/json"},
			wantId:     "status-content-type",
		},
		{
			name:        "a Content-Type the front changed",
			difference:  parityDifference{key: "response[0].header.Content-Type", nginx: "text/html", front: "application/json"},
			wantUnnamed: true,
		},
		{
			name:       "the front field of the lb status json",
			parityCase: &parityCase{name: "unit", tags: []string{"lb-status"}},
			requests:   []*parityRequest{lbStatus},
			nginx:      statusBody(nginxStatusBody),
			front:      statusBody(frontStatusBody),
			difference: parityDifference{key: "response[0].body", nginx: nginxStatusBody, front: frontStatusBody},
			wantId:     "lb-status-json",
		},
		{
			name:        "an lb status json with another version",
			parityCase:  &parityCase{name: "unit", tags: []string{"lb-status"}},
			requests:    []*parityRequest{lbStatus},
			nginx:       statusBody(nginxStatusBody),
			front:       statusBody(`"{\"version\":\"0.0.1\",\"status\":\"ok\",\"front\":\"lbalt\"}\n"`),
			difference:  parityDifference{key: "response[0].body", nginx: nginxStatusBody, front: `"{\"version\":\"0.0.1\",\"status\":\"ok\",\"front\":\"lbalt\"}\n"`},
			wantUnnamed: true,
		},
		{
			name:        "an lb status json from an upstream, beside the case's status request",
			parityCase:  &parityCase{name: "unit", tags: []string{"lb-status"}},
			requests:    []*parityRequest{apiGet, lbStatus},
			nginx:       statusBody(nginxStatusBody),
			front:       statusBody(frontStatusBody),
			difference:  parityDifference{key: "response[0].body", nginx: nginxStatusBody, front: frontStatusBody},
			wantUnnamed: true,
		},
		{
			name:       "a static file header the lb status json has none of",
			parityCase: &parityCase{name: "unit", tags: []string{"lb-status"}},
			requests:   []*parityRequest{lbStatus},
			difference: parityDifference{key: "response[0].header.Etag", nginx: `"6afe918d-29"`, front: parityAbsent},
			wantId:     "lb-status-static-file",
		},
		{
			name:       "a static file header of a status request with no Host, which reaches the lb default server",
			parityCase: &parityCase{name: "unit", tags: []string{"lb-status"}},
			requests:   []*parityRequest{{scheme: "https", method: http.MethodGet, target: parityLbPrefix + "/status", proto: "HTTP/1.0"}},
			difference: parityDifference{key: "response[0].header.Last-Modified", nginx: "Thu, 19 Nov 2026 06:05:01 GMT", front: parityAbsent},
			wantId:     "lb-status-static-file",
		},
		{
			name:        "a static file header dropped from another request of an lb status case",
			parityCase:  &parityCase{name: "unit", tags: []string{"lb-status"}},
			requests:    []*parityRequest{apiGet, lbStatus},
			difference:  parityDifference{key: "response[0].header.Etag", nginx: `"upstream-1"`, front: parityAbsent},
			wantUnnamed: true,
		},
		{
			name:        "a static file header the front sends another value of",
			parityCase:  &parityCase{name: "unit", tags: []string{"lb-status"}},
			requests:    []*parityRequest{lbStatus},
			difference:  parityDifference{key: "response[0].header.Etag", nginx: `"6afe918d-29"`, front: `"front-1"`},
			wantUnnamed: true,
		},
		{
			name:        "a static file header dropped from a status request on a service name",
			parityCase:  &parityCase{name: "unit", tags: []string{"lb-status"}},
			requests:    []*parityRequest{parityGet(parityApiName, parityApiName, parityLbPrefix+"/status")},
			difference:  parityDifference{key: "response[0].header.Etag", nginx: `"upstream-1"`, front: parityAbsent},
			wantUnnamed: true,
		},
		{
			name:       "the 206 nginx's static module answers a Range on the lb status",
			parityCase: &parityCase{name: "unit", tags: []string{"lb-status", "lb-status-range"}},
			requests:   []*parityRequest{lbStatusRange},
			nginx:      [][2]string{{"response[0].status", "206"}},
			front:      [][2]string{{"response[0].status", "200"}},
			difference: parityDifference{key: "response[0].header.Content-Range", nginx: "bytes 0-3/41", front: parityAbsent},
			wantId:     "lb-status-range",
		},
		{
			name:        "a Range on the lb status the front refused",
			parityCase:  &parityCase{name: "unit", tags: []string{"lb-status", "lb-status-range"}},
			requests:    []*parityRequest{lbStatusRange},
			nginx:       [][2]string{{"response[0].status", "206"}},
			front:       [][2]string{{"response[0].status", "416"}},
			difference:  parityDifference{key: "response[0].status", nginx: "206", front: "416"},
			wantUnnamed: true,
		},
		{
			name:        "an upstream's 206 the front answered 200, beside the case's Range on the lb status",
			parityCase:  &parityCase{name: "unit", tags: []string{"lb-status", "lb-status-range"}},
			requests:    []*parityRequest{apiGet, lbStatusRange},
			nginx:       [][2]string{{"response[0].status", "206"}},
			front:       [][2]string{{"response[0].status", "200"}},
			difference:  parityDifference{key: "response[0].status", nginx: "206", front: "200"},
			wantUnnamed: true,
		},
		{
			name:       "the longer Content-Length of a HEAD of the lb status",
			parityCase: &parityCase{name: "unit", tags: []string{"lb-status"}},
			requests:   []*parityRequest{parityWith(lbStatus, http.MethodHead)},
			difference: parityDifference{key: "response[0].header.Content-Length", nginx: "41", front: "57"},
			wantId:     "lb-status-head-length",
		},
		{
			name:        "a HEAD's Content-Length longer by more than the front field",
			parityCase:  &parityCase{name: "unit", tags: []string{"lb-status"}},
			requests:    []*parityRequest{parityWith(lbStatus, http.MethodHead)},
			difference:  parityDifference{key: "response[0].header.Content-Length", nginx: "41", front: "58"},
			wantUnnamed: true,
		},
		{
			name:        "a longer Content-Length on a GET of the lb status, whose body must account for it",
			parityCase:  &parityCase{name: "unit", tags: []string{"lb-status"}},
			requests:    []*parityRequest{lbStatus},
			difference:  parityDifference{key: "response[0].header.Content-Length", nginx: "41", front: "57"},
			wantUnnamed: true,
		},
		{
			name:        "nginx's two Connection headers to a forwarded websocket",
			frontBlocks: []string{"g1"},
			difference:  parityDifference{key: "upstream[0].header.Connection", nginx: "keep-alive | upgrade", front: "upgrade"},
			wantId:      "websocket-upstream-connection",
		},
		{
			name:         "the client's own Connection header in-process",
			frontBlocks:  []string{parityInProcessBlock},
			difference:   parityDifference{key: "upstream[0].header.Connection", nginx: "keep-alive | upgrade", front: "close"},
			wantId:       "in-process-hop-by-hop",
			wantProposed: true,
		},
		{
			name:         "a client's own Connection: upgrade in-process",
			frontBlocks:  []string{parityInProcessBlock},
			difference:   parityDifference{key: "upstream[0].header.Connection", nginx: "keep-alive | upgrade", front: "upgrade"},
			wantId:       "in-process-hop-by-hop",
			wantProposed: true,
		},
		{
			name:        "a Connection header a forwarder dropped",
			frontBlocks: []string{"g1"},
			difference:  parityDifference{key: "upstream[0].header.Connection", nginx: "keep-alive", front: parityAbsent},
			wantUnnamed: true,
		},
		{
			name:       "the Content-Length: 0 of nginx's preflight",
			front:      [][2]string{{"response[0].status", "204"}},
			difference: parityDifference{key: "response[0].header.Content-Length", nginx: "0", front: parityAbsent},
			wantId:     "preflight-content-length",
		},
		{
			name:        "a Content-Length the front dropped from a 200",
			front:       [][2]string{{"response[0].status", "200"}},
			difference:  parityDifference{key: "response[0].header.Content-Length", nginx: "0", front: parityAbsent},
			wantUnnamed: true,
		},
		{
			name:       "gzip bytes of the same body",
			nginx:      [][2]string{{"response[0].header.Content-Encoding", "gzip"}, {"response[0].body.decoded", `"x"`}},
			front:      [][2]string{{"response[0].header.Content-Encoding", "gzip"}, {"response[0].body.decoded", `"x"`}},
			difference: parityDifference{key: "response[0].body", nginx: `"a"`, front: `"b"`},
			wantId:     "gzip-bytes",
		},
		{
			name:        "gzip bytes of another body",
			nginx:       [][2]string{{"response[0].header.Content-Encoding", "gzip"}, {"response[0].body.decoded", `"x"`}},
			front:       [][2]string{{"response[0].header.Content-Encoding", "gzip"}, {"response[0].body.decoded", `"y"`}},
			difference:  parityDifference{key: "response[0].body", nginx: `"a"`, front: `"b"`},
			wantUnnamed: true,
		},
		{
			name:         "nginx's Connection: keep-alive",
			front:        [][2]string{{"response[0].proto", "HTTP/1.1"}},
			difference:   parityDifference{key: "response[0].header.Connection", nginx: "keep-alive", front: parityAbsent},
			wantId:       "keepalive-connection-header",
			wantProposed: true,
		},
		{
			name:        "a Connection: close the front left out",
			front:       [][2]string{{"response[0].proto", "HTTP/1.1"}},
			difference:  parityDifference{key: "response[0].header.Connection", nginx: "close", front: parityAbsent},
			wantUnnamed: true,
		},
		{
			name:        "an upstream attempt count that differs",
			difference:  parityDifference{key: "upstream.count", nginx: "2", front: "1"},
			wantUnnamed: true,
		},
		{
			name:       "nginx's always headers on connect's in-process 101",
			parityCase: &parityCase{name: "unit", tags: []string{"in-process-upgrade"}},
			front:      [][2]string{{"response[0].status", "101"}},
			difference: parityDifference{key: "response[0].header.X-Frame-Options", nginx: "SAMEORIGIN", front: parityAbsent},
			wantId:     "in-process-switch-headers",
		},
		{
			name:        "an always header missing from an in-process 200",
			parityCase:  &parityCase{name: "unit", tags: []string{"in-process-upgrade"}},
			front:       [][2]string{{"response[0].status", "200"}},
			difference:  parityDifference{key: "response[0].header.X-Frame-Options", nginx: "SAMEORIGIN", front: parityAbsent},
			wantUnnamed: true,
		},
		{
			name:        "another frame option on an in-process 101",
			parityCase:  &parityCase{name: "unit", tags: []string{"in-process-upgrade"}},
			front:       [][2]string{{"response[0].status", "101"}},
			difference:  parityDifference{key: "response[0].header.X-Frame-Options", nginx: "SAMEORIGIN", front: "DENY"},
			wantUnnamed: true,
		},
		{
			name:        "an always header missing from a forwarded 101",
			front:       [][2]string{{"response[0].status", "101"}},
			difference:  parityDifference{key: "response[0].header.X-Frame-Options", nginx: "SAMEORIGIN", front: parityAbsent},
			wantUnnamed: true,
		},
		{
			name:       "http/1.1 where nginx offers a connect name h2",
			parityCase: &parityCase{name: "unit", tags: []string{"connect-h2"}},
			difference: parityDifference{key: "conn[0].tls.alpn", nginx: "h2", front: "http/1.1"},
			wantId:     "connect-alpn",
		},
		{
			name:       "the HTTP/1.1 response that follows from it",
			parityCase: &parityCase{name: "unit", tags: []string{"connect-h2"}},
			difference: parityDifference{key: "response[0].proto", nginx: "HTTP/2.0", front: "HTTP/1.1"},
			wantId:     "connect-alpn",
		},
		{
			name:        "no ALPN for a connect name",
			parityCase:  &parityCase{name: "unit", tags: []string{"connect-h2"}},
			difference:  parityDifference{key: "conn[0].tls.alpn", nginx: "h2", front: ""},
			wantUnnamed: true,
		},
		{
			name:        "http/1.1 for a name that is not connect's",
			difference:  parityDifference{key: "conn[0].tls.alpn", nginx: "h2", front: "http/1.1"},
			wantUnnamed: true,
		},
		{
			name:       "the Trailer request header the front drops",
			difference: parityDifference{key: "upstream[0].header.Trailer", nginx: "X-Parity-Trailer", front: parityAbsent},
			wantId:     "trailer-request-header",
		},
		{
			name:        "a Trailer request header the front changed",
			difference:  parityDifference{key: "upstream[0].header.Trailer", nginx: "X-Parity-Trailer", front: "X-Other"},
			wantUnnamed: true,
		},
		{
			name:        "a Trailer response header the front drops",
			difference:  parityDifference{key: "response[0].header.Trailer", nginx: "X-Parity-Trailer", front: parityAbsent},
			wantUnnamed: true,
		},
		{
			name:       "the upstream's answer to a head nginx's packed buffers refuse",
			parityCase: &parityCase{name: "unit", tags: []string{"header-packing"}},
			requests:   []*parityRequest{packedHead},
			nginx:      [][2]string{{"response[0].status", "400"}},
			front:      forwardedPackedHead(1, false),
			difference: parityDifference{key: "response[0].status", nginx: "400", front: "200"},
			wantId:     "header-buffer-packing",
		},
		{
			name:       "the front's one attempt for it, every header line as sent",
			parityCase: &parityCase{name: "unit", tags: []string{"header-packing"}},
			requests:   []*parityRequest{packedHead},
			nginx:      [][2]string{{"response[0].status", "400"}},
			front:      forwardedPackedHead(1, false),
			difference: parityDifference{key: "upstream[0].header.X-Parity-Fill-3", nginx: parityAbsent, front: packedHead.headers[3][1]},
			wantId:     "header-buffer-packing",
		},
		{
			name:        "a packed head the front forwarded with a line cut short",
			parityCase:  &parityCase{name: "unit", tags: []string{"header-packing"}},
			requests:    []*parityRequest{packedHead},
			nginx:       [][2]string{{"response[0].status", "400"}},
			front:       forwardedPackedHead(1, true),
			difference:  parityDifference{key: "upstream[0].header.X-Parity-Fill-3", nginx: parityAbsent, front: "<cut>"},
			wantUnnamed: true,
		},
		{
			name:        "a packed head the front sent upstream twice",
			parityCase:  &parityCase{name: "unit", tags: []string{"header-packing"}},
			requests:    []*parityRequest{packedHead},
			nginx:       [][2]string{{"response[0].status", "400"}},
			front:       forwardedPackedHead(2, false),
			difference:  parityDifference{key: "upstream.count", nginx: "0", front: "2"},
			wantUnnamed: true,
		},
		{
			name:        "a head refused in a case not written for nginx's packing",
			requests:    []*parityRequest{packedHead},
			nginx:       [][2]string{{"response[0].status", "400"}},
			front:       forwardedPackedHead(1, false),
			difference:  parityDifference{key: "response[0].status", nginx: "400", front: "200"},
			wantUnnamed: true,
		},
		{
			name:         "Go's HTTP/1.0 status line for an HTTP/1.0 request",
			requests:     []*parityRequest{http10},
			difference:   parityDifference{key: "response[0].proto", nginx: "HTTP/1.1", front: "HTTP/1.0"},
			wantId:       "http10-status-line",
			wantProposed: true,
		},
		{
			name:        "an HTTP/1.0 status line for an HTTP/1.1 request",
			requests:    []*parityRequest{apiGet},
			difference:  parityDifference{key: "response[0].proto", nginx: "HTTP/1.1", front: "HTTP/1.0"},
			wantUnnamed: true,
		},
		{
			name:         "Go's AES-128-GCM where OpenSSL prefers AES-256-GCM, in TLS 1.3",
			parityCase:   &parityCase{name: "unit", tags: []string{"tls-suites"}},
			difference:   parityDifference{key: "conn[0].tls.cipher", nginx: "TLS_AES_256_GCM_SHA384", front: "TLS_AES_128_GCM_SHA256"},
			wantId:       "tls-suite-choice",
			wantProposed: true,
		},
		{
			name:         "the same choice in TLS 1.2",
			parityCase:   &parityCase{name: "unit", tags: []string{"tls-suites"}},
			difference:   parityDifference{key: "conn[0].tls.cipher", nginx: "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384", front: "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256"},
			wantId:       "tls-suite-choice",
			wantProposed: true,
		},
		{
			name:        "a TLS 1.2 suite outside nginx's EECDH+AESGCM",
			parityCase:  &parityCase{name: "unit", tags: []string{"tls-suites"}},
			difference:  parityDifference{key: "conn[0].tls.cipher", nginx: "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384", front: "TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256"},
			wantUnnamed: true,
		},
		{
			name:        "Go's suite choice in a case not written for it",
			difference:  parityDifference{key: "conn[0].tls.cipher", nginx: "TLS_AES_256_GCM_SHA384", front: "TLS_AES_128_GCM_SHA256"},
			wantUnnamed: true,
		},
		{
			name:         "the default certificate for an SNI over 255 bytes, routed by Host",
			parityCase:   &parityCase{name: "unit", tags: []string{"long-sni"}},
			requests:     []*parityRequest{longSni},
			nginx:        longSniRefused,
			front:        longSniRouted(parityLbName, parityApiName),
			difference:   parityDifference{key: "response[0].status", nginx: parityAbsent, front: "200"},
			wantId:       "long-sni-default-certificate",
			wantProposed: true,
		},
		{
			name:         "the upstream attempt it makes",
			parityCase:   &parityCase{name: "unit", tags: []string{"long-sni"}},
			requests:     []*parityRequest{longSni},
			nginx:        longSniRefused,
			front:        longSniRouted(parityLbName, parityApiName),
			difference:   parityDifference{key: "upstream[0].host", nginx: parityAbsent, front: parityApiName},
			wantId:       "long-sni-default-certificate",
			wantProposed: true,
		},
		{
			name:        "a long SNI the front answered with another certificate",
			parityCase:  &parityCase{name: "unit", tags: []string{"long-sni"}},
			requests:    []*parityRequest{longSni},
			nginx:       longSniRefused,
			front:       longSniRouted(parityApiName, parityApiName),
			difference:  parityDifference{key: "response[0].status", nginx: parityAbsent, front: "200"},
			wantUnnamed: true,
		},
		{
			name:        "a long SNI request that reached an upstream under another Host",
			parityCase:  &parityCase{name: "unit", tags: []string{"long-sni"}},
			requests:    []*parityRequest{longSni},
			nginx:       longSniRefused,
			front:       longSniRouted(parityLbName, parityWebName),
			difference:  parityDifference{key: "upstream[0].host", nginx: parityAbsent, front: parityWebName},
			wantUnnamed: true,
		},
		{
			name:         "the front's answer to a chunked body beside a Content-Length, which nginx refuses",
			parityCase:   &parityCase{name: "unit", tags: []string{"content-length-beside-chunked"}},
			requests:     []*parityRequest{chunkedBesideLength},
			nginx:        [][2]string{{"response[0].status", "400"}},
			front:        forwarded(reframed),
			difference:   parityDifference{key: "response[0].status", nginx: "400", front: "200"},
			wantId:       "content-length-beside-chunked",
			wantProposed: true,
		},
		{
			name:         "the body it forwarded, re-framed by its length",
			parityCase:   &parityCase{name: "unit", tags: []string{"content-length-beside-chunked"}},
			requests:     []*parityRequest{chunkedBesideLength},
			nginx:        [][2]string{{"response[0].status", "400"}},
			front:        forwarded(reframed),
			difference:   parityDifference{key: "upstream[0].body", nginx: parityAbsent, front: `"hello"`},
			wantId:       "content-length-beside-chunked",
			wantProposed: true,
		},
		{
			name:        "a chunked body forwarded as its raw chunks",
			parityCase:  &parityCase{name: "unit", tags: []string{"content-length-beside-chunked"}},
			requests:    []*parityRequest{chunkedBesideLength},
			nginx:       [][2]string{{"response[0].status", "400"}},
			front:       forwarded(forwardedAttempt{sequence: 0, method: http.MethodPost, uri: "/upload", body: "5\r\nhello\r\n0\r\n\r\n", framing: "length 15"}),
			difference:  parityDifference{key: "upstream[0].body", nginx: parityAbsent, front: `"5\r\nhello\r\n0\r\n\r\n"`},
			wantUnnamed: true,
		},
		{
			name:        "a chunked body framed by the Content-Length beside it",
			parityCase:  &parityCase{name: "unit", tags: []string{"content-length-beside-chunked"}},
			requests:    []*parityRequest{chunkedBesideLength},
			nginx:       [][2]string{{"response[0].status", "400"}},
			front:       forwarded(forwardedAttempt{sequence: 0, method: http.MethodPost, uri: "/upload", body: "5\r\nhe", framing: "length 5"}),
			difference:  parityDifference{key: "response[0].status", nginx: "400", front: "200"},
			wantUnnamed: true,
		},
		{
			name:        "a chunked body forwarded still chunked",
			parityCase:  &parityCase{name: "unit", tags: []string{"content-length-beside-chunked"}},
			requests:    []*parityRequest{chunkedBesideLength},
			nginx:       [][2]string{{"response[0].status", "400"}},
			front:       forwarded(forwardedAttempt{sequence: 0, method: http.MethodPost, uri: "/upload", body: "hello", framing: "chunked"}),
			difference:  parityDifference{key: "upstream[0].framing", nginx: parityAbsent, front: "chunked"},
			wantUnnamed: true,
		},
		{
			name:        "a second upstream request out of the one the client sent",
			parityCase:  &parityCase{name: "unit", tags: []string{"content-length-beside-chunked"}},
			requests:    []*parityRequest{chunkedBesideLength},
			nginx:       [][2]string{{"response[0].status", "400"}},
			front:       forwarded(reframed, reframed),
			difference:  parityDifference{key: "upstream.count", nginx: "0", front: "2"},
			wantUnnamed: true,
		},
		{
			name:         "the next request on the connection, reaching the upstream as its own",
			parityCase:   &parityCase{name: "unit", tags: []string{"content-length-beside-chunked"}},
			requests:     []*parityRequest{&chunkedBesideLengthKeptAlive, secondRequest},
			nginx:        [][2]string{{"response[0].status", "400"}, {"response[1].error", "eof"}},
			front:        append(forwarded(reframed, followed), [2]string{"response[1].status", "200"}),
			difference:   parityDifference{key: "response[1].error", nginx: "eof", front: parityAbsent},
			wantId:       "content-length-beside-chunked",
			wantProposed: true,
		},
		{
			name:        "a next request the upstream never saw as its own",
			parityCase:  &parityCase{name: "unit", tags: []string{"content-length-beside-chunked"}},
			requests:    []*parityRequest{&chunkedBesideLengthKeptAlive, secondRequest},
			nginx:       [][2]string{{"response[0].status", "400"}, {"response[1].error", "eof"}},
			front:       append(forwarded(reframed), [2]string{"response[1].status", "400"}),
			difference:  parityDifference{key: "response[1].status", nginx: parityAbsent, front: "400"},
			wantUnnamed: true,
		},
		{
			name:        "a body beside a Content-Length in a case not written for it",
			requests:    []*parityRequest{chunkedBesideLength},
			nginx:       [][2]string{{"response[0].status", "400"}},
			front:       forwarded(reframed),
			difference:  parityDifference{key: "response[0].status", nginx: "400", front: "200"},
			wantUnnamed: true,
		},
		{
			name:       "Go's 431 for a head past LBALT-IMPL §7's band",
			parityCase: &parityCase{name: "unit", tags: []string{"past-max-header-bytes"}},
			requests:   []*parityRequest{filledHead(21)},
			nginx:      ownPage("400", "proxy"),
			front:      ownPage("431", "proxy"),
			difference: parityDifference{key: "response[0].status", nginx: "400", front: "431"},
			wantId:     "max-header-bytes-431",
		},
		{
			name:        "Go's 431 for a head inside LBALT-IMPL §7's band, which the front answers itself",
			parityCase:  &parityCase{name: "unit", tags: []string{"past-max-header-bytes"}},
			requests:    []*parityRequest{filledHead(11)},
			nginx:       ownPage("400", "proxy"),
			front:       ownPage("431", "proxy"),
			difference:  parityDifference{key: "response[0].status", nginx: "400", front: "431"},
			wantUnnamed: true,
		},
		{
			name:        "a head past the band that the front forwarded",
			parityCase:  &parityCase{name: "unit", tags: []string{"past-max-header-bytes"}},
			requests:    []*parityRequest{filledHead(21)},
			nginx:       ownPage("400", "proxy"),
			front:       ownPage("200", "upstream"),
			difference:  parityDifference{key: "response[0].status", nginx: "400", front: "200"},
			wantUnnamed: true,
		},
		{
			name:        "Go's 431 for a head past the band, in a case not written for it",
			requests:    []*parityRequest{filledHead(21)},
			nginx:       ownPage("400", "proxy"),
			front:       ownPage("431", "proxy"),
			difference:  parityDifference{key: "response[0].status", nginx: "400", front: "431"},
			wantUnnamed: true,
		},
	}
	for _, c := range cases {
		checkCase := c.parityCase
		if checkCase == nil {
			checkCase = &parityCase{name: "unit"}
		}
		check := &parityCheck{
			parityCase: checkCase,
			nginx:      newParityTestObservation(c.nginx),
			front:      newParityTestObservation(c.front, c.frontBlocks...),
		}
		for sequence, request := range c.requests {
			check.nginx.sequenceRequests[sequence] = request
			check.front.sequenceRequests[sequence] = request
		}
		difference := c.difference
		acceptance := check.acceptance(&difference)
		switch {
		case c.wantUnnamed:
			if acceptance != nil {
				t.Errorf("%s: named by %s, want by none", c.name, acceptance.id)
			}
		case acceptance == nil:
			t.Errorf("%s: named by none, want %s", c.name, c.wantId)
		case acceptance.id != c.wantId || acceptance.proposed != c.wantProposed:
			t.Errorf("%s: named by %s (proposed %v), want %s (proposed %v)", c.name, acceptance.id, acceptance.proposed, c.wantId, c.wantProposed)
		}
	}
}

// A statuses scenario numbers the attempts of each request of a case on
// their own, so every request's first attempt meets the first status: each
// ring client of a retry walk is answered 503 and walks on.
func TestParityStatusesScenarioNumbersEachRequestsAttempts(t *testing.T) {
	recorder := newParityRecorder(defaultParityHarnessSettings())
	recorder.setScenario("balance/unit", parityStatusesScenario(http.StatusServiceUnavailable, http.StatusOK))
	handler := recorder.handler("mcp", "g1")
	statuses := []int{}
	for _, sequence := range []int{0, 0, 1, 1, 2, 2} {
		request := httptest.NewRequest(http.MethodGet, "/ring", nil)
		request.Header.Set(parityCaseHeader, "front/balance/unit")
		request.Header.Set(paritySequenceHeader, strconv.Itoa(sequence))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		statuses = append(statuses, response.Code)
	}
	want := []int{503, 200, 503, 200, 503, 200}
	if !slices.Equal(statuses, want) {
		t.Errorf("statuses = %v, want %v", statuses, want)
	}
	attempts := recorder.attempts("front/balance/unit")
	if len(attempts) != len(want) {
		t.Fatalf("recorded %d attempts, want %d", len(attempts), len(want))
	}
	for i, attempt := range attempts {
		if attempt.status != want[i] {
			t.Errorf("attempt %d recorded status %d, want %d", i, attempt.status, want[i])
		}
	}
}

// Each timeout case straddles the timeout it is for, at the values nginx's
// config sets, as the front's defaults and the fixture's route file carry
// them: what should pass waits under its timeout, and what should end the
// exchange waits past it.
func TestParityTimeoutCasesStraddleTheirTimeouts(t *testing.T) {
	settings := defaultParityHarnessSettings()
	frontSettings := nginx.DefaultFrontSettings()

	clientHeaderTimeout := frontSettings.Http.ClientHeaderTimeout
	if !(settings.HeaderPause < clientHeaderTimeout && clientHeaderTimeout < settings.HeaderStall) {
		t.Errorf("the header pause (%s) and stall (%s) don't straddle client_header_timeout (%s)", settings.HeaderPause, settings.HeaderStall, clientHeaderTimeout)
	}

	clientBodyTimeout := frontSettings.Body.ClientBodyTimeout
	if clientBodyTimeout <= settings.ProgressingUploadPause {
		t.Errorf("the progressing upload's pause (%s) is not under client_body_timeout (%s)", settings.ProgressingUploadPause, clientBodyTimeout)
	}
	routeFile, err := os.ReadFile(filepath.Join(parityFixtureDir, parityRouteFileName))
	if err != nil {
		t.Fatal(err)
	}
	routeTable, err := nginx.ParseRouteTable(routeFile)
	if err != nil {
		t.Fatal(err)
	}
	var artifactTimeout time.Duration
	for _, service := range routeTable.Services {
		for _, routeRequestBody := range service.RouteRequestBodies {
			if service.Name == "api" && routeRequestBody.Path == "/sn/attempt-artifact" {
				artifactTimeout = time.Duration(routeRequestBody.ClientBodyTimeoutSeconds) * time.Second
			}
		}
	}
	if artifactTimeout == 0 {
		t.Fatal("the fixture's api has no /sn/attempt-artifact override")
	}
	if !(clientBodyTimeout < settings.StallWithinOverride && settings.StallWithinOverride < artifactTimeout && artifactTimeout < settings.StallPastOverride) {
		t.Errorf("the stalls within (%s) and past (%s) the artifact override (%s) don't straddle it above client_body_timeout (%s)", settings.StallWithinOverride, settings.StallPastOverride, artifactTimeout, clientBodyTimeout)
	}
}

// The free space of the Docker VM is read from df's one filesystem line.
func TestParseParityDfAvailable(t *testing.T) {
	dfOutput := "Filesystem 1-byte-blocks Used Available Capacity Mounted on\noverlay 269490393088 194348990464 61335707648 77% /\n"
	available, err := parseParityDfAvailable(dfOutput)
	if err != nil {
		t.Fatal(err)
	}
	if available != 61335707648 {
		t.Errorf("available = %d, want 61335707648", available)
	}
	if _, err := parseParityDfAvailable("Filesystem 1-byte-blocks Used Available Capacity Mounted on\n"); err == nil {
		t.Error("df output with no filesystem line parsed")
	}
}
