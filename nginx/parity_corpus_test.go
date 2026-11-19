package nginx_test

// The parity harness's corpus (parity_test.go), following LBALT.md §8 step
// 2's coverage list: the hidden per-block routes and redirects; CORS, Vary:
// Origin and the CSP-dependent frame options; streaming; the /status
// denial; routing after TLS; least_conn and nginx's Ketama ring; the body
// timeouts; Route53's check; and header limits, TRACE, underscores,
// oversized upstream heads and gzip. Every case runs the same requests
// against nginx and the front.
//
// The ordinary client comes from an excluded subnet of the fixture's
// interface, so the tight limits there apply only to the limit cases, each
// from an address of its own. The concurrent cases, which wait out
// timeouts, run in the background beside the rest. They come from the
// ordinary clients and stay off pool, whose least_conn choices a case
// compares: api's blocks are compared only on its per-block routes, and
// mcp's ring doesn't depend on load.

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// One case of the corpus.
type parityCase struct {
	name string
	// the one request of a simple case
	request *parityRequest
	// what the upstreams do; nil answers parityDefaultScenario
	scenario parityScenario
	// drives the case itself, in place of request
	run func(run *parityCaseRun)
	// compares the block each attempt reached, which the balancing and
	// per-block cases decide; elsewhere a balancer's state decides it
	compareBlocks bool
	// the tags acceptances match on
	tags []string
	// runs in the background, its two targets at once
	concurrent bool
}

// The fixture's names (warp warpctl/testdata/lbalt_parity_services.yml).
const (
	parityLbName           = "test-lb.service.example"
	parityLbOtherName      = "test-lb.other.example"
	parityApiName          = "test-api.service.example"
	parityApiAliasName     = "api.service.example"
	parityRedirectName     = "old-api.service.example"
	parityConnectName      = "test-connect.service.example"
	parityConnectChildName = "edge.connect.service.example"
	parityMcpName          = "mcp.service.example"
	parityPoolName         = "pool.service.example"
	parityRelayName        = "relay.service.example"
	parityWebName          = "web.service.example"
	parityExactWildName    = "exact.wild.service.example"
	parityWildChildName    = "one.wild.service.example"
	parityWildGrandName    = "two.one.wild.service.example"
	parityWildBaseName     = "wild.service.example"
	parityWorkerName       = "worker.service.example"
	parityUnknownName      = "unknown.example"
	parityLbPrefix         = "/hidden-parity"
	parityRelayPrefix      = "/relay-prefix"
	parityAllowedOrigin    = "https://app.service.example"
	parityOtherOrigin      = "https://elsewhere.example"
)

// The corpus's ordinary clients, in the interface's excluded subnets.
var (
	parityClientIpv4 = netip.MustParseAddr("203.0.113.10")
	parityClientIpv6 = netip.MustParseAddr("2001:db8:c::10")
)

// The clients whose addresses pick a block of mcp, nginx's Ketama ring, from
// the text of each address: IPv4, and IPv6 with the shapes whose text
// ngx_inet6_ntop has to agree on with the front's (a lone zero group, which
// stays written out; two equal zero runs, of which only the first is
// compressed; runs at the end; no zero group at all).
func parityRingClients() []netip.Addr {
	clients := []netip.Addr{}
	for i := range 24 {
		clients = append(clients, netip.AddrFrom4([4]byte{203, 0, 113, byte(100 + i)}))
	}
	for i := range 16 {
		clients = append(clients, netip.MustParseAddr(fmt.Sprintf("2001:db8:c::%x", 0x100+i)))
	}
	for _, shape := range []string{
		"2001:db8:c:0:1:0:0:1",
		"2001:db8:c:1:0:1:0:1",
		"2001:db8:c:0:0:1:0:0",
		"2001:db8:c:1:2:3:4::",
		"2001:db8:c::",
		"2001:db8:c:abcd:ef01:2345:6789:abcd",
	} {
		clients = append(clients, netip.MustParseAddr(shape))
	}
	return clients
}

// The limit cases' clients, outside the excluded subnets, one per case.
func parityLimitClient(i int) netip.Addr {
	return netip.AddrFrom4([4]byte{198, 51, 100, byte(200 + i)})
}

// The IPv6 limit case's client.
var parityLimitClientIpv6 = netip.MustParseAddr("2001:db8:d::1")

// The number of limit clients parityLimitClient hands out.
const parityLimitClientCount = 24

// The fixture's eth1 limits as requests: a burst of 9 at 1 r/min admits ten
// requests at once, /connect/control's burst of 3 admits four, and three may
// be in flight.
const (
	paritySharedAdmitted       = 10
	parityRouteAdmitted        = 4
	parityConnectionLimitCount = 3
)

// A GET over TLS with the given server name and Host.
func parityGet(sni string, host string, target string) *parityRequest {
	return &parityRequest{
		scheme: "https",
		sni:    sni,
		method: http.MethodGet,
		target: target,
		host:   host,
	}
}

// A GET on port 80 with the given Host.
func parityPlainGet(host string, target string) *parityRequest {
	return &parityRequest{
		scheme: "http",
		method: http.MethodGet,
		target: target,
		host:   host,
	}
}

// A request with its method and header lines changed.
func parityWith(request *parityRequest, method string, headers ...[2]string) *parityRequest {
	changed := *request
	if method != "" {
		changed.method = method
	}
	changed.headers = append(append([][2]string{}, request.headers...), headers...)
	return &changed
}

// A request sent over h2.
func parityOverH2(request *parityRequest) *parityRequest {
	changed := *request
	changed.alpn = []string{"h2", "http/1.1"}
	changed.http2 = true
	return &changed
}

// A request from another client address.
func parityFrom(request *parityRequest, clientAddr netip.Addr) *parityRequest {
	changed := *request
	changed.clientAddr = clientAddr
	return &changed
}

// A request with a body.
func parityWithBody(request *parityRequest, method string, body []byte, chunked bool) *parityRequest {
	changed := *request
	changed.method = method
	changed.body = body
	changed.chunked = chunked
	return &changed
}

// Sends the same request count times, one after another, and records each.
func paritySendRepeated(request *parityRequest, count int) func(run *parityCaseRun) {
	return func(run *parityCaseRun) {
		for range count {
			run.send(request)
		}
	}
}

// Sends one h2 request with exactly the given fields, through a raw framer.
func paritySendRawH2(sni string, fields [][2]string) func(run *parityCaseRun) {
	return func(run *parityCaseRun) {
		request := parityOverH2(parityGet(sni, sni, "/"))
		sequence := run.sequence()
		conn, err := run.dial(request, sequence)
		if err != nil {
			return
		}
		defer conn.close()
		if conn.negotiatedProtocol() != "h2" {
			run.fact("h2", "not negotiated")
			return
		}
		// a copy per run, so one target's case fields never reach the other's
		requestFields := append(slices.Clone(fields), [2]string{strings.ToLower(parityCaseHeader), run.caseKey}, [2]string{strings.ToLower(paritySequenceHeader), strconv.Itoa(sequence)})
		response, err := conn.rawH2(requestFields)
		run.recordResponse(sequence, response, err)
	}
}

// Reads a streamed response's first line while the upstream holds the
// rest, fires "got-first", and records the response with its whole body.
func paritySendStreamedResponse(request *parityRequest) func(run *parityCaseRun) {
	return func(run *parityCaseRun) {
		sequence := run.sequence()
		conn, err := run.dial(request, sequence)
		if err != nil {
			return
		}
		defer conn.close()
		var response *http.Response
		if request.http2 && conn.negotiatedProtocol() == "h2" {
			response, err = conn.roundTripH2(request, sequence)
		} else {
			conn.writeH1(request, sequence)
			response, err = conn.readH1Response(request.method)
		}
		if err != nil {
			run.recordResponse(sequence, nil, err)
			return
		}
		defer response.Body.Close()
		reader := bufio.NewReader(response.Body)
		first, firstErr := reader.ReadString('\n')
		if firstErr == nil {
			run.fire("got-first")
		}
		rest, _ := io.ReadAll(reader)
		run.recordResponseHead(sequence, response)
		run.observation.set(fmt.Sprintf("response[%d].body", sequence), parityValueOfBytes([]byte(first+string(rest))))
	}
}

