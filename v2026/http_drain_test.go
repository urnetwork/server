package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// testListenAddr reserves an ephemeral port and returns it as an addr for
// HttpListenAndServeWithReusePort. The listener is closed before returning,
// so there is a small reuse race; acceptable for tests.
func testListenAddr(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %s", err)
	}
	addr := listener.Addr().String()
	listener.Close()
	return addr
}

// waitForServe polls url until the server answers or the deadline passes.
func waitForServe(t *testing.T, client *http.Client, url string) {
	for i := 0; i < 100; i += 1 {
		response, err := client.Get(url)
		if err == nil {
			response.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server did not come up at %s", url)
}

// TestHttpDrainGraceKeepaliveRetire: during the drain grace the server keeps
// serving and stamps `Connection: close` on responses, so a pooled keepalive
// connection is retired cleanly after its next request; the serve call
// returns nil (clean drain) after the grace.
func TestHttpDrainGraceKeepaliveRetire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := testListenAddr(t)
	url := fmt.Sprintf("http://%s/hello", addr)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello")
	})

	serveErrs := make(chan error, 1)
	go func() {
		serveErrs <- HttpListenAndServeWithReusePort(
			ctx,
			addr,
			handler,
			false,
			HttpServerOptions{
				ReadTimeout:           5 * time.Second,
				WriteTimeout:          5 * time.Second,
				IdleTimeout:           time.Minute,
				ShutdownTimeout:       5 * time.Second,
				KeepaliveDrainTimeout: 2 * time.Second,
			},
		)
	}()

	// a keepalive client so the second request reuses the pooled connection
	client := &http.Client{
		Transport: &http.Transport{},
		Timeout:   5 * time.Second,
	}
	defer client.CloseIdleConnections()

	waitForServe(t, client, url)

	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("get: %s", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(body) != "hello" {
		t.Fatalf("bad response: %d %s", response.StatusCode, body)
	}
	// the go client strips the hop-by-hop Connection header from
	// response.Header; response.Close reflects whether it was on the wire
	if response.Close {
		t.Fatalf("connection close stamped before drain")
	}

	drainStartTime := time.Now()
	cancel()
	// let the drain flag flip
	time.Sleep(300 * time.Millisecond)

	if draining := testutil.ToFloat64(httpServerDrainingGauge); draining != 1 {
		t.Fatalf("draining gauge = %f during grace", draining)
	}

	// the server still serves during the grace, now stamping close
	response, err = client.Get(url)
	if err != nil {
		t.Fatalf("get during grace: %s", err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(body) != "hello" {
		t.Fatalf("bad response during grace: %d %s", response.StatusCode, body)
	}
	if !response.Close {
		t.Fatalf("expected Connection: close on the wire during grace")
	}

	select {
	case err := <-serveErrs:
		if err != nil {
			t.Fatalf("drain returned error: %s", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("drain did not complete")
	}

	drainDuration := time.Now().Sub(drainStartTime)
	if drainDuration < 2*time.Second {
		t.Fatalf("drain returned before the grace elapsed (%s)", drainDuration)
	}

	if cut := testutil.ToFloat64(httpServerDrainCutGauge); cut != 0 {
		t.Fatalf("cut gauge = %f after clean drain", cut)
	}

	// the server is down now
	if _, err := client.Get(url); err == nil {
		t.Fatalf("server still serving after drain")
	}
}

// TestHttpDrainInflightCompletes: with no grace configured (previous
// behavior), a request mid-handler at cancel runs to completion and the
// drain is clean.
func TestHttpDrainInflightCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := testListenAddr(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(1 * time.Second)
			fmt.Fprintf(w, "done")
			return
		}
		fmt.Fprintf(w, "ok")
	})

	serveErrs := make(chan error, 1)
	go func() {
		serveErrs <- HttpListenAndServeWithReusePort(
			ctx,
			addr,
			handler,
			false,
			HttpServerOptions{
				ReadTimeout:     5 * time.Second,
				WriteTimeout:    5 * time.Second,
				IdleTimeout:     time.Minute,
				ShutdownTimeout: 5 * time.Second,
				// grace off: the plain shutdown path
				KeepaliveDrainTimeout: 0,
			},
		)
	}()

	client := &http.Client{
		Transport: &http.Transport{},
		Timeout:   10 * time.Second,
	}
	defer client.CloseIdleConnections()

	waitForServe(t, client, fmt.Sprintf("http://%s/", addr))

	type slowResult struct {
		statusCode int
		body       string
		err        error
	}
	slowResults := make(chan *slowResult, 1)
	go func() {
		response, err := client.Get(fmt.Sprintf("http://%s/slow", addr))
		if err != nil {
			slowResults <- &slowResult{err: err}
			return
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		slowResults <- &slowResult{
			statusCode: response.StatusCode,
			body:       string(body),
		}
	}()

	// cancel while the slow request is mid-handler
	time.Sleep(300 * time.Millisecond)
	drainStartTime := time.Now()
	cancel()

	result := <-slowResults
	if result.err != nil {
		t.Fatalf("in-flight request failed across drain: %s", result.err)
	}
	if result.statusCode != 200 || result.body != "done" {
		t.Fatalf("in-flight request bad response: %d %q", result.statusCode, result.body)
	}

	select {
	case err := <-serveErrs:
		if err != nil {
			t.Fatalf("drain returned error: %s", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("drain did not complete")
	}

	// the drain returned when the in-flight request finished (~700ms), not at
	// the 5s deadline
	drainDuration := time.Now().Sub(drainStartTime)
	if 3*time.Second < drainDuration {
		t.Fatalf("drain idled toward the deadline (%s)", drainDuration)
	}

	if cut := testutil.ToFloat64(httpServerDrainCutGauge); cut != 0 {
		t.Fatalf("cut gauge = %f after clean drain", cut)
	}
}

// TestHttpDrainDeadlineCut: a connection that outlives ShutdownTimeout is
// reported as cut with a typed error instead of a bare deadline error.
func TestHttpDrainDeadlineCut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := testListenAddr(t)

	blockRelease := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/block" {
			<-blockRelease
			fmt.Fprintf(w, "unblocked")
			return
		}
		fmt.Fprintf(w, "ok")
	})

	serveErrs := make(chan error, 1)
	go func() {
		serveErrs <- HttpListenAndServeWithReusePort(
			ctx,
			addr,
			handler,
			false,
			HttpServerOptions{
				ReadTimeout:           5 * time.Second,
				WriteTimeout:          5 * time.Second,
				IdleTimeout:           time.Minute,
				ShutdownTimeout:       500 * time.Millisecond,
				KeepaliveDrainTimeout: 0,
			},
		)
	}()

	client := &http.Client{
		Transport: &http.Transport{},
		Timeout:   30 * time.Second,
	}
	defer client.CloseIdleConnections()

	waitForServe(t, client, fmt.Sprintf("http://%s/", addr))

	blockedDone := make(chan struct{})
	go func() {
		defer close(blockedDone)
		// the response may complete or fail depending on when the process
		// would have exited; only the drain accounting is asserted
		response, err := client.Get(fmt.Sprintf("http://%s/block", addr))
		if err == nil {
			io.ReadAll(response.Body)
			response.Body.Close()
		}
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-serveErrs:
		var drainCut *HttpDrainCutError
		if !errors.As(err, &drainCut) {
			t.Fatalf("expected HttpDrainCutError, got %v", err)
		}
		if drainCut.CutConnectionCount < 1 {
			t.Fatalf("expected at least 1 cut connection, got %d", drainCut.CutConnectionCount)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("drain did not complete")
	}

	if cut := testutil.ToFloat64(httpServerDrainCutGauge); cut < 1 {
		t.Fatalf("cut gauge = %f after deadline cut", cut)
	}

	// release the blocked handler and drain the client goroutine
	close(blockRelease)
	select {
	case <-blockedDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("blocked client did not finish")
	}
}

// TestHttpConnTracker: ConnState transitions produce exact open/active counts.
func TestHttpConnTracker(t *testing.T) {
	tracker := newHttpConnTracker()

	newTestConn := func() net.Conn {
		client, server := net.Pipe()
		client.Close()
		return server
	}

	assertCounts := func(expectedOpen int64, expectedActive int64) {
		t.Helper()
		open, active := tracker.counts()
		if open != expectedOpen || active != expectedActive {
			t.Fatalf("counts = (open %d, active %d), expected (open %d, active %d)",
				open, active, expectedOpen, expectedActive)
		}
	}

	connA := newTestConn()
	connB := newTestConn()
	connC := newTestConn()

	tracker.connState(connA, http.StateNew)
	tracker.connState(connB, http.StateNew)
	assertCounts(2, 0)

	tracker.connState(connA, http.StateActive)
	assertCounts(2, 1)

	// re-entering active does not double count
	tracker.connState(connA, http.StateActive)
	assertCounts(2, 1)

	tracker.connState(connA, http.StateIdle)
	assertCounts(2, 0)

	tracker.connState(connA, http.StateActive)
	tracker.connState(connB, http.StateActive)
	assertCounts(2, 2)

	// closing an active connection releases both counts
	tracker.connState(connA, http.StateClosed)
	assertCounts(1, 1)

	// hijack releases tracking (the conn no longer belongs to the server)
	tracker.connState(connB, http.StateHijacked)
	assertCounts(0, 0)

	// an unseen conn straight to active is tracked defensively
	tracker.connState(connC, http.StateActive)
	assertCounts(1, 1)
	tracker.connState(connC, http.StateClosed)
	assertCounts(0, 0)

	// closing an unseen conn is a no-op
	tracker.connState(newTestConn(), http.StateClosed)
	assertCounts(0, 0)
}

// TestDrainHandlerStamp: the drain handler stamps `Connection: close` only
// while draining and only on http/1 requests.
func TestDrainHandlerStamp(t *testing.T) {
	handler := newDrainHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	get := func(protoMajor int) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://test/", nil)
		r.ProtoMajor = protoMajor
		handler.ServeHTTP(w, r)
		return w
	}

	if connection := get(1).Header().Get("Connection"); connection != "" {
		t.Fatalf("stamped before draining: %q", connection)
	}

	handler.draining.Store(true)

	if connection := get(1).Header().Get("Connection"); connection != "close" {
		t.Fatalf("expected close stamp while draining, got %q", connection)
	}

	// connection headers are not valid in http/2; never stamp
	if connection := get(2).Header().Get("Connection"); connection != "" {
		t.Fatalf("stamped an http/2 response: %q", connection)
	}
}
