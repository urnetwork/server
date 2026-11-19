package nginx_test

// Tests of the parity harness that run without Docker. Its comparison
// (parity_compare_test.go): every changed key is a difference, each
// acceptance names the difference it was written for and refuses its near
// misses, and a difference no entry names is left unaccepted, so the harness
// fails on it. Its recording upstreams (parity_upstream_test.go): a scenario
// meets each request's attempts as that request's own.

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
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
	lbStatus := func(body string) [][2]string {
		return [][2]string{{"response[0].body", body}}
	}
	nginxStatusBody := `"{\"version\":\"0.0.0-parity\",\"status\":\"ok\"}\n"`
	frontStatusBody := `"{\"version\":\"0.0.0-parity\",\"status\":\"ok\",\"front\":\"lbalt\"}\n"`
	// an api request whose head is count header lines of 6600 bytes
	filledHead := func(count int) *parityRequest {
		request := parityGet(parityApiName, parityApiName, "/hello")
		for i := range count {
			request = parityWith(request, "", parityLongHeader("X-Parity-Fill-"+strconv.Itoa(i), 6600))
		}
		return request
	}
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
			nginx:      lbStatus(nginxStatusBody),
			front:      lbStatus(frontStatusBody),
			difference: parityDifference{key: "response[0].body", nginx: nginxStatusBody, front: frontStatusBody},
			wantId:     "lb-status-json",
		},
		{
			name:        "an lb status json with another version",
			parityCase:  &parityCase{name: "unit", tags: []string{"lb-status"}},
			nginx:       lbStatus(nginxStatusBody),
			front:       lbStatus(`"{\"version\":\"0.0.1\",\"status\":\"ok\",\"front\":\"lbalt\"}\n"`),
			difference:  parityDifference{key: "response[0].body", nginx: nginxStatusBody, front: `"{\"version\":\"0.0.1\",\"status\":\"ok\",\"front\":\"lbalt\"}\n"`},
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