// Sends a body's first bytes, waits for the upstream to have them, and
// then the rest, recording whether they reached the upstream first: they
// do only where the body streams.
func paritySendStreamedRequest(request *parityRequest) func(run *parityCaseRun) {
	return func(run *parityCaseRun) {
		sequence := run.sequence()
		conn, err := run.dial(request, sequence)
		if err != nil {
			return
		}
		defer conn.close()
		body := request.body
		first, rest := body[:parityStreamFirstBytes], body[parityStreamFirstBytes:]
		chunk := func(b []byte) []byte {
			if !request.chunked {
				return b
			}
			return append(fmt.Appendf(nil, "%x\r\n", len(b)), append(b, '\r', '\n')...)
		}
		head := conn.h1Head(request, sequence)
		if _, err := conn.conn.Write(append(head, chunk(first)...)); err != nil {
			run.recordResponse(sequence, nil, err)
			return
		}
		reached := run.wait("upstream-got-first", run.harness.settings.BarrierTimeout)
		run.fact("first bytes reached the upstream before the rest was sent", strconv.FormatBool(reached))
		restMessage := chunk(rest)
		if request.chunked {
			restMessage = append(restMessage, "0\r\n\r\n"...)
		}
		conn.conn.Write(restMessage)
		response, err := conn.readH1Response(request.method)
		run.recordResponse(sequence, response, err)
	}
}

// Writes a body in pieces with pause between them, and records the response,
// read from the moment the head is sent: nginx's client_body_timeout bounds
// each pause, not the whole, and a server that answers or closes during a
// pause ends the writing then, so what it sent is read before any reset a
// later write would cause.
func paritySendPacedBody(request *parityRequest, pieceCount int, pause time.Duration) func(run *parityCaseRun) {
	return func(run *parityCaseRun) {
		sequence := run.sequence()
		conn, err := run.dial(request, sequence)
		if err != nil {
			return
		}
		defer conn.close()
		conn.conn.SetDeadline(time.Now().Add(time.Duration(pieceCount)*pause + run.harness.settings.ExchangeTimeout))
		if _, err := conn.conn.Write(conn.h1Head(request, sequence)); err != nil {
			run.recordResponse(sequence, nil, err)
			return
		}
		type responseResult struct {
			response *http.Response
			err      error
		}
		responseResults := make(chan responseResult, 1)
		go func() {
			response, err := http.ReadResponse(conn.reader, &http.Request{Method: request.method})
			responseResults <- responseResult{response: response, err: err}
		}()
		pieceBytes := len(request.body) / pieceCount
		piecesWritten := 0
		var result *responseResult
	writing:
		for i := range pieceCount {
			if 0 < i {
				select {
				case early := <-responseResults:
					result = &early
					break writing
				case <-time.After(pause):
				}
			}
			piece := request.body[i*pieceBytes : (i+1)*pieceBytes]
			if i == pieceCount-1 {
				piece = request.body[i*pieceBytes:]
			}
			if _, err := conn.conn.Write(piece); err != nil {
				break
			}
			piecesWritten += 1
		}
		run.fact("pieces written", fmt.Sprintf("%d of %d", piecesWritten, pieceCount))
		if result == nil {
			late := <-responseResults
			result = &late
		}
		run.recordResponse(sequence, result.response, result.err)
	}
}

// Writes a head in two parts and watches the server for the pause between
// them: a server that answers or closes in that time ends the case there,
// as a fact records. Without a rest that is all the case does; with one, the
// rest goes out and the response is recorded. "{case}" and "{sequence}" in
// the parts are the case key and the request's sequence.
func paritySendHeadInParts(request *parityRequest, partial string, pause time.Duration, rest string) func(run *parityCaseRun) {
	return func(run *parityCaseRun) {
		sequence := run.sequence()
		conn, err := run.dial(request, sequence)
		if err != nil {
			return
		}
		defer conn.close()
		replacer := strings.NewReplacer("{case}", run.caseKey, "{sequence}", strconv.Itoa(sequence))
		if _, err := io.WriteString(conn.conn, replacer.Replace(partial)); err != nil {
			run.fact("partial write", parityErrorClass(err))
			return
		}
		conn.conn.SetReadDeadline(time.Now().Add(pause))
		line, err := conn.reader.ReadString('\n')
		switch {
		case err == nil:
			run.fact("the server sent", strings.TrimSpace(line))
			return
		case !errors.Is(err, os.ErrDeadlineExceeded):
			run.fact("the server sent nothing, then", parityErrorClass(err))
			return
		}
		run.fact("the server waited out the pause", "true")
		if rest == "" {
			return
		}
		if _, err := io.WriteString(conn.conn, replacer.Replace(rest)); err != nil {
			run.fact("rest write", parityErrorClass(err))
			return
		}
		response, err := conn.readH1Response(request.method)
		run.recordResponse(sequence, response, err)
	}
}

// Upgrades to a websocket, then sends a line and records its echo.
func paritySendWebsocket(request *parityRequest) func(run *parityCaseRun) {
	return func(run *parityCaseRun) {
		sequence := run.sequence()
		conn, err := run.dial(request, sequence)
		if err != nil {
			return
		}
		defer conn.close()
		conn.writeH1(request, sequence)
		response, err := conn.readH1Response(request.method)
		if err != nil {
			run.recordResponse(sequence, nil, err)
			return
		}
		run.recordResponseHead(sequence, response)
		if response.StatusCode != http.StatusSwitchingProtocols {
			body, _ := io.ReadAll(response.Body)
			run.observation.set(fmt.Sprintf("response[%d].body", sequence), parityValueOfBytes(body))
			return
		}
		io.WriteString(conn.conn, "ping\n")
		echo, err := conn.reader.ReadString('\n')
		if err != nil {
			run.fact("echo", parityErrorClass(err))
			return
		}
		run.fact("echo", strconv.Quote(echo))
	}
}

// Holds count requests open at the upstream, each sent once the one before
// it arrived; the case's "release" barrier lets them all go, and done ends
// when their responses are recorded.
func parityHold(run *parityCaseRun, request *parityRequest, count int, done *sync.WaitGroup) {
	for range count {
		sequence := run.sequence()
		done.Add(1)
		go func() {
			defer done.Done()
			run.sendSequence(request, sequence)
		}()
		arrived := run.wait("arrived/"+strconv.Itoa(sequence), run.harness.settings.BarrierTimeout)
		run.fact("held request "+strconv.Itoa(sequence)+" arrived", strconv.FormatBool(arrived))
	}
}

// Sends one request from each ring client, so each one's block is
// compared.
func parityRing(request *parityRequest, clients []netip.Addr) func(run *parityCaseRun) {
	return func(run *parityCaseRun) {
		for _, client := range clients {
			run.send(parityFrom(request, client))
		}
	}
}

// Two requests on one h1 connection, the first keeping it open.
func paritySendKeepalive(first *parityRequest, second *parityRequest) func(run *parityCaseRun) {
	return func(run *parityCaseRun) {
		sequence := run.sequence()
		conn, err := run.dial(first, sequence)
		if err != nil {
			return
		}
		defer conn.close()
		conn.writeH1(first, sequence)
		response, err := conn.readH1Response(first.method)
		run.recordResponse(sequence, response, err)
		if err != nil {
			return
		}
		<-conn.writeDone
		sequence = run.sequence()
		conn.writeH1(second, sequence)
		response, err = conn.readH1Response(second.method)
		run.recordResponse(sequence, response, err)
	}
}

// A body of n bytes of a repeating pattern.
func parityBody(n int) []byte {
	body := make([]byte, n)
	for i := range body {
		body[i] = 'a' + byte(i%26)
	}
	return body
}

// A header line n bytes long, name and value.
func parityLongHeader(name string, n int) [2]string {
	return [2]string{name, strings.Repeat("v", n-len(name)-len(": ")-len("\r\n"))}
}

