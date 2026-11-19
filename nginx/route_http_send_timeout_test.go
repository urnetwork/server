package nginx

// Tests of send_timeout as the front applies it (response.go): nginx bounds
// a write to the client only while it is blocked, so the deadline is armed
// around each write and never across a pause. The first tests check where
// the deadline is armed through a writer that records it; the rest drive
// real h1 and h2 servers, where a deadline left armed across a pause cuts
// the response.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A response writer that records the write deadlines set on it, as an h1
// server's writer takes them for its connection.
type testDeadlineResponseWriter struct {
	*httptest.ResponseRecorder
	writeDeadlines []time.Time
}

// See http.ResponseController.
func (self *testDeadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	self.writeDeadlines = append(self.writeDeadlines, deadline)
	return nil
}

// Whether the last deadline set is armed.
func (self *testDeadlineResponseWriter) armed() bool {
	return 0 < len(self.writeDeadlines) && !self.writeDeadlines[len(self.writeDeadlines)-1].IsZero()
}

// A connection that records the write deadlines set on it.
type testDeadlineConn struct {
	net.Conn
	writeDeadlines []time.Time
}

// See net.Conn.
func (self *testDeadlineConn) SetWriteDeadline(deadline time.Time) error {
	self.writeDeadlines = append(self.writeDeadlines, deadline)
	return self.Conn.SetWriteDeadline(deadline)
}

// The deadline is armed just before each write and flush and cleared when
// it returns, so none is armed while the response pauses between writes,
// checked at each pause. The head takes none of its own, and the deadline
// is armed again as the handler returns, for what the server writes then.
// A hijack clears it for the tunnel's owner; a zero SendTimeout sets none.
func TestRouteHttpSendTimeoutBoundsEachWrite(t *testing.T) {
	settings := testFrontSettings()
	settings.Http.SendTimeout = 30 * time.Second
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	writer := &testDeadlineResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
	}
	pauseArmeds := []bool{}
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		w.WriteHeader(http.StatusOK)
		pauseArmeds = append(pauseArmeds, writer.armed())
		for range 3 {
			io.WriteString(w, "chunk")
			pauseArmeds = append(pauseArmeds, writer.armed())
			http.NewResponseController(w).Flush()
			pauseArmeds = append(pauseArmeds, writer.armed())
		}
	}
	handler := testFront.front.HttpsHandler("eth1")

	before := time.Now()
	handler.ServeHTTP(writer, newTestRequest("GET", "api.service.example", "/v1", true))
	after := time.Now()
	if slices.Contains(pauseArmeds, true) {
		t.Errorf("a deadline was armed at the pauses %v, want none", pauseArmeds)
	}
	// an arm and a clear per write and per flush, then the arm as the
	// handler returns
	if len(writer.writeDeadlines) != 2*2*3+1 {
		t.Fatalf("%d write deadlines, want 13: %v", len(writer.writeDeadlines), writer.writeDeadlines)
	}
	for i, deadline := range writer.writeDeadlines {
		if i%2 == 1 {
			if !deadline.IsZero() {
				t.Errorf("deadline %d is %s, want it cleared after its write", i, deadline)
			}
			continue
		}
		if deadline.Before(before.Add(30*time.Second)) || after.Add(30*time.Second).Before(deadline) {
			t.Errorf("deadline %d is %s, want SendTimeout from its write", i, deadline)
		}
	}

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	deadlineConn := &testDeadlineConn{
		Conn: serverConn,
	}
	testFront.connectHandler.serve = func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}
	handler.ServeHTTP(&testHijackResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             deadlineConn,
	}, newTestRequest("GET", "connect.service.example", "/", true))
	if len(deadlineConn.writeDeadlines) != 1 || !deadlineConn.writeDeadlines[0].IsZero() {
		t.Errorf("the hijacked connection's write deadlines %v, want one clear", deadlineConn.writeDeadlines)
	}

	settings.Http.SendTimeout = 0
	writer = &testDeadlineResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
	}
	handler.ServeHTTP(writer, newTestRequest("GET", "api.service.example", "/v1", true))
	if len(writer.writeDeadlines) != 0 {
		t.Errorf("SendTimeout 0 set %d write deadlines", len(writer.writeDeadlines))
	}
}

