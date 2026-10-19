package router

// A database child deadline can end while the HTTP caller is still alive.
// Exercise the actual router and due client without a database or remote host.
import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

// Db converts an expired child connection context into this sentinel. A live
// caller must see an unavailable response, not implicit 200 with empty JSON.
func TestRouterChildDoneReturnsUnavailableToDueClient(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	route := NewRoute(http.MethodGet, "/network/provider-blackhole-due", func(_ http.ResponseWriter, request *http.Request) {
		child, stop := context.WithDeadline(request.Context(), time.Unix(0, 0))
		defer stop()
		if !errors.Is(child.Err(), context.DeadlineExceeded) || request.Context().Err() != nil {
			t.Error("fixture did not distinguish child expiry from caller cancellation")
		}
		panic(server.DbContextDoneError)
	})
	router := NewRouter(ctx, []*Route{route})
	metrics := newHttpMetrics(prometheus.NewRegistry())
	router.metrics = metrics
	endpoint := httptest.NewServer(router)
	defer endpoint.Close()
	response, err := endpoint.Client().Get(endpoint.URL + "/network/provider-blackhole-due")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusServiceUnavailable || string(body) != "Service temporarily unavailable.\n" {
		t.Errorf("internal deadline became malformed successful response: status=%d body=%q error=%v", response.StatusCode, body, err)
	}
	client := &ingest.Client{ServerUrl: endpoint.URL, Http: endpoint.Client(), OperatorSecret: "synthetic-operator-secret"}
	due, err := client.BlackholeDue(ctx, 250)
	if len(due) != 0 || !errors.Is(err, ingest.ErrRejected) || errors.Is(err, io.EOF) {
		t.Errorf("internal deadline reached due JSON decoder instead of rejection: due=%d error=%v", len(due), err)
	}
	if got := testutil.ToFloat64(metrics.requests.WithLabelValues(route.id, "503", "canceled")); got != 2 {
		t.Errorf("unavailable child deadline status lost in metrics: %g", got)
	}
}

// An already-canceled caller and a hijacked connection retain their original
// no-write ownership. Only an internal cancellation with a live caller changes.
func TestRouterChildDonePreservesCanceledCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	router := NewRouter(ctx, []*Route{NewRoute(http.MethodGet, "/due", func(http.ResponseWriter, *http.Request) {
		panic(server.DbContextDoneError)
	})})
	requestCtx, stop := context.WithCancel(ctx)
	stop()
	writer := httptest.NewRecorder()
	router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/due", nil).WithContext(requestCtx))
	if writer.Body.Len() != 0 || len(writer.Header()) != 0 {
		t.Fatalf("canceled caller received a synthesized error: body=%q", writer.Body.String())
	}
}

// Once a response is committed, changing its status or completing a partial
// JSON/stream body would lie to the caller. Preserve bytes and abort transport.
func TestRouterChildDoneAbortsCommittedResponse(t *testing.T) {
	for _, test := range []struct {
		name                 string
		write, flush, header bool
	}{
		{name: "implicit_write", write: true},
		{name: "implicit_flush", flush: true},
		{name: "explicit_header", header: true},
		{name: "write_then_flush", write: true, flush: true},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		router := NewRouter(ctx, []*Route{NewRoute(http.MethodGet, "/stream", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if test.header {
				w.WriteHeader(http.StatusOK)
			}
			if test.write {
				_, _ = io.WriteString(w, `{"providers":[`)
			}
			if test.flush {
				w.(http.Flusher).Flush()
			}
			panic(server.DbContextDoneError)
		})})
		metrics := newHttpMetrics(prometheus.NewRegistry())
		router.metrics = metrics
		writer := httptest.NewRecorder()
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/stream", nil))
		}()
		cancel()
		wantBody := ""
		if test.write {
			wantBody = `{"providers":[`
		}
		if recovered != http.ErrAbortHandler || writer.Code != http.StatusOK || writer.Body.String() != wantBody || strings.Contains(writer.Body.String(), "unavailable") {
			t.Fatalf("internal deadline completed a partial response: case=%s recovered=%v status=%d body=%q", test.name, recovered, writer.Code, writer.Body.String())
		}
		if got := testutil.ToFloat64(metrics.requests.WithLabelValues("GET ^/stream$", "200", "aborted")); got != 1 {
			t.Errorf("%s: abort was not observed once before escaping: %g", test.name, got)
		}
		if got := testutil.ToFloat64(metrics.inflight.WithLabelValues("GET ^/stream$")); got != 0 {
			t.Errorf("%s: abort left %g HTTP requests in flight", test.name, got)
		}
	}
}
