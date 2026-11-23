// The front's deadlines in the timer census (server timer_census.go, CPUOPT7.md
// §11): its http.ResponseController deadlines at front_response_deadline, and
// its upstream connections' at front_upstream_deadline, each once per set in
// the bucket of the timeout, and each clear as a stop. Each test runs a census
// of its own, every call counted.
package nginx

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/urnetwork/server"
)

var timerCensusHorizons = []string{"lt_1ms", "1ms_10ms", "10ms_100ms", "100ms_1s", "1s_10s", "ge_10s", "stop"}

func timerCensusCounts(site server.TimerCensusSite) map[string]float64 {
	counts := map[string]float64{}
	for _, horizon := range timerCensusHorizons {
		counts[horizon] = server.Testing_TimerCensusCount(site, horizon)
	}
	return counts
}

// The counts at site since before.
func timerCensusDeltas(site server.TimerCensusSite, before map[string]float64) map[string]float64 {
	deltas := map[string]float64{}
	for horizon, count := range timerCensusCounts(site) {
		if delta := count - before[horizon]; delta != 0 {
			deltas[horizon] = delta
		}
	}
	return deltas
}

// Requires exactly wants at site since before, and nothing in another bucket.
func requireTimerCensusCounts(t testing.TB, what string, site server.TimerCensusSite, before map[string]float64, wants map[string]float64) {
	t.Helper()
	deltas := timerCensusDeltas(site, before)
	for _, horizon := range timerCensusHorizons {
		if deltas[horizon] != wants[horizon] {
			t.Errorf("%s: %s counted %v in %s, want %v", what, site, deltas[horizon], horizon, wants[horizon])
		}
	}
}

// Each of a response's writes and flushes arms its send_timeout and clears it,
// and the handler's return arms it once more: seven arms and six clears for
// three chunks, as TestRouteHttpSendTimeoutBoundsEachWrite counts them.
func TestRouteHttpCountsEachResponseWriteDeadlineInTheTimerCensus(t *testing.T) {
	t.Cleanup(server.Testing_StartTimerCensus())
	settings := testFrontSettings()
	settings.Http.SendTimeout = 30 * time.Second
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		w.WriteHeader(http.StatusOK)
		for range 3 {
			io.WriteString(w, "chunk")
			http.NewResponseController(w).Flush()
		}
	}
	writer := &testDeadlineResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
	}
	before := timerCensusCounts(server.TimerCensusSiteFrontResponseDeadline)
	testFront.front.HttpsHandler("eth1").ServeHTTP(writer, newTestRequest("GET", "api.service.example", "/v1", true))
	if len(writer.writeDeadlines) != 13 {
		t.Fatalf("%d write deadlines, want 13", len(writer.writeDeadlines))
	}
	requireTimerCensusCounts(t, "three chunks", server.TimerCensusSiteFrontResponseDeadline, before, map[string]float64{"ge_10s": 7, "stop": 6})
}

// A forwarded body arms send_timeout before each chunk it copies and clears it
// as the copy ends.
func TestCopyToClientCountsItsDeadlinesInTheTimerCensus(t *testing.T) {
	t.Cleanup(server.Testing_StartTimerCensus())
	settings := testForwardFrontSettings()
	settings.Http.SendTimeout = 3 * time.Second
	group, _ := newTestGroup(t, testForwardRouteService(testServiceName("census-copy"), []testBlock{
		{name: "g1", weight: 1, port: closedPort(t)},
	}), settings)
	writer := &testDeadlineResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
	}
	before := timerCensusCounts(server.TimerCensusSiteFrontResponseDeadline)
	// two reads of a byte each, then the end
	clientErr, upstreamErr := group.copyToClient(writer, http.NewResponseController(writer), iotest.OneByteReader(strings.NewReader("ab")), nil)
	if clientErr != nil || upstreamErr != nil || writer.Body.String() != "ab" {
		t.Fatalf("copy = %q, %v, %v", writer.Body.String(), clientErr, upstreamErr)
	}
	requireTimerCensusCounts(t, "two chunks", server.TimerCensusSiteFrontResponseDeadline, before, map[string]float64{"1s_10s": 2, "stop": 1})
}

