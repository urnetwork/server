package nginx

// Tests of D7's retries as the review amended them: which failures move a
// request to another block, which pass through or end it, how the final
// failure maps to a status, and that every physical attempt is counted,
// a stale keepalive connection's redo included.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Synthetic body bytes, distinct at every offset within 64 KiB.
func testBodyBytes(size int) []byte {
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i*131 + i>>8 + 7)
	}
	return body
}

// A refused connect moves the request to another block, where it succeeds.
func TestRetryRefusedConnectOnAnotherBlock(t *testing.T) {
	serviceName := testServiceName("retry-refused")
	live := newRecordingUpstream(t, "g2", nil)
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: closedPort(t)},
		{name: "g2", weight: 1, port: live.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, testForwardOptions)
	request, _ := http.NewRequest(http.MethodPost, front.url("/write"), strings.NewReader("payload"))
	response, body, err := testDo(t, request)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "g2" {
		t.Fatalf("response %v %q %v, want 200 from g2", response, body, err)
	}
	if got := string(live.recorded()[0].body); got != "payload" {
		t.Errorf("g2 got body %q", got)
	}
	if attemptCount(serviceName, "g1", "error") != 1 || attemptCount(serviceName, "g2", "response") != 1 || retryCount(serviceName, "error") != 1 {
		t.Errorf("attempts g1 error %v, g2 response %v, retries %v; want 1 each",
			attemptCount(serviceName, "g1", "error"), attemptCount(serviceName, "g2", "response"), retryCount(serviceName, "error"))
	}
}

// A 503 moves the request to another block with its body replayed byte for
// byte, the spilled part included; a final 503 passes through as the
// block sent it; and a service with one block makes one attempt.
func TestRetry503ReplaysBodyByteForByte(t *testing.T) {
	requestBody := testBodyBytes(64*1024 + 1000)
	answer := func(status int) func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
		return func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
			w.Header().Set("X-Block", recorded.block)
			w.WriteHeader(status)
			io.WriteString(w, "answer from "+recorded.block)
		}
	}

	serviceName := testServiceName("retry-503")
	busy := newRecordingUpstream(t, "g1", answer(http.StatusServiceUnavailable))
	live := newRecordingUpstream(t, "g2", answer(http.StatusOK))
	group, bodyStore := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: busy.port},
		{name: "g2", weight: 1, port: live.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, testForwardOptions)
	request, _ := http.NewRequest(http.MethodPost, front.url("/sn/attempt-artifact"), bytes.NewReader(requestBody))
	response, body, err := testDo(t, request)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "answer from g2" {
		t.Fatalf("response %v %q %v, want g2's 200", response, body, err)
	}
	front.waitServed(t)
	if !bytes.Equal(busy.recorded()[0].body, requestBody) || !bytes.Equal(live.recorded()[0].body, requestBody) {
		t.Errorf("a block got a different body: %d and %d bytes of %d", len(busy.recorded()[0].body), len(live.recorded()[0].body), len(requestBody))
	}
	if retryCount(serviceName, "http_503") != 1 || attemptCount(serviceName, "g1", "http_503") != 1 {
		t.Errorf("the 503 was not counted as an attempt and a retry")
	}
	if memoryBytes, spillBytes, spillFiles, bodies := bodyStore.usage(); memoryBytes != 0 || spillBytes != 0 || spillFiles != 0 || bodies != 0 {
		t.Errorf("body store still holds %d %d %d %d", memoryBytes, spillBytes, spillFiles, bodies)
	}

	// both blocks busy: the second 503 passes through
	finalServiceName := testServiceName("retry-503-final")
	first := newRecordingUpstream(t, "g1", answer(http.StatusServiceUnavailable))
	second := newRecordingUpstream(t, "g2", answer(http.StatusServiceUnavailable))
	finalGroup, _ := newTestGroup(t, testForwardRouteService(finalServiceName, []testBlock{
		{name: "g1", weight: 1, port: first.port},
		{name: "g2", weight: 1, port: second.port},
	}), testForwardFrontSettings())
	finalFront := newTestForwardFront(t, finalGroup, testForwardOptions)
	request, _ = http.NewRequest(http.MethodPost, finalFront.url("/"), bytes.NewReader(requestBody))
	response, body, err = testDo(t, request)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable || string(body) != "answer from g2" || response.Header.Get("X-Block") != "g2" {
		t.Fatalf("final response %v %q %v, want g2's 503", response, body, err)
	}
	if forwardErrorCount(finalServiceName, http.StatusServiceUnavailable) != 0 || forwardErrorCount(finalServiceName, http.StatusBadGateway) != 0 {
		t.Errorf("an upstream 503 was counted as the front's own error")
	}

	// one block: nothing to retry on
	singleServiceName := testServiceName("retry-503-single")
	only := newRecordingUpstream(t, "g1", answer(http.StatusServiceUnavailable))
	singleGroup, _ := newTestGroup(t, testForwardRouteService(singleServiceName, []testBlock{
		{name: "g1", weight: 1, port: only.port},
	}), testForwardFrontSettings())
	singleFront := newTestForwardFront(t, singleGroup, testForwardOptions)
	request, _ = http.NewRequest(http.MethodGet, singleFront.url("/"), nil)
	response, _, err = testDo(t, request)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable || len(only.recorded()) != 1 {
		t.Fatalf("single block: %v %v, %d requests; want one 503", response, err, len(only.recorded()))
	}
}

