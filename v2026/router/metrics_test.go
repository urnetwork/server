package router

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRouterMetricsUseConfiguredRouteAndExactIo(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newHttpMetrics(registry)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	route := NewRoute(http.MethodPost, "/objects/([^/]+)", func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "abc" {
			t.Fatalf("body = %q, want abc", body)
		}
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte("hello"))
	})
	router := NewRouter(ctx, []*Route{route})
	router.metrics = metrics

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/objects/caller-controlled", strings.NewReader("abc"))
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Body.String() != "hello" {
		t.Fatalf("response = %d %q, want 201 hello", response.Code, response.Body.String())
	}
	if got := testutil.ToFloat64(metrics.requests.WithLabelValues(route.id, "201", "completed")); got != 1 {
		t.Fatalf("completed requests = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.requestBytes.WithLabelValues(route.id)); got != 3 {
		t.Fatalf("request bytes = %v, want 3", got)
	}
	if got := testutil.ToFloat64(metrics.responseBytes.WithLabelValues(route.id)); got != 5 {
		t.Fatalf("response bytes = %v, want 5", got)
	}
	if got := testutil.ToFloat64(metrics.inflight.WithLabelValues(route.id)); got != 0 {
		t.Fatalf("inflight = %v, want 0", got)
	}
	if strings.Contains(route.id, "caller-controlled") {
		t.Fatalf("metric route leaked request path: %q", route.id)
	}
}

func TestRouterMetricsClassifyUnmatchedPanicCancelAndAbort(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newHttpMetrics(registry)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	canceledRoute := NewRoute(http.MethodGet, "/canceled", func(http.ResponseWriter, *http.Request) {
		panic(context.Canceled)
	})
	panicRoute := NewRoute(http.MethodGet, "/panic", func(http.ResponseWriter, *http.Request) {
		panic("synthetic failure")
	})
	abortRoute := NewRoute(http.MethodGet, "/abort", func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})
	router := NewRouter(ctx, []*Route{canceledRoute, panicRoute, abortRoute})
	router.metrics = metrics

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", nil))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/panic", nil))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/canceled", nil))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("abort panic = %v, want http.ErrAbortHandler", recovered)
			}
		}()
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/abort", nil))
	}()

	checks := []struct {
		route   string
		status  string
		outcome string
	}{
		{route: "not_found", status: "404", outcome: "not_found"},
		{route: "method_not_allowed", status: "405", outcome: "method_not_allowed"},
		{route: canceledRoute.id, status: "none", outcome: "canceled"},
		{route: panicRoute.id, status: "500", outcome: "panic"},
		{route: abortRoute.id, status: "none", outcome: "aborted"},
	}
	for _, check := range checks {
		if got := testutil.ToFloat64(metrics.requests.WithLabelValues(check.route, check.status, check.outcome)); got != 1 {
			t.Errorf("%s/%s/%s requests = %v, want 1", check.route, check.status, check.outcome, got)
		}
	}
}

func TestHttpIntervalMaximumCarriesItsOwnFreshness(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newHttpMetrics(registry)
	now := time.Unix(2_000_000_000, 0)
	metrics.now = func() time.Time { return now }
	metrics.observe("GET ^/synthetic$", "200", "completed", 2*time.Second, 0, 0)
	metrics.observe("GET ^/synthetic$", "200", "completed", time.Second, 0, 0)

	metrics.intervalMaxStateLock.Lock()
	first := metrics.intervalMaxByRoute["GET ^/synthetic$"]
	metrics.intervalMaxStateLock.Unlock()
	if first.seconds != 2 || !first.observedAt.Equal(now) {
		t.Fatalf("first interval max = %+v, want 2s at %v", first, now)
	}

	now = now.Add(httpMetricsInterval)
	metrics.observe("GET ^/synthetic$", "200", "completed", 250*time.Millisecond, 0, 0)
	metrics.intervalMaxStateLock.Lock()
	second := metrics.intervalMaxByRoute["GET ^/synthetic$"]
	metrics.intervalMaxStateLock.Unlock()
	if second.seconds != 0.25 || !second.observedAt.Equal(now) {
		t.Fatalf("second interval max = %+v, want reset 0.25s at %v", second, now)
	}
}