// On h1, where Go's server writes a 100 Continue at the first read of the
// body, that read arms the deadline, and nothing arms one before it: a
// deadline armed as the request arrived would have passed when a late read
// writes the 100 Continue. Later reads arm no more, and a bodiless request
// gets none. h2, whose server queues its 100 Continue without blocking the
// read, arms none for it. An h2 stream already closed when the handler
// returns gets no deadline for the server's final writes.
func TestRouteHttpSendTimeoutArmsForTheContinueAndTheEnd(t *testing.T) {
	settings := testFrontSettings()
	settings.Http.SendTimeout = 30 * time.Second
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	var writer *testDeadlineResponseWriter
	// the deadlines set before the first read, and after each read
	deadlineCounts := []int{}
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		deadlineCounts = append(deadlineCounts, len(writer.writeDeadlines))
		buffer := make([]byte, 2)
		for {
			_, err := r.Body.Read(buffer)
			deadlineCounts = append(deadlineCounts, len(writer.writeDeadlines))
			if err != nil {
				return
			}
		}
	}
	handler := testFront.front.HttpsHandler("eth1")
	closedCtx, cancel := context.WithCancel(t.Context())
	cancel()

	cases := []struct {
		name           string
		protoMajor     int
		body           string
		ctx            context.Context
		deadlineCounts []int
		// every one set, all armed
		writeDeadlineCount int
	}{
		// the first read arms; the handler's return arms again
		{name: "h1 with a body", protoMajor: 1, body: "abcd", ctx: t.Context(), deadlineCounts: []int{0, 1, 1, 1}, writeDeadlineCount: 2},
		{name: "h1 without a body", protoMajor: 1, body: "", ctx: t.Context(), deadlineCounts: []int{0, 0}, writeDeadlineCount: 1},
		{name: "h2 with a body", protoMajor: 2, body: "abcd", ctx: t.Context(), deadlineCounts: []int{0, 0, 0, 0}, writeDeadlineCount: 1},
		{name: "h2, closed", protoMajor: 2, body: "abcd", ctx: closedCtx, deadlineCounts: []int{0, 0, 0, 0}, writeDeadlineCount: 0},
	}
	for _, c := range cases {
		writer = &testDeadlineResponseWriter{
			ResponseRecorder: httptest.NewRecorder(),
		}
		deadlineCounts = []int{}
		r := newTestRequest("POST", "api.service.example", "/v1", true)
		if c.protoMajor == 2 {
			r = asTestProtocol(r, 2)
		}
		if c.body != "" {
			r.Body = io.NopCloser(strings.NewReader(c.body))
			r.ContentLength = int64(len(c.body))
		}
		handler.ServeHTTP(writer, r.WithContext(c.ctx))
		if !slices.Equal(deadlineCounts, c.deadlineCounts) {
			t.Errorf("%s: deadlines before the first read and after each %v, want %v", c.name, deadlineCounts, c.deadlineCounts)
		}
		if len(writer.writeDeadlines) != c.writeDeadlineCount {
			t.Errorf("%s: %d write deadlines %v, want %d", c.name, len(writer.writeDeadlines), writer.writeDeadlines, c.writeDeadlineCount)
		}
		for _, deadline := range writer.writeDeadlines {
			if deadline.IsZero() {
				t.Errorf("%s: a deadline was cleared, with no write to clear it after", c.name)
			}
		}
	}
}

// A real TLS server of the front's TCP 443 handler, h2 when enableHttp2 is
// set, with a client for it. It closes when the test ends.
func newTestFrontServer(t *testing.T, testFront *testFront, enableHttp2 bool) (*httptest.Server, *http.Transport) {
	t.Helper()
	server := httptest.NewUnstartedServer(testFront.front.HttpsHandler("eth1"))
	server.EnableHTTP2 = enableHttp2
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, server.Client().Transport.(*http.Transport).Clone()
}

// A request to a test server for a name the front serves.
func newTestServerRequest(t *testing.T, server *httptest.Server, method string, host string, target string, body io.Reader) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, server.URL+target, body)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = host
	return r
}

// Closes a channel once, however often it is asked to.
type testOnceClose struct {
	channel chan struct{}
	once    sync.Once
}

// A new open channel.
func newTestOnceClose() *testOnceClose {
	return &testOnceClose{
		channel: make(chan struct{}),
	}
}

// Closes the channel, the first time only.
func (self *testOnceClose) close() {
	self.once.Do(func() {
		close(self.channel)
	})
}