// 500, 502 and 504 from a block pass through, never retried: a retried 502
// once ran a password reset twice.
func TestRetryNeverOn500502504(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout} {
		serviceName := testServiceName("retry-never-" + http.StatusText(status))
		failing := newRecordingUpstream(t, "g1", func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
			w.WriteHeader(status)
			io.WriteString(w, "application error")
		})
		other := newRecordingUpstream(t, "g2", nil)
		group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
			{name: "g1", weight: 1, port: failing.port},
			{name: "g2", weight: 1, port: other.port},
		}), testForwardFrontSettings())
		front := newTestForwardFront(t, group, testForwardOptions)
		request, _ := http.NewRequest(http.MethodPost, front.url("/auth/password-reset"), strings.NewReader("reset"))
		response, body, err := testDo(t, request)
		if err != nil || response.StatusCode != status || string(body) != "application error" {
			t.Errorf("%d: response %v %q %v, want it passed through", status, response, body, err)
		}
		if len(other.recorded()) != 0 || len(failing.recorded()) != 1 {
			t.Errorf("%d was retried", status)
		}
	}
}

// A response cut short after its head is never retried: the client sees a
// truncated response, aborted rather than ended cleanly.
func TestRetryNeverOnPartialResponse(t *testing.T) {
	raw := newRawUpstream(t)
	other := newRecordingUpstream(t, "g2", nil)
	group, _ := newTestGroup(t, testForwardRouteService("retry-partial", []testBlock{
		{name: "g1", weight: 1, port: raw.port},
		{name: "g2", weight: 1, port: other.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, testForwardOptions)
	type result struct {
		response *http.Response
		body     []byte
		err      error
	}
	results := make(chan result, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodGet, front.url("/"), nil)
		response, body, err := testDo(t, request)
		results <- result{response: response, body: body, err: err}
	}()
	conn, _, _ := raw.accept(t)
	io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial")
	conn.Close()
	got := receiveBounded(t, results, "result")
	if got.response == nil || got.response.StatusCode != http.StatusOK || got.err == nil || string(got.body) != "partial" {
		t.Fatalf("response %v %q %v, want a truncated 200", got.response, got.body, got.err)
	}
	if recovered := front.waitServed(t); recovered != http.ErrAbortHandler {
		t.Errorf("serveHttp ended with %v, want http.ErrAbortHandler", recovered)
	}
	if len(other.recorded()) != 0 {
		t.Errorf("a partial response was retried")
	}
}

// A per-block route reports its block's own result and is never retried
// elsewhere, its connect error and its 503 included.
func TestRetryNeverForNamedBlock(t *testing.T) {
	serviceName := testServiceName("retry-named")
	busy := newRecordingUpstream(t, "g2", func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	other := newRecordingUpstream(t, "g3", nil)
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: closedPort(t)},
		{name: "g2", weight: 1, port: busy.port},
		{name: "g3", weight: 1, port: other.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.blockName = r.URL.Query().Get("block")
		return options
	})
	request, _ := http.NewRequest(http.MethodGet, front.url("/status?block=g1"), nil)
	if response, _, err := testDo(t, request); err != nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("named refused block: %v %v, want 502", response, err)
	}
	request, _ = http.NewRequest(http.MethodGet, front.url("/status?block=g2"), nil)
	if response, _, err := testDo(t, request); err != nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("named busy block: %v %v, want its 503", response, err)
	}
	if len(other.recorded()) != 0 || retryCount(serviceName, "error") != 0 || retryCount(serviceName, "http_503") != 0 {
		t.Errorf("a named block's request was retried")
	}
}