// An upstream connection's write and reads set their deadlines once each, its
// read timeout's only once reads are armed.
func TestUpstreamConnectionCountsItsDeadlinesInTheTimerCensus(t *testing.T) {
	t.Cleanup(server.Testing_StartTimerCensus())
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	go func() {
		var b [1]byte
		io.ReadFull(peer, b[:])
		peer.Write([]byte("b"))
	}()
	upstreamConn := &upstreamConnection{
		conn:          conn,
		readTimeout:   3 * time.Second,
		writeTimeout:  30 * time.Second,
		headBytesLeft: -1,
	}
	before := timerCensusCounts(server.TimerCensusSiteFrontUpstreamDeadline)
	if _, err := upstreamConn.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	upstreamConn.armRead()
	upstreamConn.armRead()
	var b [1]byte
	if _, err := upstreamConn.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	requireTimerCensusCounts(t, "a write, two arms and a read", server.TimerCensusSiteFrontUpstreamDeadline, before, map[string]float64{"ge_10s": 1, "1s_10s": 2})
}

// A forwarded request's upstream reads and writes count at
// front_upstream_deadline in the bucket of the proxy timeouts, and its copy to
// the client at front_response_deadline in send_timeout's, with its clear.
func TestForwardCountsItsDeadlinesInTheTimerCensus(t *testing.T) {
	t.Cleanup(server.Testing_StartTimerCensus())
	settings := testForwardFrontSettings()
	settings.Forward.ProxyReadTimeout = 30 * time.Second
	settings.Forward.ProxySendTimeout = 30 * time.Second
	settings.Http.SendTimeout = 3 * time.Second
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testForwardRouteService(testServiceName("census-forward"), []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), settings)
	front := newTestForwardFront(t, group, testForwardOptions)
	beforeUpstream := timerCensusCounts(server.TimerCensusSiteFrontUpstreamDeadline)
	beforeResponse := timerCensusCounts(server.TimerCensusSiteFrontResponseDeadline)
	response, body := rawFrontRequest(t, front, "GET /census HTTP/1.1\r\nHost: census.service.example\r\n\r\n")
	if response.StatusCode != http.StatusOK || string(body) != "g1" {
		t.Fatalf("response %d %q, want 200 from g1", response.StatusCode, body)
	}
	front.waitServed(t)
	upstreamDeltas := timerCensusDeltas(server.TimerCensusSiteFrontUpstreamDeadline, beforeUpstream)
	if len(upstreamDeltas) != 1 || upstreamDeltas["ge_10s"] < 2 {
		t.Errorf("the upstream exchange counted %v, want a write and reads, all at 30 s", upstreamDeltas)
	}
	responseDeltas := timerCensusDeltas(server.TimerCensusSiteFrontResponseDeadline, beforeResponse)
	if len(responseDeltas) != 2 || responseDeltas["1s_10s"] < 1 || responseDeltas["stop"] != 1 {
		t.Errorf("the copy to the client counted %v, want an arm a chunk at send_timeout and one clear", responseDeltas)
	}
}

// A tunnel clears its upstream connection's deadlines once, and moves the
// upstream's idle read deadline and write deadline as bytes cross.
func TestTunnelCountsItsUpstreamDeadlinesInTheTimerCensus(t *testing.T) {
	t.Cleanup(server.Testing_StartTimerCensus())
	settings := testForwardFrontSettings()
	settings.Forward.StreamTimeout = 60 * time.Second
	fixture := newTunnelFixture(t, "census-tunnel", settings)
	before := timerCensusCounts(server.TimerCensusSiteFrontUpstreamDeadline)
	clientConn, clientReader, upstreamConn, upstreamReader := fixture.open(t)
	io.WriteString(clientConn, "ping")
	readExactly(t, upstreamReader, "ping")
	io.WriteString(upstreamConn, "pong")
	readExactly(t, clientReader, "pong")
	deltas := timerCensusDeltas(server.TimerCensusSiteFrontUpstreamDeadline, before)
	// the switch's clear, then StreamTimeout's idle reads and upstream writes
	if deltas["stop"] != 1 || deltas["ge_10s"] < 3 || len(deltas) != 2 {
		t.Errorf("the tunnel counted %v at the upstream, want one clear and its 60 s deadlines", deltas)
	}
	clientConn.Close()
	readEnd(t, upstreamReader)
	fixture.front.waitServed(t)
}