// Over real h1 and h2 servers, a response that goes quiet for longer than
// SendTimeout, between two writes and again before it ends, while the
// client reads all it is sent, completes, as under nginx, whose
// send_timeout runs only while a write is blocked. A deadline left armed
// across a pause cuts it: Go's h2 server resets a stream whose deadline
// passes with no write pending, and its h1 server writes the end of the
// response after the handler returns. Each pause is the condition under
// test, ten SendTimeouts long, started once the client has read what came
// before it.
func TestRouteHttpSendTimeoutLetsAQuietResponseLive(t *testing.T) {
	const sendTimeout = 25 * time.Millisecond
	const pause = 10 * sendTimeout
	for _, enableHttp2 := range []bool{false, true} {
		settings := testFrontSettings()
		settings.Http.SendTimeout = sendTimeout
		testFront := newTestFront(t, loadTestRouteTable(t), settings)
		resumes := []*testOnceClose{newTestOnceClose(), newTestOnceClose()}
		for _, resume := range resumes {
			defer resume.close()
		}
		testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
			w.Header().Set("Content-Type", "text/event-stream")
			for i, chunk := range []string{"a", "b"} {
				io.WriteString(w, chunk)
				http.NewResponseController(w).Flush()
				<-resumes[i].channel
			}
		}
		server, transport := newTestFrontServer(t, testFront, enableHttp2)
		client := &http.Client{
			Transport: transport,
		}

		response, err := client.Do(newTestServerRequest(t, server, "GET", "api.service.example", "/events", nil))
		if err != nil {
			t.Fatalf("h2 %t: %s", enableHttp2, err)
		}
		defer response.Body.Close()
		wantProtoMajor := 1
		if enableHttp2 {
			wantProtoMajor = 2
		}
		if response.ProtoMajor != wantProtoMajor {
			t.Fatalf("the response came over HTTP/%d, want %d", response.ProtoMajor, wantProtoMajor)
		}
		body := []byte{}
		for _, resume := range resumes {
			chunk := make([]byte, 1)
			if _, err := io.ReadFull(response.Body, chunk); err != nil {
				t.Fatalf("HTTP/%d: after %q: %s", response.ProtoMajor, body, err)
			}
			body = append(body, chunk...)
			time.Sleep(pause)
			resume.close()
		}
		rest, err := io.ReadAll(response.Body)
		if err != nil || string(body)+string(rest) != "ab" {
			t.Errorf("HTTP/%d: body %q then %q, %v, want \"ab\" and its end", response.ProtoMajor, body, rest, err)
		}
	}
}

// Over real h1 and h2 servers, a client that stops reading has the blocked
// write cut once it has waited SendTimeout: on h2 when the stream's
// flow-control window runs out, on h1 when the connection's buffers fill.
// The handler's write fails, no sooner than SendTimeout after it started.
// h1 runs without TLS: over TLS, Go's h1 server closes the connection when
// the write fails, and crypto/tls then spends up to 5 s more sending
// close_notify to the client that isn't reading before the write returns.
func TestRouteHttpSendTimeoutCutsAWriteTheClientStopsReading(t *testing.T) {
	const sendTimeout = 100 * time.Millisecond
	for _, enableHttp2 := range []bool{false, true} {
		settings := testFrontSettings()
		settings.Http.SendTimeout = sendTimeout
		testFront := newTestFront(t, loadTestRouteTable(t), settings)
		type cut struct {
			err     error
			blocked time.Duration
		}
		cuts := make(chan cut, 1)
		testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
			w.WriteHeader(http.StatusOK)
			http.NewResponseController(w).Flush()
			chunk := make([]byte, 16*1024)
			// at most 1 GiB, far past any buffer, so a write never cut
			// still ends
			for range 64 * 1024 {
				writeStart := time.Now()
				if _, err := w.Write(chunk); err != nil {
					cuts <- cut{
						err:     err,
						blocked: time.Since(writeStart),
					}
					return
				}
			}
			cuts <- cut{}
		}
		var server *httptest.Server
		var transport *http.Transport
		if enableHttp2 {
			server, transport = newTestFrontServer(t, testFront, true)
			// the stream's window, so a write blocks after 16 KiB unread
			transport.HTTP2 = &http.HTTP2Config{
				MaxReceiveBufferPerStream: 16 * 1024,
			}
		} else {
			server = httptest.NewServer(testFront.front.HttpsHandler("eth1"))
			t.Cleanup(server.Close)
			transport = server.Client().Transport.(*http.Transport).Clone()
		}
		client := &http.Client{
			Transport: transport,
		}

		response, err := client.Do(newTestServerRequest(t, server, "GET", "api.service.example", "/download", nil))
		if err != nil {
			t.Fatalf("h2 %t: %s", enableHttp2, err)
		}
		// the body is never read; closing it on the way out ends a write
		// that was never cut
		defer response.Body.Close()
		select {
		case cut := <-cuts:
			if cut.err == nil {
				t.Fatalf("HTTP/%d: 1 GiB was written to a client that reads nothing", response.ProtoMajor)
			}
			if cut.blocked < sendTimeout {
				t.Errorf("HTTP/%d: the write was cut after %s, want at least SendTimeout %s", response.ProtoMajor, cut.blocked, sendTimeout)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("HTTP/%d: a write the client never reads was not cut", response.ProtoMajor)
		}
	}
}