// A streamed body whose request began going upstream, and that its first
// read didn't return whole, is never retried; one whose connect was refused,
// before anything was sent, is. The first body comes in two parts, the
// second once the block has the first, as nginx streams a body its first
// pass reads only part of.
func TestRetryStreamedOnlyBeforeSending(t *testing.T) {
	streamedOptions := func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		return options
	}

	raw := newRawUpstream(t)
	other := newRecordingUpstream(t, "g2", nil)
	sentServiceName := testServiceName("retry-streamed-sent")
	group, _ := newTestGroup(t, testForwardRouteService(sentServiceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
		{name: "g2", weight: 1, port: other.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, streamedOptions)
	clientConn, err := net.Dial("tcp", front.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer clientConn.Close()
	io.WriteString(clientConn, "POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\nContent-Length: 8\r\n\r\nstre")
	conn, _, request := raw.accept(t)
	// a bound, so a body held back fails the read rather than hangs it
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	readExactly(t, request.Body, "stre")
	io.WriteString(clientConn, "amed")
	readExactly(t, request.Body, "amed")
	if request.Header.Get(urRequestBufferingHeader) != "off" {
		t.Fatalf("first block got buffering %q", request.Header.Get(urRequestBufferingHeader))
	}
	// the block read the request, then closed without answering
	conn.Close()
	clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if response, err := http.ReadResponse(bufio.NewReader(clientConn), nil); err != nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("sent streamed request: %v %v, want 502", response, err)
	}
	front.waitServed(t)
	if len(other.recorded()) != 0 {
		t.Errorf("a streamed body was retried after it was sent")
	}

	refusedServiceName := testServiceName("retry-streamed-refused")
	live := newRecordingUpstream(t, "g2", nil)
	refusedGroup, _ := newTestGroup(t, testForwardRouteService(refusedServiceName, []testBlock{
		{name: "g1", weight: 1, port: closedPort(t)},
		{name: "g2", weight: 1, port: live.port},
	}), testForwardFrontSettings())
	refusedFront := newTestForwardFront(t, refusedGroup, streamedOptions)
	request2, _ := http.NewRequest(http.MethodPost, refusedFront.url("/mcp"), strings.NewReader("streamed"))
	response, responseBody, err := testDo(t, request2)
	if err != nil || response.StatusCode != http.StatusOK || string(responseBody) != "g2" || string(live.recorded()[0].body) != "streamed" {
		t.Fatalf("refused streamed request: %v %q %v, want g2 with the whole body", response, responseBody, err)
	}
}

// A conn whose writes fail on demand, before writing anything.
type faultConn struct {
	net.Conn
	failWrites atomic.Bool
}

// The failure a faultConn's writes return once set to fail.
var errInjectedWrite = errors.New("injected write failure")

// Fails with nothing written once failWrites is set.
func (self *faultConn) Write(p []byte) (int, error) {
	if self.failWrites.Load() {
		return 0, errInjectedWrite
	}
	return self.Conn.Write(p)
}

// A reused keepalive connection that fails before any request byte was
// written is redone on a fresh connection to the same block: it is counted
// as a physical attempt, but not as one of D7's, so a service with one block
// still succeeds, and the block sees the request once.
func TestStaleKeepaliveRedoIsNotAnAttempt(t *testing.T) {
	serviceName := testServiceName("stale-redo")
	upstream := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testForwardFrontSettings())
	dialer := &net.Dialer{}
	dialedConnsLock := sync.Mutex{}
	dialedConns := []*faultConn{}
	group.dialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		dialedConn := &faultConn{Conn: conn}
		dialedConnsLock.Lock()
		defer dialedConnsLock.Unlock()
		dialedConns = append(dialedConns, dialedConn)
		return dialedConn, nil
	}
	front := newTestForwardFront(t, group, testForwardOptions)

	request, _ := http.NewRequest(http.MethodPost, front.url("/first"), strings.NewReader("one"))
	if response, _, err := testDo(t, request); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("first request: %v %v", response, err)
	}
	front.waitServed(t)
	if group.blocks[0].pool.idleCount() != 1 {
		t.Fatalf("the first connection was not pooled")
	}
	// the pooled connection goes stale
	dialedConns[0].failWrites.Store(true)

	request, _ = http.NewRequest(http.MethodPost, front.url("/second"), strings.NewReader("two"))
	response, body, err := testDo(t, request)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "g1" {
		t.Fatalf("second request: %v %q %v, want 200", response, body, err)
	}
	recorded := upstream.recorded()
	if len(recorded) != 2 || recorded[1].requestUri != "/second" || string(recorded[1].body) != "two" {
		t.Fatalf("block saw %d requests, want the first and the redo once", len(recorded))
	}
	if len(dialedConns) != 2 {
		t.Errorf("dialed %d connections, want the redo's fresh one", len(dialedConns))
	}
	if attemptCount(serviceName, "g1", "stale_redo") != 1 || attemptCount(serviceName, "g1", "response") != 2 || retryCount(serviceName, "error") != 0 {
		t.Errorf("attempts: stale_redo %v response %v retries %v; want 1, 2 and 0",
			attemptCount(serviceName, "g1", "stale_redo"), attemptCount(serviceName, "g1", "response"), retryCount(serviceName, "error"))
	}
	// the stale connection counts as reused, and the redo's as dialed
	checkConnectionCounts(t, serviceName, "after the redo", 1, 2, 0)
}