// The corpus, in the order it runs.
func parityCorpus() []*parityCase {
	settings := defaultParityHarnessSettings()
	api := parityGet(parityApiName, parityApiName, "/hello?x=1")
	html := "<html><body>" + strings.Repeat("parity ", 40) + "</body></html>\n"
	htmlScenario := func(status int, extra ...[2]string) parityScenario {
		return parityRespondScenario(status, append([][2]string{{"Content-Type", "text/html; charset=utf-8"}}, extra...), html)
	}
	cspScenario := func(policies ...string) parityScenario {
		headerLines := [][2]string{{"Content-Type", "text/plain; charset=utf-8"}}
		for _, policy := range policies {
			headerLines = append(headerLines, [2]string{"Content-Security-Policy", policy})
		}
		return parityRespondScenario(http.StatusOK, headerLines, "csp\n")
	}
	websocketHeaders := [][2]string{
		{"Upgrade", "websocket"},
		{"Connection", "Upgrade"},
		{"Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ=="},
		{"Sec-WebSocket-Version", "13"},
	}
	spoofedHeaders := [][2]string{
		{"X-UR-Forwarded-For", "192.0.2.99:1234"},
		{"X-Forwarded-For", "192.0.2.98"},
		{"X-Forwarded-Source-Port", "4321"},
		{"X-UR-Request-Buffering", "off"},
		{"Early-Data", "1"},
	}
	limitIndex := 0
	nextLimitClient := func() netip.Addr {
		if parityLimitClientCount <= limitIndex {
			panic("the corpus needs more limit clients than parityLimitClientCount")
		}
		client := parityLimitClient(limitIndex)
		limitIndex += 1
		return client
	}
	// count short header lines
	shortHeaderLines := func(count int) [][2]string {
		lines := [][2]string{}
		for i := range count {
			lines = append(lines, [2]string{"X-Parity-Line-" + strconv.Itoa(i), "v"})
		}
		return lines
	}
	// the lines an h1 GET carries besides its own: Host, the case's two,
	// User-Agent and Connection
	const parityGetOwnHeaderLines = 5
	headLines := func(count int) [][2]string {
		lines := [][2]string{}
		for i := range count {
			lines = append(lines, parityLongHeader("X-Parity-Fill-"+strconv.Itoa(i), 6600))
		}
		return lines
	}
	// the first lines of a head, without its end
	partialHead := "GET /hello HTTP/1.1\r\nHost: " + parityApiName + "\r\n" + parityCaseHeader + ": {case}\r\n" + paritySequenceHeader + ": {sequence}\r\n"
	// a chunked body beside a Content-Length, which nginx refuses
	contentLengthAndChunked := parityWith(parityWithBody(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, []byte("hello"), true), "", [2]string{"Content-Length", "5"}, [2]string{"Transfer-Encoding", "chunked"})
	contentLengthAndChunkedKeptAlive := *contentLengthAndChunked
	contentLengthAndChunkedKeptAlive.keepalive = true

	return []*parityCase{
		// timeouts, in the background: nginx's client_body_timeout is 15 s,
		// the artifact route's override 20 s, client_header_timeout 15 s, and
		// the proxy timeouts 30 s, or 60 s on streamed routes
		{
			name:       "timeout/upload-progressing-past-the-idle-timeout",
			run:        paritySendPacedBody(parityWithBody(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, parityBody(4000), false), settings.ProgressingUploadPieces, settings.ProgressingUploadPause),
			concurrent: true,
		},
		// streamed as it arrives, so the proxy timeouts run during it: a
		// whole-request bound, or a read timeout armed before the body is
		// sent, ends it early
		{
			name:       "timeout/streamed-upload-progressing-past-the-idle-timeout",
			run:        paritySendPacedBody(parityWithBody(parityGet(parityMcpName, parityMcpName, "/mcp"), http.MethodPost, parityBody(8000), false), settings.StreamedUploadPieces, settings.StreamedUploadPause),
			concurrent: true,
		},
		{
			name:       "timeout/artifact-stall-within-its-override",
			run:        paritySendPacedBody(parityWithBody(parityGet(parityApiName, parityApiName, "/sn/attempt-artifact"), http.MethodPost, parityBody(2000), false), 2, settings.StallWithinOverride),
			concurrent: true,
		},
		{
			name:       "timeout/artifact-stall-past-its-override",
			run:        paritySendPacedBody(parityWithBody(parityGet(parityApiName, parityApiName, "/sn/attempt-artifact"), http.MethodPost, parityBody(2000), false), 2, settings.StallPastOverride),
			concurrent: true,
		},
		{
			name:       "timeout/default-route-stall-within-the-override",
			run:        paritySendPacedBody(parityWithBody(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, parityBody(2000), false), 2, settings.StallWithinOverride),
			concurrent: true,
		},
		{
			name:       "timeout/lb-artifact-stall-within-its-override",
			run:        paritySendPacedBody(parityWithBody(parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/service/api/sn/attempt-artifact"), http.MethodPost, parityBody(2000), false), 2, settings.StallWithinOverride),
			concurrent: true,
		},
		{
			name:       "timeout/header-stall",
			run:        paritySendHeadInParts(parityGet(parityApiName, parityApiName, "/"), partialHead, settings.HeaderStall, ""),
			concurrent: true,
		},
		{
			name:       "timeout/header-pause-within-the-timeout",
			run:        paritySendHeadInParts(parityGet(parityApiName, parityApiName, "/"), partialHead, settings.HeaderPause, "User-Agent: parity\r\nConnection: close\r\n\r\n"),
			concurrent: true,
		},
		{
			name:       "stream/request-buffered-on-api",
			run:        paritySendStreamedRequest(parityWithBody(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, parityBody(100), true)),
			scenario:   parityStreamRequestScenario,
			concurrent: true,
		},

		// routing after tls (D9): the certificate, the upstream, the
		// forwarded Host and the status routes
		{name: "tls/no-sni-known-host", request: parityGet("", parityApiName, "/hello?x=1")},
		{name: "tls/expose-alias-name", request: parityGet(parityApiAliasName, parityApiAliasName, "/hello")},
		{name: "tls/sni-web-host-api", request: parityGet(parityWebName, parityApiName, "/hello")},
		{name: "tls/sni-lb-host-api", request: parityGet(parityLbName, parityApiName, "/hello")},
		{name: "tls/sni-api-host-lb-status", request: parityGet(parityApiName, parityLbName, parityLbPrefix+"/status"), tags: []string{"lb-status"}},
		{name: "tls/sni-api-host-lb-other-path", request: parityGet(parityApiName, parityLbName, "/hello")},
		{name: "tls/unknown-sni-unknown-host", request: parityGet(parityUnknownName, parityUnknownName, "/hello")},
		{name: "tls/unknown-sni-api-host", request: parityGet(parityUnknownName, parityApiName, "/hello")},
		{name: "tls/exact-name-under-a-wildcard", request: parityGet(parityExactWildName, parityExactWildName, "/hello")},
		{name: "tls/wildcard-one-label", request: parityGet(parityWildChildName, parityWildChildName, "/hello")},
		{name: "tls/wildcard-two-labels", request: parityGet(parityWildGrandName, parityWildGrandName, "/hello")},
		{name: "tls/wildcard-base", request: parityGet(parityWildBaseName, parityWildBaseName, "/hello")},
		{name: "tls/unexposed-worker-name", request: parityGet(parityWorkerName, parityWorkerName, "/hello")},
		{name: "tls/http10-no-host-lb-status", request: &parityRequest{scheme: "https", method: http.MethodGet, target: parityLbPrefix + "/status", proto: "HTTP/1.0"}, tags: []string{"lb-status"}},
		{name: "tls/http11-no-host", request: &parityRequest{scheme: "https", sni: parityApiName, method: http.MethodGet, target: "/hello"}},
		{name: "tls/host-port-and-case", request: parityGet(parityApiName, "TEST-API.Service.Example:443", "/hello")},
		{name: "tls/host-trailing-dot", request: parityGet(parityApiName, parityApiName+".", "/hello")},
		{name: "tls/host-invalid", request: parityGet(parityApiName, "test-api.service.example@x", "/hello")},
		{name: "tls/ipv6-client", request: parityFrom(parityGet(parityApiName, parityApiName, "/hello"), parityClientIpv6)},
		{name: "tls/connect-wildcard-name", request: parityGet(parityConnectChildName, parityConnectChildName, "/hello")},
		{name: "tls/connect-name-offered-h2", request: parityOverH2(parityGet(parityConnectName, parityConnectName, "/hello")), tags: []string{"connect-h2"}},
		{name: "tls/sni-over-255-bytes", request: parityGet(strings.Repeat(strings.Repeat("a", 63)+".", 4)+parityUnknownName, parityApiName, "/hello"), tags: []string{"long-sni"}},
		{name: "tls/alpn-without-overlap", request: &parityRequest{scheme: "https", sni: parityApiName, alpn: []string{"spdy/3"}, method: http.MethodGet, target: "/hello", host: parityApiName}},
		{name: "tls/alpn-none", request: &parityRequest{scheme: "https", sni: parityApiName, alpn: []string{}, method: http.MethodGet, target: "/hello", host: parityApiName}},
		{name: "tls/tls13-suite", request: &parityRequest{scheme: "https", sni: parityApiName, recordCipher: true, method: http.MethodGet, target: "/hello", host: parityApiName}, tags: []string{"tls-suites"}},
		{name: "tls/tls12-suite", request: &parityRequest{scheme: "https", sni: parityApiName, maxTlsVersion: tls.VersionTLS12, recordCipher: true, method: http.MethodGet, target: "/hello", host: parityApiName}, tags: []string{"tls-suites"}},
		{name: "tls/tls12-suite-connect-name", request: &parityRequest{scheme: "https", sni: parityConnectName, maxTlsVersion: tls.VersionTLS12, recordCipher: true, method: http.MethodGet, target: "/hello", host: parityConnectName}, tags: []string{"tls-suites"}},
		{
			name: "tls/h2-two-authorities-on-one-connection",
			// one after another on one h2 connection, each response read
			// before the next request
			run: func(run *parityCaseRun) {
				requests := []*parityRequest{
					parityOverH2(parityGet(parityApiName, parityApiName, "/one")),
					parityOverH2(parityGet(parityApiName, parityWebName, "/two")),
					parityOverH2(parityGet(parityApiName, parityLbName, parityLbPrefix+"/status")),
					parityOverH2(parityGet(parityApiName, parityUnknownName, "/four")),
				}
				first := run.sequence()
				conn, err := run.dial(requests[0], first)
				if err != nil {
					return
				}
				defer conn.close()
				for i, request := range requests {
					sequence := first
					if 0 < i {
						sequence = run.sequence()
					}
					response, err := conn.roundTripH2(request, sequence)
					run.recordResponse(sequence, response, err)
				}
			},
			tags: []string{"lb-status"},
		},
		{
			name: "tls/h2-authority-differs-from-host",
			run: paritySendRawH2(parityApiName, [][2]string{
				{":method", "GET"}, {":scheme", "https"}, {":authority", parityApiName}, {":path", "/hello"}, {"host", parityWebName},
			}),
		},
		{
			name: "tls/h2-host-without-authority",
			run: paritySendRawH2(parityApiName, [][2]string{
				{":method", "GET"}, {":scheme", "https"}, {":path", "/hello"}, {"host", parityWebName},
			}),
		},
		{
			name: "tls/h2-authority-equals-host",
			run: paritySendRawH2(parityApiName, [][2]string{
				{":method", "GET"}, {":scheme", "https"}, {":authority", parityApiName}, {":path", "/hello"}, {"host", parityApiName},
			}),
		},
		{
			name: "tls/h2-no-authority-no-host",
			run: paritySendRawH2(parityApiName, [][2]string{
				{":method", "GET"}, {":scheme", "https"}, {":path", "/hello"},
			}),
		},

		// the lb's hidden routes, and Route53's check
		{name: "lb/status", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/status"), tags: []string{"lb-status"}},
		{name: "lb/status-other-lb-name", request: parityGet(parityLbOtherName, parityLbOtherName, parityLbPrefix+"/status"), tags: []string{"lb-status"}},
		{name: "lb/status-head", request: parityWith(parityGet(parityLbName, parityLbName, parityLbPrefix+"/status"), http.MethodHead), tags: []string{"lb-status"}},
		{name: "lb/status-post", request: parityWithBody(parityGet(parityLbName, parityLbName, parityLbPrefix+"/status"), http.MethodPost, []byte("x"), false), tags: []string{"lb-status"}},
		{name: "lb/status-range", request: parityWith(parityGet(parityLbName, parityLbName, parityLbPrefix+"/status"), "", [2]string{"Range", "bytes=0-3"}), tags: []string{"lb-status", "lb-status-range"}},
		{name: "lb/status-case-variant", request: parityGet(parityLbName, parityLbName, strings.ToUpper(parityLbPrefix)+"/STATUS")},
		{name: "lb/status-trailing-slash", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/status/")},
		{name: "lb/root", request: parityGet(parityLbName, parityLbName, "/")},
		// no location: nginx's static module answers 405 to a method but
		// GET, HEAD and POST, the index module declining it on a trailing
		// slash
		{name: "lb/root-put", request: parityWith(parityGet(parityLbName, parityLbName, "/"), http.MethodPut)},
		{name: "lb/other-path-delete", request: parityWith(parityGet(parityLbName, parityLbName, "/hello"), http.MethodDelete)},
		{name: "lb/unknown-host-patch", request: parityWith(parityGet(parityUnknownName, parityUnknownName, "/hello"), http.MethodPatch)},
		{name: "lb/by-service", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/service/api/hello?a=b&c")},
		{name: "lb/by-service-status", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/service/api/status")},
		{name: "lb/by-service-unexposed-worker", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/service/worker/jobs")},
		{name: "lb/by-service-not-lb-exposed", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/service/relay/x")},
		{name: "lb/by-service-connect", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/service/connect/hello")},
		{name: "lb/by-service-without-slash", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/service/api"), tags: []string{"slash-redirect"}},
		{name: "lb/by-block", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/b/api/g2/hello"), compareBlocks: true},
		{name: "lb/by-block-status", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/b/worker/g1/status"), compareBlocks: true},
		{name: "lb/by-block-unknown-block", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/b/api/g9/hello")},
		{name: "lb/by-block-without-slash", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/b/api/g1"), tags: []string{"slash-redirect"}},
		{name: "lb/by-service-artifact-route", request: parityWithBody(parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/service/api/sn/attempt-artifact"), http.MethodPost, parityBody(300), false)},
		{name: "lb/by-block-artifact-route", request: parityWithBody(parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/b/api/g1/sn/attempt-artifact"), http.MethodPost, parityBody(300), false), compareBlocks: true},
		{name: "lb/by-service-mcp-streams", request: parityWithBody(parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/service/mcp/stream"), http.MethodPost, parityBody(300), true), tags: []string{"streamed-chunked-whole"}},
		{name: "route53/ipv4", request: parityPlainGet(parityLbName, parityLbPrefix+"/status"), tags: []string{"lb-status"}},
		{name: "route53/ipv6", request: parityFrom(parityPlainGet(parityLbName, parityLbPrefix+"/status"), parityClientIpv6), tags: []string{"lb-status"}},
		{name: "route53/head", request: parityWith(parityPlainGet(parityLbName, parityLbPrefix+"/status"), http.MethodHead), tags: []string{"lb-status"}},
		{name: "route53/limited-client", request: parityFrom(parityPlainGet(parityLbName, parityLbPrefix+"/status"), parityLimitClientIpv6), tags: []string{"lb-status"}},
		{name: "http/lb-by-block-status", request: parityPlainGet(parityLbName, parityLbPrefix+"/by/b/api/g1/status"), compareBlocks: true},
		{name: "http/lb-by-block-status-of-a-websocket-service", request: parityPlainGet(parityLbName, parityLbPrefix+"/by/b/relay/g1/status"), compareBlocks: true},
		{name: "http/lb-by-block-status-of-a-streamable-service", request: parityPlainGet(parityLbName, parityLbPrefix+"/by/b/mcp/g2/status"), compareBlocks: true},
		// every block of every service the route file lists, lb exposed or
		// not, as the lb config routes them all
		{
			name: "http/lb-by-block-status-of-every-block",
			run: func(run *parityCaseRun) {
				for _, service := range run.harness.routeTable.Services {
					for _, block := range service.Blocks {
						run.send(parityPlainGet(parityLbName, parityLbPrefix+"/by/b/"+service.Name+"/"+block.Name+"/status"))
					}
				}
			},
			compareBlocks: true,
		},
		{name: "http/lb-by-service", request: parityPlainGet(parityLbName, parityLbPrefix+"/by/service/api/hello")},
		{name: "http/lb-other-path", request: parityPlainGet(parityLbName, "/hello?x=1")},

		// redirects
		{name: "redirect/alias", request: parityGet(parityRedirectName, parityRedirectName, "/a/b?c=d")},
		{name: "redirect/alias-raw-target", request: parityGet(parityRedirectName, parityRedirectName, "/a/../b%2Fc//d?e=%20f")},
		{name: "redirect/alias-over-h2", request: parityOverH2(parityGet(parityRedirectName, parityRedirectName, "/a?b"))},
		{name: "redirect/alias-port-80", request: parityPlainGet(parityRedirectName, "/a?b")},
		{name: "redirect/service-port-80", request: parityPlainGet(parityApiName, "/a/b?c=d")},
		{name: "redirect/service-port-80-host-case", request: parityPlainGet("TEST-API.Service.Example:80", "/a")},
		{name: "redirect/unknown-port-80", request: parityPlainGet(parityUnknownName, "/a")},
		{name: "redirect/unexposed-port-80", request: parityPlainGet(parityWorkerName, "/a")},
		{name: "redirect/port-80-ipv6", request: parityFrom(parityPlainGet(parityWebName, "/a"), parityClientIpv6)},

		// the /status denial on service names
		{name: "status/api", request: parityGet(parityApiName, parityApiName, "/status")},
		{name: "status/api-query", request: parityGet(parityApiName, parityApiName, "/status?x=1")},
		{name: "status/api-double-slash", request: parityGet(parityApiName, parityApiName, "//status")},
		{name: "status/api-escaped", request: parityGet(parityApiName, parityApiName, "/%73tatus")},
		{name: "status/api-dot-segment", request: parityGet(parityApiName, parityApiName, "/x/../status")},
		{name: "status/api-trailing-slash", request: parityGet(parityApiName, parityApiName, "/status/")},
		{name: "status/api-uppercase", request: parityGet(parityApiName, parityApiName, "/STATUS")},
		{name: "status/api-over-h2", request: parityOverH2(parityGet(parityApiName, parityApiName, "/status"))},
		{name: "status/connect", request: parityGet(parityConnectName, parityConnectName, "/status")},
		{name: "status/web", request: parityGet(parityWebName, parityWebName, "/status")},
		{name: "status/relay-status-none", request: parityGet(parityRelayName, parityRelayName, parityRelayPrefix+"/status")},
		{name: "status/relay-outside-its-prefix", request: parityGet(parityRelayName, parityRelayName, "/status")},
		{name: "status/relay-outside-its-prefix-options", request: parityWith(parityGet(parityRelayName, parityRelayName, "/x"), http.MethodOptions, [2]string{"Origin", parityAllowedOrigin})},

		// CORS, Vary: Origin and frame options
		{name: "cors/allowed-origin", request: parityWith(api, "", [2]string{"Origin", parityAllowedOrigin})},
		{name: "cors/other-origin", request: parityWith(api, "", [2]string{"Origin", parityOtherOrigin})},
		{name: "cors/origin-case", request: parityWith(api, "", [2]string{"Origin", strings.ToUpper(parityAllowedOrigin)})},
		{name: "cors/no-origin", request: api},
		{name: "cors/preflight", request: parityWith(api, http.MethodOptions, [2]string{"Origin", parityAllowedOrigin}, [2]string{"Access-Control-Request-Method", "POST"})},
		{name: "cors/preflight-other-origin", request: parityWith(api, http.MethodOptions, [2]string{"Origin", parityOtherOrigin})},
		{name: "cors/preflight-no-origin", request: parityWith(api, http.MethodOptions)},
		{name: "cors/preflight-route-zone", request: parityWith(parityGet(parityApiName, parityApiName, "/connect/control"), http.MethodOptions, [2]string{"Origin", parityAllowedOrigin})},
		{name: "cors/preflight-over-h2", request: parityOverH2(parityWith(api, http.MethodOptions, [2]string{"Origin", parityAllowedOrigin}))},
		{name: "cors/wildcard", request: parityWith(parityGet(parityWebName, parityWebName, "/page"), "", [2]string{"Origin", parityOtherOrigin})},
		{name: "cors/wildcard-preflight", request: parityWith(parityGet(parityWebName, parityWebName, "/page"), http.MethodOptions, [2]string{"Origin", parityOtherOrigin})},
		{name: "cors/origins-from-api", request: parityWith(parityGet(parityMcpName, parityMcpName, "/mcp"), "", [2]string{"Origin", parityAllowedOrigin})},
		{name: "cors/no-cors-service-options", request: parityWith(parityGet(parityRelayName, parityRelayName, parityRelayPrefix+"/x"), http.MethodOptions, [2]string{"Origin", parityAllowedOrigin})},
		{name: "cors/upstream-vary", request: parityWith(api, "", [2]string{"Origin", parityAllowedOrigin}), scenario: parityRespondScenario(http.StatusOK, [][2]string{{"Content-Type", "text/plain; charset=utf-8"}, {"Vary", "Accept-Encoding"}}, "vary\n")},
		{name: "cors/upstream-allow-origin", request: parityWith(api, "", [2]string{"Origin", parityOtherOrigin}), scenario: parityRespondScenario(http.StatusOK, [][2]string{{"Content-Type", "text/plain; charset=utf-8"}, {"Access-Control-Allow-Origin", "*"}}, "acao\n")},
		{name: "cors/upstream-error", request: parityWith(api, "", [2]string{"Origin", parityAllowedOrigin}), scenario: parityStatusesScenario(http.StatusInternalServerError)},
		{name: "cors/upstream-not-found", request: parityWith(api, "", [2]string{"Origin", parityAllowedOrigin}), scenario: parityStatusesScenario(http.StatusNotFound)},
		{name: "cors/declared-body-over-the-cap", request: parityWith(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, [2]string{"Origin", parityAllowedOrigin}, [2]string{"Content-Length", strconv.Itoa(17 * 1024 * 1024)})},
		{name: "frame/no-policy", request: api, scenario: cspScenario()},
		{name: "frame/ancestors-self", request: api, scenario: cspScenario("frame-ancestors 'self'")},
		{name: "frame/ancestors-none", request: api, scenario: cspScenario("default-src 'self'; frame-ancestors 'none'")},
		{name: "frame/ancestors-source", request: api, scenario: cspScenario("frame-ancestors https://app.service.example")},
		{name: "frame/ancestors-self-and-source", request: api, scenario: cspScenario("frame-ancestors 'self' https://app.service.example")},
		{name: "frame/default-src-only", request: api, scenario: cspScenario("default-src 'self'")},
		{name: "frame/two-policies", request: api, scenario: cspScenario("default-src 'self'", "frame-ancestors https://app.service.example")},
		{name: "frame/uppercase-directive", request: api, scenario: cspScenario("FRAME-ANCESTORS https://app.service.example")},
		{name: "frame/proxy-error", request: api, scenario: parityLargeResponseHeadScenario},

		// streaming
		{name: "stream/response-on-api", run: paritySendStreamedResponse(api), scenario: parityStreamResponseScenario},
		{name: "stream/response-on-api-over-h2", run: paritySendStreamedResponse(parityOverH2(api)), scenario: parityStreamResponseScenario},
		{name: "stream/response-on-mcp", run: paritySendStreamedResponse(parityGet(parityMcpName, parityMcpName, "/events")), scenario: parityStreamResponseScenario},
		{name: "stream/request-on-mcp-chunked", run: paritySendStreamedRequest(parityWithBody(parityGet(parityMcpName, parityMcpName, "/mcp"), http.MethodPost, parityBody(100), true)), scenario: parityStreamRequestScenario},
		{name: "stream/request-on-mcp-declared", run: paritySendStreamedRequest(parityWithBody(parityGet(parityMcpName, parityMcpName, "/mcp"), http.MethodPost, parityBody(100), false)), scenario: parityStreamRequestScenario},
		{name: "stream/request-on-a-streamable-path", run: paritySendStreamedRequest(parityWithBody(parityGet(parityWebName, parityWebName, "/stream/upload"), http.MethodPost, parityBody(100), true)), scenario: parityStreamRequestScenario},
		{name: "stream/streamable-path-raw-target", request: parityGet(parityWebName, parityWebName, "/stream//x/../y?z=%20")},
		{name: "stream/streamable-path-buffering-header", request: parityWithBody(parityGet(parityWebName, parityWebName, "/stream/up"), http.MethodPost, parityBody(300), false)},
		{name: "stream/mcp-buffering-header", request: parityWithBody(parityGet(parityMcpName, parityMcpName, "/mcp"), http.MethodPost, parityBody(300), false)},

		// websockets
		{name: "websocket/forwarded", run: paritySendWebsocket(parityWith(parityGet(parityRelayName, parityRelayName, parityRelayPrefix+"/ws"), "", websocketHeaders...)), scenario: parityWebsocketScenario},
		{name: "websocket/connect-in-process", run: paritySendWebsocket(parityWith(parityGet(parityConnectName, parityConnectName, "/"), "", websocketHeaders...)), scenario: parityWebsocketScenario, tags: []string{"in-process-upgrade"}},
		{name: "websocket/forwarded-without-upgrade", request: parityGet(parityRelayName, parityRelayName, parityRelayPrefix+"/plain"), scenario: parityWebsocketScenario},
		{name: "websocket/upgrade-to-a-plain-service", request: parityWith(api, "", websocketHeaders...)},

		// upstream choice
		// weighted least_conn under held requests: pool's blocks (beta 1, g1
		// 2, g2 3) take requests that are never released until the end, so
		// each choice sees the load the earlier ones left. In between, two
		// requests held on the lb's per-block route of g2 show whether that
		// load counts: nginx keeps the per-block upstream's connection counts
		// apart from the service's.
		{
			name: "balance/least-conn-under-held-requests",
			run: func(run *parityCaseRun) {
				var done sync.WaitGroup
				aggregate := parityGet(parityPoolName, parityPoolName, "/hold")
				perBlock := parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/b/pool/g2/hold")
				parityHold(run, aggregate, 8, &done)
				parityHold(run, perBlock, 2, &done)
				parityHold(run, aggregate, 6, &done)
				run.fire("release")
				done.Wait()
			},
			scenario:      parityHoldScenario,
			compareBlocks: true,
		},
		{name: "balance/ketama-ipv4-and-ipv6", run: parityRing(parityGet(parityMcpName, parityMcpName, "/ring"), parityRingClients()), compareBlocks: true},
		{name: "balance/ketama-retry-walk", run: parityRing(parityGet(parityMcpName, parityMcpName, "/ring"), parityRingClients()[:4]), scenario: parityStatusesScenario(http.StatusServiceUnavailable, http.StatusOK), compareBlocks: true},
		{name: "balance/ketama-retry-walk-ipv6", run: parityRing(parityGet(parityMcpName, parityMcpName, "/ring"), parityRingClients()[24:28]), scenario: parityStatusesScenario(http.StatusServiceUnavailable, http.StatusOK), compareBlocks: true},
		{name: "balance/ketama-retry-walk-post", run: parityRing(parityWithBody(parityGet(parityMcpName, parityMcpName, "/ring"), http.MethodPost, parityBody(50), false), parityRingClients()[24:28]), scenario: parityStatusesScenario(http.StatusServiceUnavailable, http.StatusOK), compareBlocks: true},

		// retries
		{name: "retry/503-then-200", request: parityWithBody(api, http.MethodPost, parityBody(500), false), scenario: parityStatusesScenario(http.StatusServiceUnavailable, http.StatusOK)},
		{name: "retry/503-twice", request: parityWithBody(api, http.MethodPost, parityBody(500), false), scenario: parityStatusesScenario(http.StatusServiceUnavailable)},
		{name: "retry/500", request: parityWithBody(api, http.MethodPost, parityBody(500), false), scenario: parityStatusesScenario(http.StatusInternalServerError)},
		{name: "retry/502", request: parityWithBody(api, http.MethodPost, parityBody(500), false), scenario: parityStatusesScenario(http.StatusBadGateway)},
		{name: "retry/504", request: parityWithBody(api, http.MethodPost, parityBody(500), false), scenario: parityStatusesScenario(http.StatusGatewayTimeout)},
		{name: "retry/503-on-a-block-route", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/b/api/g1/hello"), scenario: parityStatusesScenario(http.StatusServiceUnavailable, http.StatusOK), compareBlocks: true},
		{name: "retry/503-spilled-body", request: parityWithBody(api, http.MethodPost, parityBody(100*1024), false), scenario: parityStatusesScenario(http.StatusServiceUnavailable, http.StatusOK)},
		{name: "retry/503-chunked-body", request: parityWithBody(api, http.MethodPost, parityBody(3000), true), scenario: parityStatusesScenario(http.StatusServiceUnavailable, http.StatusOK)},
		{name: "retry/503-streamed-get", request: parityGet(parityMcpName, parityMcpName, "/mcp"), scenario: parityStatusesScenario(http.StatusServiceUnavailable, http.StatusOK), compareBlocks: true},
		{name: "retry/503-streamed-post-sent-whole", request: parityWithBody(parityGet(parityMcpName, parityMcpName, "/mcp"), http.MethodPost, parityBody(500), false), scenario: parityStatusesScenario(http.StatusServiceUnavailable, http.StatusOK), compareBlocks: true},
		{
			name: "retry/503-streamed-post-in-flight",
			run:  paritySendStreamedRequest(parityWithBody(parityGet(parityMcpName, parityMcpName, "/mcp"), http.MethodPost, parityBody(100), false)),
			// the body read as it streams, then 503 and 200
			scenario: func(exchange *parityUpstreamExchange) {
				exchange.readStreamedBody()
				parityStatusesScenario(http.StatusServiceUnavailable, http.StatusOK)(exchange)
			},
			compareBlocks: true,
		},
		{name: "retry/503-single-block-service", request: parityGet(parityWebName, parityWebName, "/page"), scenario: parityStatusesScenario(http.StatusServiceUnavailable, http.StatusOK)},
		{name: "retry/large-response-head", request: api, scenario: parityLargeResponseHeadScenario},
		{
			name:    "retry/partial-response",
			request: api,
			// a head promising 100 body bytes, 10 of them, and a cut
			// connection
			scenario: func(exchange *parityUpstreamExchange) {
				exchange.readBody()
				exchange.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				exchange.w.Header().Set("Content-Length", "100")
				exchange.answered(http.StatusOK)
				exchange.w.WriteHeader(http.StatusOK)
				io.WriteString(exchange.w, "0123456789")
				http.NewResponseController(exchange.w).Flush()
				panic(http.ErrAbortHandler)
			},
		},

		// header limits, methods, header names and bodies
		{name: "limits/request-line-at-8k", request: parityGet(parityApiName, parityApiName, "/"+strings.Repeat("a", 8*1024-len("GET / HTTP/1.1\r\n")))},
		{name: "limits/request-line-past-8k", request: parityGet(parityApiName, parityApiName, "/"+strings.Repeat("a", 8*1024))},
		{name: "limits/header-at-8k", request: parityWith(api, "", parityLongHeader("X-Parity-Long", 8*1024))},
		{name: "limits/header-past-8k", request: parityWith(api, "", parityLongHeader("X-Parity-Long", 8*1024+100))},
		{name: "limits/head-of-60k", request: parityWith(api, "", headLines(9)...), tags: []string{"header-packing"}},
		{name: "limits/head-of-66k", request: parityWith(api, "", headLines(10)...)},
		// inside LBALT-IMPL §7's band, twice the 64 KiB, where the front
		// answers nginx's 400 itself; past it, Go's 431 is accepted
		{name: "limits/head-of-73k", request: parityWith(api, "", headLines(11)...)},
		{name: "limits/head-of-139k", request: parityWith(api, "", headLines(21)...), tags: []string{"past-max-header-bytes"}},
		{name: "limits/head-of-60k-over-h2", request: parityOverH2(parityWith(api, "", headLines(9)...))},
		// nginx's max_headers: 1000 header lines, Host included, and one more
		// is a 400
		{name: "limits/header-lines-at-1000", request: parityWith(api, "", shortHeaderLines(1000-parityGetOwnHeaderLines)...)},
		{name: "limits/header-lines-past-1000", request: parityWith(api, "", shortHeaderLines(1001-parityGetOwnHeaderLines)...)},
		{name: "limits/header-lines-past-1000-with-ignored-names", request: parityWith(api, "", append(shortHeaderLines(1000-parityGetOwnHeaderLines), [2]string{"X_Parity_Ignored", "v"})...)},
		{name: "limits/trace", request: parityWith(api, "TRACE")},
		{name: "limits/trace-over-h2", request: parityOverH2(parityWith(api, "TRACE"))},
		{name: "limits/lowercase-method", request: parityWith(api, "get")},
		{name: "limits/unknown-method", request: parityWith(api, "PARITY_CHECK-X")},
		{name: "limits/connect-method", request: &parityRequest{scheme: "https", sni: parityApiName, method: http.MethodConnect, target: parityApiName + ":443", host: parityApiName}},
		{name: "limits/underscore-and-dot-header-names", request: parityWith(api, "", [2]string{"X_Under", "1"}, [2]string{"X.Dot", "2"}, [2]string{"X-Kept", "3"})},
		{name: "limits/two-host-headers", request: &parityRequest{scheme: "https", sni: parityApiName, method: http.MethodGet, target: "/hello", host: parityApiName, extraHosts: []string{parityWebName}}},
		{name: "limits/absolute-form-target", request: parityGet(parityApiName, parityWebName, "https://"+parityApiName+"/abs?x=1")},
		{name: "limits/body-declared-over-16m", request: parityWith(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, [2]string{"Content-Length", strconv.Itoa(17 * 1024 * 1024)})},
		{name: "limits/body-chunked-over-16m", request: parityWithBody(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, parityBody(17*1024*1024), true)},
		{name: "limits/body-spilled", request: parityWithBody(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, parityBody(100*1024), false), scenario: parityEchoScenario},
		{name: "limits/body-chunked-buffered", request: parityWithBody(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, parityBody(3000), true), scenario: parityEchoScenario},
		{name: "limits/body-empty-declared", request: parityWithBody(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, []byte{}, false)},
		{name: "limits/body-on-a-get", request: parityWithBody(parityGet(parityApiName, parityApiName, "/hello"), http.MethodGet, parityBody(20), false)},
		{name: "limits/body-over-h2", request: parityOverH2(parityWithBody(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, parityBody(3000), false)), scenario: parityEchoScenario},
		{name: "limits/content-length-and-chunked", request: contentLengthAndChunked, tags: []string{"content-length-beside-chunked"}},
		// a second request on the connection after it reaches the upstream
		// as its own only where the body was read by its chunks
		{
			name:       "limits/content-length-and-chunked-then-another-request",
			run:        paritySendKeepalive(&contentLengthAndChunkedKeptAlive, parityGet(parityApiName, parityApiName, "/two")),
			tags:       []string{"content-length-beside-chunked"},
			concurrent: true,
		},
		{name: "limits/transfer-encoding-not-chunked-last", request: parityWith(parityWithBody(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, []byte("hello"), true), "", [2]string{"Transfer-Encoding", "gzip, chunked"})},
		{name: "limits/two-content-lengths", request: parityWith(parityWithBody(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, []byte("hello"), false), "", [2]string{"Content-Length", "5"}, [2]string{"Content-Length", "6"})},
		{name: "limits/head-method", request: parityWith(api, http.MethodHead)},
		{name: "limits/options-asterisk", request: parityWith(parityGet(parityApiName, parityApiName, "*"), http.MethodOptions)},

		// gzip of text/html
		{name: "gzip/html", request: parityWith(api, "", [2]string{"Accept-Encoding", "gzip"}), scenario: htmlScenario(http.StatusOK)},
		{name: "gzip/html-with-etag", request: parityWith(api, "", [2]string{"Accept-Encoding", "gzip, deflate"}), scenario: htmlScenario(http.StatusOK, [2]string{"ETag", `"parity-1"`}, [2]string{"Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT"})},
		{name: "gzip/html-not-accepted", request: api, scenario: htmlScenario(http.StatusOK)},
		{name: "gzip/html-404", request: parityWith(api, "", [2]string{"Accept-Encoding", "gzip"}), scenario: htmlScenario(http.StatusNotFound)},
		{name: "gzip/html-403", request: parityWith(api, "", [2]string{"Accept-Encoding", "gzip"}), scenario: htmlScenario(http.StatusForbidden)},
		{name: "gzip/html-500", request: parityWith(api, "", [2]string{"Accept-Encoding", "gzip"}), scenario: htmlScenario(http.StatusInternalServerError)},
		{name: "gzip/html-via-a-proxy", request: parityWith(api, "", [2]string{"Accept-Encoding", "gzip"}, [2]string{"Via", "1.1 cache.example"}), scenario: htmlScenario(http.StatusOK)},
		{name: "gzip/html-short", request: parityWith(api, "", [2]string{"Accept-Encoding", "gzip"}), scenario: parityRespondScenario(http.StatusOK, [][2]string{{"Content-Type", "text/html"}}, "<p>short</p>")},
		{name: "gzip/html-encoded-upstream", request: parityWith(api, "", [2]string{"Accept-Encoding", "gzip"}), scenario: htmlScenario(http.StatusOK, [2]string{"Content-Encoding", "identity"})},
		{name: "gzip/html-head", request: parityWith(api, http.MethodHead, [2]string{"Accept-Encoding", "gzip"}), scenario: htmlScenario(http.StatusOK)},
		{name: "gzip/html-http10", request: &parityRequest{scheme: "https", sni: parityApiName, method: http.MethodGet, target: "/hello", host: parityApiName, proto: "HTTP/1.0", headers: [][2]string{{"Accept-Encoding", "gzip"}}}, scenario: htmlScenario(http.StatusOK)},
		{name: "gzip/html-over-h2", request: parityOverH2(parityWith(api, "", [2]string{"Accept-Encoding", "gzip"})), scenario: htmlScenario(http.StatusOK)},
		{name: "gzip/plain-text", request: parityWith(api, "", [2]string{"Accept-Encoding", "gzip"}), scenario: parityRespondScenario(http.StatusOK, [][2]string{{"Content-Type", "text/plain"}}, html)},
		// nginx's type is the value before a ';' only when a charset
		// follows it
		{name: "gzip/html-with-another-parameter", request: parityWith(api, "", [2]string{"Accept-Encoding", "gzip"}), scenario: parityRespondScenario(http.StatusOK, [][2]string{{"Content-Type", "text/html; foo=bar"}}, html)},
		{name: "gzip/html-with-a-space-before-its-charset", request: parityWith(api, "", [2]string{"Accept-Encoding", "gzip"}), scenario: parityRespondScenario(http.StatusOK, [][2]string{{"Content-Type", "text/html ; charset=utf-8"}}, html)},
		{name: "gzip/html-with-an-uppercase-charset", request: parityWith(api, "", [2]string{"Accept-Encoding", "gzip"}), scenario: parityRespondScenario(http.StatusOK, [][2]string{{"Content-Type", "TEXT/HTML;CHARSET=utf-8"}}, html)},
		{name: "gzip/own-404-page", request: parityWith(parityGet(parityLbName, parityLbName, "/missing"), "", [2]string{"Accept-Encoding", "gzip"})},

		// the ingress rewrite
		{name: "ingress/spoofed-headers", request: parityWith(api, "", spoofedHeaders...)},
		{name: "ingress/spoofed-headers-in-process", request: parityWith(parityGet(parityConnectName, parityConnectName, "/hello"), "", spoofedHeaders...)},
		{name: "ingress/spoofed-headers-streamed", request: parityWith(parityGet(parityMcpName, parityMcpName, "/mcp"), "", spoofedHeaders...)},
		{name: "ingress/spoofed-headers-on-lb-routes", request: parityWith(parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/service/api/hello"), "", spoofedHeaders...)},
		{name: "ingress/ipv6-client", request: parityFrom(api, parityClientIpv6)},
		{name: "ingress/spoofed-headers-over-h2", request: parityOverH2(parityWith(api, "", spoofedHeaders...))},
		{name: "ingress/hop-by-hop-headers", request: parityWith(api, "", [2]string{"Connection", "close, X-Parity-Hop"}, [2]string{"X-Parity-Hop", "1"}, [2]string{"Keep-Alive", "timeout=5"}, [2]string{"TE", "trailers"}, [2]string{"Proxy-Connection", "keep-alive"}, [2]string{"Trailer", "X-Parity-Trailer"})},
		{name: "ingress/expect-continue", request: parityWithBody(parityWith(api, "", [2]string{"Expect", "100-continue"}), http.MethodPost, parityBody(100), false)},

		// the uri sent upstream
		{name: "uri/normalized", request: parityGet(parityApiName, parityApiName, "/a//b/../c/%7Euser/./d?q=%20x&r=1")},
		{name: "uri/encoded-slash", request: parityGet(parityApiName, parityApiName, "/a%2Fb/c%3Fd")},
		{name: "uri/escaped-dot-segment", request: parityGet(parityApiName, parityApiName, "/a/%2e%2e/b")},
		{name: "uri/above-the-root", request: parityGet(parityApiName, parityApiName, "/../a")},
		{name: "uri/empty-query", request: parityGet(parityApiName, parityApiName, "/a?")},
		{name: "uri/unicode-and-reserved", request: parityGet(parityApiName, parityApiName, "/caf%C3%A9;v=1/%20x?k=%E2%9C%93")},
		// raw bytes above 0x7f with no % in the path go as received, the
		// path normalized; beside a % they are escaped (quoted_uri)
		{name: "uri/raw-utf8-without-a-percent", request: parityGet(parityApiName, parityApiName, "/a//caf\xc3\xa9/./x?k=%E2%9C%93")},
		{name: "uri/raw-utf8-beside-a-percent", request: parityGet(parityApiName, parityApiName, "/caf\xc3\xa9/%20x")},
		{name: "uri/raw-utf8-on-an-lb-route", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/service/api/caf\xc3\xa9")},
		{name: "uri/raw-utf8-in-process", request: parityGet(parityConnectName, parityConnectName, "/caf\xc3\xa9/x")},
		{name: "uri/lb-route-normalized", request: parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/service/api//x/../y%2Fz?q")},
		{name: "uri/relay-prefix", request: parityGet(parityRelayName, parityRelayName, parityRelayPrefix+"/a?b")},
		{name: "uri/in-process", request: parityGet(parityConnectName, parityConnectName, "/a//b/../c?d")},
		{name: "uri/invalid-escape", request: parityGet(parityApiName, parityApiName, "/a%zz")},
		{name: "uri/escaped-nul", request: parityGet(parityApiName, parityApiName, "/a%00b")},
		{name: "uri/escaped-space-and-plus", request: parityGet(parityApiName, parityApiName, "/a%20b+c")},

		// keepalive and the access-log privacy names
		{name: "keepalive/two-requests", run: paritySendKeepalive(&parityRequest{scheme: "https", sni: parityApiName, method: http.MethodGet, target: "/one", host: parityApiName, keepalive: true}, parityGet(parityApiName, parityApiName, "/two"))},
		{name: "keepalive/no-access-log-name", request: parityGet("quiet.service.example", "quiet.service.example", "/page")},

		// limits, each from an address of its own outside the excluded
		// subnets: on the interface 1 r/min with burst 9, so ten requests
		// pass at once, and three connections; 1 r/min with burst 3 on the
		// api's /connect/control
		{name: "limit/shared-zone", run: paritySendRepeated(parityFrom(api, nextLimitClient()), paritySharedAdmitted+2)},
		{name: "limit/shared-zone-ipv6", run: paritySendRepeated(parityFrom(api, parityLimitClientIpv6), paritySharedAdmitted+2)},
		{name: "limit/route-zone", run: paritySendRepeated(parityFrom(parityGet(parityApiName, parityApiName, "/connect/control"), nextLimitClient()), parityRouteAdmitted+2)},
		{
			name: "limit/route-zone-replaces-the-shared-one",
			run: func() func(run *parityCaseRun) {
				client := nextLimitClient()
				return func(run *parityCaseRun) {
					paritySendRepeated(parityFrom(parityGet(parityApiName, parityApiName, "/connect/control"), client), parityRouteAdmitted+1)(run)
					paritySendRepeated(parityFrom(api, client), paritySharedAdmitted+1)(run)
				}
			}(),
		},
		{
			name: "limit/preflights-uncharged",
			run: func() func(run *parityCaseRun) {
				client := nextLimitClient()
				return func(run *parityCaseRun) {
					paritySendRepeated(parityFrom(parityWith(api, http.MethodOptions, [2]string{"Origin", parityAllowedOrigin}), client), paritySharedAdmitted+2)(run)
					run.send(parityFrom(api, client))
				}
			}(),
		},
		{
			name: "limit/redirects-uncharged",
			run: func() func(run *parityCaseRun) {
				client := nextLimitClient()
				return func(run *parityCaseRun) {
					paritySendRepeated(parityFrom(parityGet(parityRedirectName, parityRedirectName, "/a"), client), paritySharedAdmitted+2)(run)
					paritySendRepeated(parityFrom(parityPlainGet(parityApiName, "/a"), client), paritySharedAdmitted+2)(run)
					run.send(parityFrom(api, client))
				}
			}(),
		},
		{
			name: "limit/declared-body-over-the-cap-uncharged",
			run: func() func(run *parityCaseRun) {
				client := nextLimitClient()
				return func(run *parityCaseRun) {
					paritySendRepeated(parityFrom(parityWith(parityGet(parityApiName, parityApiName, "/upload"), http.MethodPost, [2]string{"Content-Length", strconv.Itoa(17 * 1024 * 1024)}), client), paritySharedAdmitted+2)(run)
					run.send(parityFrom(api, client))
				}
			}(),
		},
		{name: "limit/status-denials-charged", run: paritySendRepeated(parityFrom(parityGet(parityApiName, parityApiName, "/status"), nextLimitClient()), paritySharedAdmitted+2)},
		{name: "limit/not-found-charged", run: paritySendRepeated(parityFrom(parityGet(parityLbName, parityLbName, "/missing"), nextLimitClient()), paritySharedAdmitted+2)},
		{name: "limit/port-80-default-deny-charged", run: paritySendRepeated(parityFrom(parityPlainGet(parityUnknownName, "/a"), nextLimitClient()), paritySharedAdmitted+2)},
		{name: "limit/lb-status-exempt", run: paritySendRepeated(parityFrom(parityGet(parityLbName, parityLbName, parityLbPrefix+"/status"), nextLimitClient()), paritySharedAdmitted+2), tags: []string{"lb-status"}},
		{name: "limit/lb-block-status-exempt", run: paritySendRepeated(parityFrom(parityGet(parityLbName, parityLbName, parityLbPrefix+"/by/b/api/g1/status"), nextLimitClient()), paritySharedAdmitted+2)},
		{name: "limit/lb-status-on-a-service-name-charged", run: paritySendRepeated(parityFrom(parityGet(parityApiName, parityApiName, parityLbPrefix+"/status"), nextLimitClient()), paritySharedAdmitted+2)},
		{name: "limit/in-process-charged", run: paritySendRepeated(parityFrom(parityGet(parityConnectName, parityConnectName, "/hello"), nextLimitClient()), paritySharedAdmitted+2)},
		// as many requests held as the interface's connection limit allows,
		// then one more, which nginx refuses with 429
		{
			name: "limit/connections",
			run: func() func(run *parityCaseRun) {
				request := parityFrom(parityGet(parityApiName, parityApiName, "/hold"), nextLimitClient())
				return func(run *parityCaseRun) {
					var done sync.WaitGroup
					parityHold(run, request, parityConnectionLimitCount, &done)
					run.send(request)
					run.fire("release")
					done.Wait()
				}
			}(),
			scenario: parityHoldScenario,
		},
		// the same as streams of one h2 connection: each stream counts
		// against the address's connections
		{
			name: "limit/connections-per-h2-stream",
			run: func() func(run *parityCaseRun) {
				request := parityOverH2(parityFrom(parityGet(parityApiName, parityApiName, "/hold"), nextLimitClient()))
				return func(run *parityCaseRun) {
					first := run.sequence()
					conn, err := run.dial(request, first)
					if err != nil {
						return
					}
					defer conn.close()
					if _, err := conn.h2(); err != nil {
						run.fact("h2", parityErrorClass(err))
						return
					}
					var done sync.WaitGroup
					for i := range parityConnectionLimitCount {
						sequence := first
						if 0 < i {
							sequence = run.sequence()
						}
						done.Add(1)
						go func() {
							defer done.Done()
							response, err := conn.roundTripH2(request, sequence)
							run.recordResponse(sequence, response, err)
						}()
						arrived := run.wait("arrived/"+strconv.Itoa(sequence), run.harness.settings.BarrierTimeout)
						run.fact("held stream "+strconv.Itoa(sequence)+" arrived", strconv.FormatBool(arrived))
					}
					sequence := run.sequence()
					response, err := conn.roundTripH2(request, sequence)
					run.recordResponse(sequence, response, err)
					run.fire("release")
					done.Wait()
				}
			}(),
			scenario: parityHoldScenario,
		},
		// as many forwarded websocket tunnels open as the connection limit
		// allows, then a plain request, which nginx refuses with 429: an
		// upgraded request holds its connection until the tunnel closes
		{
			name: "limit/connections-held-by-tunnels",
			run: func() func(run *parityCaseRun) {
				clientAddr := nextLimitClient()
				tunnel := parityFrom(parityWith(parityGet(parityRelayName, parityRelayName, parityRelayPrefix+"/ws"), "", websocketHeaders...), clientAddr)
				plain := parityFrom(parityGet(parityApiName, parityApiName, "/hello"), clientAddr)
				return func(run *parityCaseRun) {
					for range parityConnectionLimitCount {
						sequence := run.sequence()
						conn, err := run.dial(tunnel, sequence)
						if err != nil {
							return
						}
						defer conn.close()
						conn.writeH1(tunnel, sequence)
						response, err := conn.readH1Response(tunnel.method)
						if err != nil {
							run.recordResponse(sequence, nil, err)
							continue
						}
						run.recordResponseHead(sequence, response)
					}
					run.send(plain)
				}
			}(),
			scenario: parityWebsocketScenario,
		},
	}
}