type metricsAllCapabilitiesWriter struct {
	*httptest.ResponseRecorder
	flushed bool
	pushed  bool
	read    bool
}

type metricsPlainWriter struct {
	header http.Header
}

func (self *metricsPlainWriter) Header() http.Header { return self.header }

func (*metricsPlainWriter) Write(buffer []byte) (int, error) { return len(buffer), nil }

func (*metricsPlainWriter) WriteHeader(int) {}

func (self *metricsAllCapabilitiesWriter) Flush() { self.flushed = true }

func (self *metricsAllCapabilitiesWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	server, client := net.Pipe()
	_ = client.Close()
	return server, bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server)), nil
}

func (self *metricsAllCapabilitiesWriter) Push(string, *http.PushOptions) error {
	self.pushed = true
	return nil
}

func (self *metricsAllCapabilitiesWriter) ReadFrom(reader io.Reader) (int64, error) {
	self.read = true
	return io.Copy(self.ResponseRecorder, reader)
}

func TestRequestMetricsWriterPreservesOptionalCapabilitiesAndUnwrap(t *testing.T) {
	plain := &metricsPlainWriter{header: http.Header{}}
	plainBase := &requestMetricsWriter{ResponseWriter: plain}
	plainWrapped := wrapRequestMetricsWriter(plainBase)
	if _, ok := plainWrapped.(http.Flusher); ok {
		t.Fatal("plain writer falsely advertised Flusher")
	}
	if _, ok := plainWrapped.(http.Hijacker); ok {
		t.Fatal("plain writer falsely advertised Hijacker")
	}
	if _, ok := plainWrapped.(http.Pusher); ok {
		t.Fatal("plain writer falsely advertised Pusher")
	}
	if _, ok := plainWrapped.(io.ReaderFrom); ok {
		t.Fatal("plain writer falsely advertised ReaderFrom")
	}
	if unwrapped := plainWrapped.(interface{ Unwrap() http.ResponseWriter }).Unwrap(); unwrapped != plain {
		t.Fatal("plain writer Unwrap did not return the native writer")
	}

	native := &metricsAllCapabilitiesWriter{ResponseRecorder: httptest.NewRecorder()}
	base := &requestMetricsWriter{ResponseWriter: native}
	wrapped := wrapRequestMetricsWriter(base)
	flusher, flushOK := wrapped.(http.Flusher)
	hijacker, hijackOK := wrapped.(http.Hijacker)
	pusher, pushOK := wrapped.(http.Pusher)
	readerFrom, readOK := wrapped.(io.ReaderFrom)
	if !flushOK || !hijackOK || !pushOK || !readOK {
		t.Fatalf("all-capability writer retained flusher/hijacker/pusher/reader = %t/%t/%t/%t", flushOK, hijackOK, pushOK, readOK)
	}
	flusher.Flush()
	if err := pusher.Push("/synthetic", nil); err != nil {
		t.Fatal(err)
	}
	count, err := readerFrom.ReadFrom(strings.NewReader("payload"))
	if err != nil || count != 7 || base.bytes != 7 {
		t.Fatalf("ReaderFrom count/base bytes/error = %d/%d/%v, want 7/7/nil", count, base.bytes, err)
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if !native.flushed || !native.pushed || !native.read || !base.hijacked {
		t.Fatalf("capability calls flush/push/read/hijack = %t/%t/%t/%t", native.flushed, native.pushed, native.read, base.hijacked)
	}
	if err := http.NewResponseController(wrapped).Flush(); err != nil {
		t.Fatalf("ResponseController Flush: %v", err)
	}
}