// A reused keepalive connection that fails after the request was written,
// as when a block closes it idle in the instant the request arrives, is one
// of D7's attempts, since the block may have seen the request: it isn't
// redone on the block, so a service with one block answers 502, where
// nginx, which gives any error on a cached connection a free try, redoes it
// there (LBALT-IMPL.md §4). The block sees the request once, and nothing
// dials again.
func TestStaleKeepaliveFailureAfterTheWriteIsAnAttempt(t *testing.T) {
	serviceName := testServiceName("stale-after-write")
	raw := newRawUpstream(t)
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
	}), testForwardFrontSettings())
	var dialCount atomic.Int32
	wrapGroupDials(group, func(conn net.Conn, address string) net.Conn {
		dialCount.Add(1)
		return conn
	})
	front := newTestForwardFront(t, group, testForwardOptions)
	type result struct {
		response *http.Response
		err      error
	}
	send := func(method string, path string, body string) <-chan result {
		results := make(chan result, 1)
		go func() {
			request, _ := http.NewRequest(method, front.url(path), strings.NewReader(body))
			response, _, err := testDo(t, request)
			results <- result{response: response, err: err}
		}()
		return results
	}

	firstResults := send(http.MethodGet, "/one", "")
	conn, reader, _ := raw.accept(t)
	io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	if first := receiveBounded(t, firstResults, "the first response"); first.err != nil || first.response.StatusCode != http.StatusOK {
		t.Fatalf("first request: %v %v", first.response, first.err)
	}
	front.waitServed(t)
	if group.blocks[0].pool.idleCount() != 1 {
		t.Fatalf("the first connection was not pooled")
	}

	secondResults := send(http.MethodPost, "/two", "two")
	// the request reaches the block whole on the pooled connection, which
	// the block then closes unanswered
	second, err := http.ReadRequest(reader)
	if err != nil {
		t.Fatalf("the pooled connection carried no request: %v", err)
	}
	secondBody, _ := io.ReadAll(second.Body)
	if second.RequestURI != "/two" || string(secondBody) != "two" {
		t.Fatalf("the pooled connection carried %s %q", second.RequestURI, secondBody)
	}
	conn.Close()
	if second := receiveBounded(t, secondResults, "the second response"); second.err != nil || second.response.StatusCode != http.StatusBadGateway {
		t.Fatalf("second request: %v %v, want the front's 502", second.response, second.err)
	}
	if dialCount.Load() != 1 {
		t.Errorf("%d dials, want only the first", dialCount.Load())
	}
	if attemptCount(serviceName, "g1", "error") != 1 || attemptCount(serviceName, "g1", "stale_redo") != 0 || forwardErrorCount(serviceName, http.StatusBadGateway) != 1 {
		t.Errorf("attempts: error %v, stale_redo %v, 502s %v; want 1, 0 and 1",
			attemptCount(serviceName, "g1", "error"), attemptCount(serviceName, "g1", "stale_redo"), forwardErrorCount(serviceName, http.StatusBadGateway))
	}
}