// Over a real h1 server, a request that expects 100 Continue, whose body is
// first read more than SendTimeout after it arrived, still gets its 100
// Continue, as under nginx: Go's server writes it at that first read, which
// arms the deadline then. The wait before the read is the condition under
// test, ten SendTimeouts long, started once the request has reached its
// forwarder.
func TestRouteHttpSendTimeoutLetsALateBodyReadContinue(t *testing.T) {
	const sendTimeout = 25 * time.Millisecond
	settings := testFrontSettings()
	settings.Http.SendTimeout = sendTimeout
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	arrived := make(chan struct{})
	resume := newTestOnceClose()
	defer resume.close()
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		close(arrived)
		<-resume.channel
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		io.WriteString(w, "read "+string(body))
	}
	server, transport := newTestFrontServer(t, testFront, false)
	// the client sends the body only once the 100 Continue arrives
	transport.ExpectContinueTimeout = 5 * time.Second
	client := &http.Client{
		Transport: transport,
	}

	type result struct {
		statusCode int
		body       string
		err        error
	}
	results := make(chan result, 1)
	r := newTestServerRequest(t, server, "POST", "api.service.example", "/v1", strings.NewReader("hello"))
	r.Header.Set("Expect", "100-continue")
	go func() {
		response, err := client.Do(r)
		if err != nil {
			results <- result{
				err: err,
			}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		results <- result{
			statusCode: response.StatusCode,
			body:       string(body),
			err:        err,
		}
	}()
	waitTestBarrier(t, arrived)
	time.Sleep(10 * sendTimeout)
	resume.close()
	lateRead := <-results
	if lateRead.err != nil || lateRead.statusCode != http.StatusOK || lateRead.body != "read hello" {
		t.Fatalf("a late body read = %d %q, %v, want 200 \"read hello\"", lateRead.statusCode, lateRead.body, lateRead.err)
	}
}

// Over a real h2 server, what the server writes once the handler returns is
// bounded too. A handler leaves part of its body buffered for a client
// whose stream window is full and which reads nothing: the server's final
// write has waited SendTimeout when the stream is reset, so the client,
// reading at last, gets an error instead of the rest. The wait is the
// condition under test, ten SendTimeouts long, started once the handler has
// returned.
func TestRouteHttpSendTimeoutBoundsTheFinalWrite(t *testing.T) {
	const sendTimeout = 25 * time.Millisecond
	settings := testFrontSettings()
	settings.Http.SendTimeout = sendTimeout
	testFront := newTestFront(t, loadTestRouteTable(t), settings)
	returned := make(chan struct{})
	testFront.forwarders["api"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		// the client's whole window, then a little the server holds until
		// the handler returns
		w.Write(make([]byte, 16*1024))
		http.NewResponseController(w).Flush()
		w.Write(make([]byte, 1024))
		close(returned)
	}
	server, transport := newTestFrontServer(t, testFront, true)
	transport.HTTP2 = &http.HTTP2Config{
		MaxReceiveBufferPerStream: 16 * 1024,
	}
	client := &http.Client{
		Transport: transport,
	}

	response, err := client.Do(newTestServerRequest(t, server, "GET", "api.service.example", "/download", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 {
		t.Fatalf("the response came over HTTP/%d, want 2", response.ProtoMajor)
	}
	waitTestBarrier(t, returned)
	time.Sleep(10 * sendTimeout)
	body, err := io.ReadAll(response.Body)
	if err == nil {
		t.Fatalf("read all %d bytes: the server's final write was never cut", len(body))
	}
}