// A refused body's lingering read is bounded by LingeringTimeout, counted at
// front_response_deadline: 200 ms, in its bucket.
func TestForwardRefusalCountsItsLingeringReadInTheTimerCensus(t *testing.T) {
	t.Cleanup(server.Testing_StartTimerCensus())
	settings := testForwardFrontSettings()
	settings.Body.MemoryBudgetBytes = 1
	settings.Http.LingeringTimeout = 200 * time.Millisecond
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testForwardRouteService(testServiceName("census-lingering"), []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), settings)
	front := newTestForwardFront(t, group, testForwardOptions)
	clientConn, err := net.Dial("tcp", front.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer clientConn.Close()
	before := timerCensusCounts(server.TimerCensusSiteFrontResponseDeadline)
	io.WriteString(clientConn, "POST /upload HTTP/1.1\r\nHost: upload.service.example\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n")
	clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response %v %v, want the local 503", response, err)
	}
	front.waitServed(t)
	if deltas := timerCensusDeltas(server.TimerCensusSiteFrontResponseDeadline, before); deltas["100ms_1s"] != 1 {
		t.Errorf("the refusal counted %v, want its 200 ms lingering read", deltas)
	}
}

// A front's own answer to a request with an unread body bounds the lingering
// read by LingeringTimeout, counted at front_response_deadline.
func TestRouteHttpOwnAnswerCountsItsLingeringReadInTheTimerCensus(t *testing.T) {
	t.Cleanup(server.Testing_StartTimerCensus())
	settings := testFrontSettings()
	settings.Http.LingeringTimeout = 200 * time.Millisecond
	testServer := newTestConfiguredServer(t, newTestFront(t, loadTestRouteTable(t), settings), false)
	client := dialTestServerH1Client(t, testServer.Listener.Addr().String())
	client.conn.SetDeadline(time.Now().Add(3 * time.Second))
	before := timerCensusCounts(server.TimerCensusSiteFrontResponseDeadline)
	fmt.Fprintf(client.conn, "POST /status HTTP/1.1\r\nHost: api.service.example\r\nContent-Length: 1000\r\n\r\n0123456789")
	response, err := http.ReadResponse(client.reader, nil)
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if deltas := timerCensusDeltas(server.TimerCensusSiteFrontResponseDeadline, before); deltas["100ms_1s"] != 1 {
		t.Errorf("the own answer counted %v, want its 200 ms lingering read", deltas)
	}
}

// A streamed body's reads move the client's read deadline through the
// forwarder, counted at front_response_deadline.
func TestForwardStreamedBodyCountsItsReadDeadlinesInTheTimerCensus(t *testing.T) {
	t.Cleanup(server.Testing_StartTimerCensus())
	raw := newRawUpstream(t)
	group, _ := newTestGroup(t, testForwardRouteService(testServiceName("census-streamed"), []testBlock{
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
	before := timerCensusCounts(server.TimerCensusSiteFrontResponseDeadline)
	io.WriteString(clientConn, "POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\nContent-Length: 5\r\n\r\n")
	upstreamConn, _, request := raw.accept(t)
	upstreamConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	io.WriteString(clientConn, "hello")
	readExactly(t, request.Body, "hello")
	readEnd(t, request.Body)
	io.WriteString(upstreamConn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response %v %v, want the block's 200", response, err)
	}
	readExactly(t, response.Body, "ok")
	front.waitServed(t)
	deltas := timerCensusDeltas(server.TimerCensusSiteFrontResponseDeadline, before)
	reads := 0.0
	for _, horizon := range []string{"lt_1ms", "1ms_10ms", "10ms_100ms", "100ms_1s", "1s_10s", "ge_10s"} {
		reads += deltas[horizon]
	}
	if reads < 1 {
		t.Errorf("the streamed body counted %v, want its read deadlines", deltas)
	}
}