// With no attempt left, a connect error is a 502 and a timeout a 504, as
// nginx maps them; each is counted as the front's own error.
func TestRetryFinalFailureMapping(t *testing.T) {
	refusedServiceName := testServiceName("final-refused")
	refusedGroup, _ := newTestGroup(t, testForwardRouteService(refusedServiceName, []testBlock{
		{name: "g1", weight: 1, port: closedPort(t)},
		{name: "g2", weight: 1, port: closedPort(t)},
	}), testForwardFrontSettings())
	refusedFront := newTestForwardFront(t, refusedGroup, testForwardOptions)
	request, _ := http.NewRequest(http.MethodGet, refusedFront.url("/"), nil)
	if response, _, err := testDo(t, request); err != nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("refused blocks: %v %v, want 502", response, err)
	}
	if attemptCount(refusedServiceName, "g1", "error")+attemptCount(refusedServiceName, "g2", "error") != 2 || forwardErrorCount(refusedServiceName, http.StatusBadGateway) != 1 {
		t.Errorf("want two failed attempts and one 502")
	}

	// a block that accepts and never answers times out its read
	settings := testForwardFrontSettings()
	settings.Forward.ProxyReadTimeout = 50 * time.Millisecond
	raw := newRawUpstream(t)
	timeoutServiceName := testServiceName("final-timeout")
	timeoutGroup, _ := newTestGroup(t, testForwardRouteService(timeoutServiceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
	}), settings)
	timeoutFront := newTestForwardFront(t, timeoutGroup, testForwardOptions)
	request, _ = http.NewRequest(http.MethodGet, timeoutFront.url("/"), nil)
	if response, _, err := testDo(t, request); err != nil || response.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("silent block: %v %v, want 504", response, err)
	}
	if attemptCount(timeoutServiceName, "g1", "timeout") != 1 || forwardErrorCount(timeoutServiceName, http.StatusGatewayTimeout) != 1 {
		t.Errorf("want one timed out attempt and one 504")
	}

	// a dial past the connect timeout
	dialTimeoutServiceName := testServiceName("final-dial-timeout")
	dialTimeoutGroup, _ := newTestGroup(t, testForwardRouteService(dialTimeoutServiceName, []testBlock{
		{name: "g1", weight: 1, port: closedPort(t)},
	}), testForwardFrontSettings())
	dialTimeoutGroup.dialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: network, Err: dialTimeoutError{}}
	}
	dialTimeoutFront := newTestForwardFront(t, dialTimeoutGroup, testForwardOptions)
	request, _ = http.NewRequest(http.MethodGet, dialTimeoutFront.url("/"), nil)
	if response, _, err := testDo(t, request); err != nil || response.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("dial timeout: %v %v, want 504", response, err)
	}
}

// A dial error that reports a timeout, as net.Dialer's does.
type dialTimeoutError struct{}

// The text net.Dialer's timeout carries.
func (self dialTimeoutError) Error() string { return "i/o timeout" }

// Always a timeout.
func (self dialTimeoutError) Timeout() bool { return true }

// Always temporary, as a timeout is.
func (self dialTimeoutError) Temporary() bool { return true }

// A timeout moves the request to another block; when that retry finds no
// eligible block, the answer is nginx's "no live upstreams" 502, not the
// timeout's 504.
func TestRetryAfterTimeoutWithNoLiveBlockIs502(t *testing.T) {
	settings := testForwardFrontSettings()
	settings.Forward.MaxBlockActiveRequests = 1
	raw := newRawUpstream(t)
	release := make(chan struct{})
	arrivals := make(chan *recordedRequest)
	busy := newRecordingUpstream(t, "g2", func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
		<-release
	})
	busy.arrivals = arrivals
	serviceName := testServiceName("retry-timeout-no-live")
	// g2's weight makes it the first pick of two idle blocks
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
		{name: "g2", weight: 2, port: busy.port},
	}), settings)
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		if r.URL.Path == "/short" {
			options.readTimeout = 50 * time.Millisecond
		}
		return options
	})
	defer close(release)

	go func() {
		request, _ := http.NewRequest(http.MethodGet, front.url("/held"), nil)
		testDo(t, request)
	}()
	if held := receiveBounded(t, arrivals, "arrival at g2"); held.requestUri != "/held" {
		t.Fatalf("g2 got %s first", held.requestUri)
	}

	// g2 is at its cap, so this goes to g1, which never answers
	request, _ := http.NewRequest(http.MethodGet, front.url("/short"), nil)
	response, _, err := testDo(t, request)
	if err != nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("retry with no live block: %v %v, want 502", response, err)
	}
	if attemptCount(serviceName, "g1", "timeout") != 1 || retryCount(serviceName, "timeout") != 1 || forwardErrorCount(serviceName, http.StatusBadGateway) != 1 {
		t.Errorf("want a timed out attempt, its retry and a 502")
	}
	if len(busy.recorded()) != 1 {
		t.Errorf("the retry reached the block at its cap")
	}
}

// A block that went away after its connection was pooled: the buffered POST
// moves to another block with its body, as across an api drain.
func TestRetryBufferedPostAcrossBlockShutdown(t *testing.T) {
	serviceName := testServiceName("retry-drain")
	draining := newRecordingUpstream(t, "g1", nil)
	live := newRecordingUpstream(t, "g2", nil)
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: draining.port},
		{name: "g2", weight: 1, port: live.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.blockName = r.URL.Query().Get("block")
		return options
	})
	// a named request pools a connection to g1 without moving the balancer
	request, _ := http.NewRequest(http.MethodGet, front.url("/warm?block=g1"), nil)
	if response, _, err := testDo(t, request); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("warm request: %v %v", response, err)
	}
	front.waitServed(t)
	if group.blocks[0].pool.idleCount() != 1 {
		t.Fatalf("no pooled connection to g1")
	}
	draining.server.Close()

	requestBody := testBodyBytes(70 * 1024)
	request, _ = http.NewRequest(http.MethodPost, front.url("/connect/control"), bytes.NewReader(requestBody))
	response, body, err := testDo(t, request)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "g2" {
		t.Fatalf("post across the shutdown: %v %q %v, want g2", response, body, err)
	}
	if !bytes.Equal(live.recorded()[0].body, requestBody) {
		t.Errorf("g2 got a different body")
	}
}

