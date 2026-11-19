package nginx

// Tests of D7's retries as the review amended them: which failures move a
// request to another block, which pass through or end it, how the final
// failure maps to a status, and that every physical attempt is counted,
// a stale keepalive connection's redo included.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
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
	group, _ := newTestGroup(t, testRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: closedPort(t)},
		{name: "g2", weight: 1, port: live.port},
	}), testFrontSettings())
	front := newTestFront(t, group, testForwardOptions)
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
	group, bodyStore := newTestGroup(t, testRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: busy.port},
		{name: "g2", weight: 1, port: live.port},
	}), testFrontSettings())
	front := newTestFront(t, group, testForwardOptions)
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
	finalGroup, _ := newTestGroup(t, testRouteService(finalServiceName, []testBlock{
		{name: "g1", weight: 1, port: first.port},
		{name: "g2", weight: 1, port: second.port},
	}), testFrontSettings())
	finalFront := newTestFront(t, finalGroup, testForwardOptions)
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
	singleGroup, _ := newTestGroup(t, testRouteService(singleServiceName, []testBlock{
		{name: "g1", weight: 1, port: only.port},
	}), testFrontSettings())
	singleFront := newTestFront(t, singleGroup, testForwardOptions)
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
		group, _ := newTestGroup(t, testRouteService(serviceName, []testBlock{
			{name: "g1", weight: 1, port: failing.port},
			{name: "g2", weight: 1, port: other.port},
		}), testFrontSettings())
		front := newTestFront(t, group, testForwardOptions)
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
	group, _ := newTestGroup(t, testRouteService("retry-partial", []testBlock{
		{name: "g1", weight: 1, port: raw.port},
		{name: "g2", weight: 1, port: other.port},
	}), testFrontSettings())
	front := newTestFront(t, group, testForwardOptions)
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
	got := <-results
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
	group, _ := newTestGroup(t, testRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: closedPort(t)},
		{name: "g2", weight: 1, port: busy.port},
		{name: "g3", weight: 1, port: other.port},
	}), testFrontSettings())
	front := newTestFront(t, group, func(r *http.Request) forwardOptions {
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

// A streamed body whose request began going upstream is never retried; one
// whose connect was refused, before anything was sent, is.
func TestRetryStreamedOnlyBeforeSending(t *testing.T) {
	streamedOptions := func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.streamed = true
		return options
	}

	raw := newRawUpstream(t)
	other := newRecordingUpstream(t, "g2", nil)
	sentServiceName := testServiceName("retry-streamed-sent")
	group, _ := newTestGroup(t, testRouteService(sentServiceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
		{name: "g2", weight: 1, port: other.port},
	}), testFrontSettings())
	front := newTestFront(t, group, streamedOptions)
	responses := make(chan *http.Response, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, front.url("/mcp"), strings.NewReader("streamed"))
		response, _, _ := testDo(t, request)
		responses <- response
	}()
	conn, _, request := raw.accept(t)
	body, _ := io.ReadAll(io.LimitReader(request.Body, 8))
	if string(body) != "streamed" || request.Header.Get(urRequestBufferingHeader) != "off" {
		t.Fatalf("first block got %q, buffering %q", body, request.Header.Get(urRequestBufferingHeader))
	}
	// the block read the request, then closed without answering
	conn.Close()
	if response := <-responses; response == nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("sent streamed request: %v, want 502", response)
	}
	if len(other.recorded()) != 0 {
		t.Errorf("a streamed body was retried after it was sent")
	}

	refusedServiceName := testServiceName("retry-streamed-refused")
	live := newRecordingUpstream(t, "g2", nil)
	refusedGroup, _ := newTestGroup(t, testRouteService(refusedServiceName, []testBlock{
		{name: "g1", weight: 1, port: closedPort(t)},
		{name: "g2", weight: 1, port: live.port},
	}), testFrontSettings())
	refusedFront := newTestFront(t, refusedGroup, streamedOptions)
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
	group, _ := newTestGroup(t, testRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: upstream.port},
	}), testFrontSettings())
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
	front := newTestFront(t, group, testForwardOptions)

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
}