// A malformed response head that arrives while the request body is still
// being written is the block's invalid header, never retried: cutting the
// connection to end the write must not make the write's failure the cause.
func TestRetryNeverOnEarlyInvalidHead(t *testing.T) {
	serviceName := testServiceName("retry-early-invalid")
	raw := newRawUpstream(t)
	other := newRecordingUpstream(t, "g2", nil)
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
		{name: "g2", weight: 1, port: other.port},
	}), testForwardFrontSettings())
	front := newTestForwardFront(t, group, testForwardOptions)
	responses := make(chan *http.Response, 1)
	go func() {
		// more than loopback buffers hold, so the write is still going
		request, _ := http.NewRequest(http.MethodPost, front.url("/"), bytes.NewReader(testBodyBytes(16*1024*1024)))
		response, _, _ := testDo(t, request)
		responses <- response
	}()
	conn, _, _ := raw.accept(t)
	// the block answers without reading the body
	io.WriteString(conn, "NOT-HTTP\r\n\r\n")
	if response := receiveBounded(t, responses, "response"); response == nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("early invalid head: %v, want 502", response)
	}
	if len(other.recorded()) != 0 || retryCount(serviceName, "error") != 0 {
		t.Errorf("an invalid head was retried")
	}
	if attemptCount(serviceName, "g1", "invalid_header") != 1 {
		t.Errorf("the attempt was counted as %v error, %v invalid_header",
			attemptCount(serviceName, "g1", "error"), attemptCount(serviceName, "g1", "invalid_header"))
	}
}

// A block that stops reading a request body is a timeout once
// ForwardSettings.ProxySendTimeout passes without progress, nginx's
// proxy_send_timeout: D7 moves the request to another block, which gets the
// whole body.
func TestRetrySendTimeoutOnStalledBlock(t *testing.T) {
	settings := testForwardFrontSettings()
	settings.Forward.ProxySendTimeout = 200 * time.Millisecond
	serviceName := testServiceName("retry-send-timeout")
	raw := newRawUpstream(t)
	live := newRecordingUpstream(t, "g2", nil)
	// the first pick of two idle blocks of equal weight is the first
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
		{name: "g2", weight: 1, port: live.port},
	}), settings)
	front := newTestForwardFront(t, group, testForwardOptions)
	type result struct {
		response *http.Response
		body     []byte
		err      error
	}
	results := make(chan result, 1)
	// the body limit, more than loopback buffers hold
	requestBody := testBodyBytes(16 * 1024 * 1024)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, front.url("/upload"), bytes.NewReader(requestBody))
		response, body, err := testDo(t, request)
		results <- result{response: response, body: body, err: err}
	}()
	// g1 takes the head and none of the body
	raw.accept(t)
	var got result
	select {
	case got = <-results:
	case <-time.After(30 * time.Second):
		t.Fatalf("a request write the block stopped reading was never bounded")
	}
	if got.err != nil || got.response.StatusCode != http.StatusOK || string(got.body) != "g2" {
		t.Fatalf("response %v %q %v, want g2's 200", got.response, got.body, got.err)
	}
	if !bytes.Equal(live.recorded()[0].body, requestBody) {
		t.Errorf("g2 got a different body")
	}
	if attemptCount(serviceName, "g1", "timeout") != 1 || retryCount(serviceName, "timeout") != 1 {
		t.Errorf("attempts: g1 timeout %v, retries %v; want the stalled send counted as a timeout and retried",
			attemptCount(serviceName, "g1", "timeout"), retryCount(serviceName, "timeout"))
	}

	// with no other block, the stalled send is the front's 504
	onlyServiceName := testServiceName("send-timeout-only")
	onlyRaw := newRawUpstream(t)
	onlyGroup, _ := newTestGroup(t, testForwardRouteService(onlyServiceName, []testBlock{
		{name: "g1", weight: 1, port: onlyRaw.port},
	}), settings)
	onlyFront := newTestForwardFront(t, onlyGroup, testForwardOptions)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, onlyFront.url("/upload"), bytes.NewReader(requestBody))
		response, body, err := testDo(t, request)
		results <- result{response: response, body: body, err: err}
	}()
	onlyRaw.accept(t)
	got = receiveBounded(t, results, "response")
	if got.err != nil || got.response.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("response %v %v, want 504", got.response, got.err)
	}
	if attemptCount(onlyServiceName, "g1", "timeout") != 1 || forwardErrorCount(onlyServiceName, http.StatusGatewayTimeout) != 1 {
		t.Errorf("want one timed out attempt and one 504")
	}
}

// A client body that signals each read as it starts, so a test knows when
// the front waits on the client.
type signalingBody struct {
	body  io.ReadCloser
	reads chan struct{}
}

// Signals the read, then reads.
func (self *signalingBody) Read(p []byte) (int, error) {
	self.reads <- struct{}{}
	return self.body.Read(p)
}

// Closes the body underneath.
func (self *signalingBody) Close() error {
	return self.body.Close()
}

// A block that fails while a streamed body is still arriving is the block's
// error, a counted 502. Stopping the body fails the client's pending read,
// which Go's server answers by canceling the request; that must not pass for
// the client going away.
func TestRetryStreamedUpstreamFailureIsUpstreamError(t *testing.T) {
	serviceName := testServiceName("streamed-upstream-failure")
	raw := newRawUpstream(t)
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
	}), testForwardFrontSettings())
	bodyReads := make(chan struct{}, 16)
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		// the front's reads of the client's body signal as they start
		r.Body = &signalingBody{body: r.Body, reads: bodyReads}
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
	// the first read took the ten bytes; the second waits on the client
	for range 2 {
		select {
		case <-bodyReads:
		case <-time.After(30 * time.Second):
			t.Fatalf("the front did not read on from the client")
		}
	}
	// the block fails with the body still arriving
	upstreamConn.Close()
	response, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("response %v %v, want 502", response, err)
	}
	front.waitServed(t)
	if attemptCount(serviceName, "g1", "error") != 1 || attemptCount(serviceName, "g1", "client") != 0 || forwardErrorCount(serviceName, http.StatusBadGateway) != 1 {
		t.Errorf("attempts: error %v client %v, front 502s %v; want the block's error, counted",
			attemptCount(serviceName, "g1", "error"), attemptCount(serviceName, "g1", "client"), forwardErrorCount(serviceName, http.StatusBadGateway))
	}
}