// With no attempt left, a connect error is a 502 and a timeout a 504, as
// nginx maps them; each is counted as the front's own error.
func TestRetryFinalFailureMapping(t *testing.T) {
	refusedServiceName := testServiceName("final-refused")
	refusedGroup, _ := newTestGroup(t, testRouteService(refusedServiceName, []testBlock{
		{name: "g1", weight: 1, port: closedPort(t)},
		{name: "g2", weight: 1, port: closedPort(t)},
	}), testFrontSettings())
	refusedFront := newTestFront(t, refusedGroup, testForwardOptions)
	request, _ := http.NewRequest(http.MethodGet, refusedFront.url("/"), nil)
	if response, _, err := testDo(t, request); err != nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("refused blocks: %v %v, want 502", response, err)
	}
	if attemptCount(refusedServiceName, "g1", "error")+attemptCount(refusedServiceName, "g2", "error") != 2 || forwardErrorCount(refusedServiceName, http.StatusBadGateway) != 1 {
		t.Errorf("want two failed attempts and one 502")
	}

	// a block that accepts and never answers times out its read
	settings := testFrontSettings()
	settings.Forward.ProxyReadTimeout = 50 * time.Millisecond
	raw := newRawUpstream(t)
	timeoutServiceName := testServiceName("final-timeout")
	timeoutGroup, _ := newTestGroup(t, testRouteService(timeoutServiceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
	}), settings)
	timeoutFront := newTestFront(t, timeoutGroup, testForwardOptions)
	request, _ = http.NewRequest(http.MethodGet, timeoutFront.url("/"), nil)
	if response, _, err := testDo(t, request); err != nil || response.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("silent block: %v %v, want 504", response, err)
	}
	if attemptCount(timeoutServiceName, "g1", "timeout") != 1 || forwardErrorCount(timeoutServiceName, http.StatusGatewayTimeout) != 1 {
		t.Errorf("want one timed out attempt and one 504")
	}

	// a dial past the connect timeout
	dialTimeoutServiceName := testServiceName("final-dial-timeout")
	dialTimeoutGroup, _ := newTestGroup(t, testRouteService(dialTimeoutServiceName, []testBlock{
		{name: "g1", weight: 1, port: closedPort(t)},
	}), testFrontSettings())
	dialTimeoutGroup.dialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: network, Err: dialTimeoutError{}}
	}
	dialTimeoutFront := newTestFront(t, dialTimeoutGroup, testForwardOptions)
	request, _ = http.NewRequest(http.MethodGet, dialTimeoutFront.url("/"), nil)
	if response, _, err := testDo(t, request); err != nil || response.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("dial timeout: %v %v, want 504", response, err)
	}
}

// A dial error that reports a timeout, as net.Dialer's does.
type dialTimeoutError struct{}

// The text net.Dialer's timeout carries.
func (dialTimeoutError) Error() string { return "i/o timeout" }

// Always a timeout.
func (dialTimeoutError) Timeout() bool { return true }

// Always temporary, as a timeout is.
func (dialTimeoutError) Temporary() bool { return true }

// A timeout moves the request to another block; when that retry finds no
// eligible block, the answer is nginx's "no live upstreams" 502, not the
// timeout's 504.
func TestRetryAfterTimeoutWithNoLiveBlockIs502(t *testing.T) {
	settings := testFrontSettings()
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
	group, _ := newTestGroup(t, testRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
		{name: "g2", weight: 2, port: busy.port},
	}), settings)
	front := newTestFront(t, group, func(r *http.Request) forwardOptions {
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
	if held := <-arrivals; held.requestUri != "/held" {
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
	group, _ := newTestGroup(t, testRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: draining.port},
		{name: "g2", weight: 1, port: live.port},
	}), testFrontSettings())
	front := newTestFront(t, group, func(r *http.Request) forwardOptions {
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
	group, _ := newTestGroup(t, testRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: raw.port},
		{name: "g2", weight: 1, port: other.port},
	}), testFrontSettings())
	front := newTestFront(t, group, testForwardOptions)
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
	if response := <-responses; response == nil || response.StatusCode != http.StatusBadGateway {
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