// The 503 decision on a streamed body holds the body back from its exchange
// before it decides, so a write poised to begin the body can't begin it
// after the decision: the body never begins on the block that answered 503,
// and stays whole for the retry. A body that began already ends the retries
// and goes on undisturbed.
func TestRetry503HoldsStreamedBodyBeforeDeciding(t *testing.T) {
	raw := newRawUpstream(t)
	group, _ := newTestGroup(t, testForwardRouteService(testServiceName("retry-503-hold"), []testBlock{
		{name: "g1", weight: 1, port: raw.port},
		{name: "g2", weight: 1, port: closedPort(t)},
	}), testForwardFrontSettings())
	dialedConns := make(chan *holdWriteConn, 2)
	wrapGroupDials(group, func(conn net.Conn, address string) net.Conn {
		holdConn := newHoldWriteConn(t, conn)
		dialedConns <- holdConn
		return holdConn
	})
	ctx := testContext(t)
	type exchangeResult struct {
		exchange *upstreamExchange
		failure  upstreamFailure
	}
	// starts a streamed request of 8 bytes on g1, its block answering 503
	// once the test lets it; a body its first read returns whole is kept
	start := func(body io.Reader) (*forwardStreamedBody, *upstreamRequest, *holdWriteConn, chan exchangeResult) {
		streamedBody := newForwardStreamedBody(ctx, body, 8, 0, 0, nil, group.bodyStore)
		request := &upstreamRequest{
			head:         []byte("POST /mcp HTTP/1.1\r\nHost: mcp.service.example\r\nContent-Length: 8\r\n\r\n"),
			method:       http.MethodPost,
			bodyFraming:  forwardBodyFraming{known: true, length: 8},
			streamedBody: streamedBody,
		}
		upstreamConn, err := group.blocks[0].pool.dial(ctx)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		results := make(chan exchangeResult, 1)
		go func() {
			exchange, failure, _ := group.exchange(ctx, upstreamConn, request)
			results <- exchangeResult{exchange: exchange, failure: failure}
		}()
		return streamedBody, request, receiveBounded(t, dialedConns, "dial"), results
	}
	attemptsFor := func(request *upstreamRequest) *forwardAttempts {
		return &forwardAttempts{
			ctx:          ctx,
			request:      request,
			options:      &forwardOptions{},
			attemptLimit: 2,
			attemptCount: 1,
		}
	}
	answer503 := func(conn net.Conn, results chan exchangeResult) *upstreamExchange {
		io.WriteString(conn, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n")
		result := receiveBounded(t, results, "exchange")
		if result.failure != upstreamFailureNone || result.exchange.response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("exchange %v, want the block's 503", result.failure)
		}
		return result.exchange
	}

	// the write is held right after the head, before the body
	streamedBody, request, holdConn, results := start(strings.NewReader("streamed"))
	waitClosed(t, holdConn.held, "the held head write")
	conn, _, _ := raw.accept(t)
	exchange := answer503(conn, results)
	if !attemptsFor(request).retry503Allowed(exchange) {
		t.Fatalf("a 503 before the streamed body began was not retried")
	}
	// the write goes on only after the decision
	holdConn.release()
	if err := exchange.joinWrite(); !errors.Is(err, errBodyInterrupted) {
		t.Errorf("the write ended with %v, want the body held back", err)
	}
	if streamedBody.began() {
		t.Errorf("the body began on the block that answered 503, after the decision to retry it")
	}
	exchange.finish(false)

	// the body began before the 503, its first read still waiting on the
	// client: that read keeps nothing in time
	reader := &gatedReader{chunks: make(chan []byte), waiting: make(chan struct{})}
	streamedBody, request, holdConn, results = start(reader)
	holdConn.release()
	receiveBounded(t, reader.waiting, "the body read")
	conn, _, upstreamRequest := raw.accept(t)
	exchange = answer503(conn, results)
	if attemptsFor(request).retry503Allowed(exchange) {
		t.Errorf("a 503 after the streamed body began was retried")
	}
	reader.chunks <- []byte("streamed")
	readExactly(t, upstreamRequest.Body, "streamed")
	if err := exchange.joinWrite(); err != nil {
		t.Errorf("the begun body's write ended with %v, want it whole", err)
	}
	exchange.finish(false)
	streamedBody.release()

	// the body began, and its first read returned it whole and kept it,
	// before the 503: the 503 is retried, with the kept body
	streamedBody, request, holdConn, results = start(strings.NewReader("streamed"))
	holdConn.release()
	conn, _, upstreamRequest = raw.accept(t)
	readExactly(t, upstreamRequest.Body, "streamed")
	exchange = answer503(conn, results)
	attempts := attemptsFor(request)
	if !attempts.retry503Allowed(exchange) {
		t.Errorf("a 503 after the streamed body was kept whole was not retried")
	}
	exchange.finish(false)
	attempts.next()
	if request.streamedBody != nil || request.storedBody == nil || string(readStoredBody(t, request.storedBody)) != "streamed" {
		t.Errorf("the next attempt doesn't replay the kept body")
	}
	streamedBody.release()
}

// A 503 that comes before a streamed body began going to its block moves the
// request to another block with the whole body, though the first block's
// write is poised right before the body as the 503 is decided on.
func TestRetry503BeforeStreamedBodyBeganMovesItWhole(t *testing.T) {
	serviceName := testServiceName("retry-503-streamed")
	raw := newRawUpstream(t)
	live := newRecordingUpstream(t, "g2", nil)
	group, _ := newTestGroup(t, testForwardRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
		{name: "g2", weight: 1, port: live.port},
	}), testForwardFrontSettings())
	rawAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(raw.port))
	heldConns := make(chan *holdWriteConn, 1)
	wrapGroupDials(group, func(conn net.Conn, address string) net.Conn {
		if address != rawAddress {
			return conn
		}
		holdConn := newHoldWriteConn(t, conn)
		heldConns <- holdConn
		return holdConn
	})
	front := newTestForwardFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		return options
	})
	responses := make(chan string, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, front.url("/mcp"), strings.NewReader("streamed"))
		response, body, err := testDo(t, request)
		if err != nil {
			responses <- err.Error()
			return
		}
		responses <- strconv.Itoa(response.StatusCode) + " " + string(body)
	}()
	holdConn := receiveBounded(t, heldConns, "dial to g1")
	waitClosed(t, holdConn.held, "the held head write")
	conn, _, _ := raw.accept(t)
	// the retry cuts g1's connection, which lets its held write go on
	io.WriteString(conn, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n")
	if response := receiveBounded(t, responses, "response"); response != "200 g2" {
		t.Fatalf("response %q, want g2's 200", response)
	}
	if recorded := live.recorded(); len(recorded) != 1 || string(recorded[0].body) != "streamed" {
		t.Errorf("g2 did not get the whole body")
	}
	if retryCount(serviceName, "http_503") != 1 {
		t.Errorf("the 503 was not retried once")
	}
}
