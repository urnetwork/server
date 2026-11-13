package router

// The discard of WrapWithInputRequireClientUnlessAnswered answers from the
// decoded body alone, before any credential is read: no authentication is
// attempted, prepare and impl never run, and the request goes from its
// prepare phase straight to its response. Synthetic functions stand in for
// those of /connect/control; nothing here reads a database or Redis.

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

type discardTestInput struct {
	Discard bool `json:"discard"`
}

// The calls each function of the wrapper received, and the Authorization
// header the discard saw.
type discardTestCalls struct {
	discards      int
	prepares      int
	impls         int
	authorization string
}

func (self *discardTestCalls) discard(req *http.Request, input discardTestInput) (string, bool) {
	self.discards += 1
	self.authorization = req.Header.Get("Authorization")
	return "discarded", input.Discard
}

func (self *discardTestCalls) prepare(context.Context, server.Id, discardTestInput) (string, bool) {
	self.prepares += 1
	return "prepared", true
}

func (self *discardTestCalls) impl(discardTestInput, *session.ClientSession) (map[string]string, error) {
	self.impls += 1
	return map[string]string{"answer": "impl"}, nil
}

// Every session authentication this process attempted.
func discardTestAuthAttempts(t *testing.T) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := float64(0)
	for _, family := range families {
		if family.GetName() != "urnetwork_session_auth_attempts_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			total += metric.GetCounter().GetValue()
		}
	}
	return total
}

// Serves one observed request through the wrapper with discard, which may be
// nil.
func discardTestServe(
	metrics *controlHttpMetrics,
	calls *discardTestCalls,
	discard func(*http.Request, discardTestInput) (string, bool),
	body string,
	authorization string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/connect/control", strings.NewReader(body))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	req, finish := observeConnectControl(req, metrics)
	writer := httptest.NewRecorder()
	func() {
		defer finish()
		WrapWithInputRequireClientUnlessAnswered(
			discard,
			calls.prepare,
			func(_ context.Context, answer string) (map[string]string, error) {
				return map[string]string{"answer": answer}, nil
			},
			calls.impl,
			writer,
			req,
		)
	}()
	return writer
}

// A discarded request is replied to without a credential, or with one that
// would not verify, and no authentication is attempted.
func TestWrapUnlessAnsweredDiscardsBeforeAuthentication(t *testing.T) {
	metrics := newControlHttpMetrics(prometheus.NewRegistry())
	attemptsBefore := discardTestAuthAttempts(t)
	for _, authorization := range []string{"", "Bearer synthetic-unverifiable-token"} {
		calls := &discardTestCalls{}
		writer := discardTestServe(metrics, calls, calls.discard, `{"discard":true}`, authorization)
		if writer.Code != http.StatusOK || strings.TrimSpace(writer.Body.String()) != `{"answer":"discarded"}` {
			t.Fatalf("authorization %q: %d %s, want the discard's reply", authorization, writer.Code, writer.Body.String())
		}
		if calls.discards != 1 || calls.prepares != 0 || calls.impls != 0 {
			t.Fatalf("authorization %q: calls = %+v, want only the discard", authorization, calls)
		}
		// the discard reads the request's headers, which it may label by
		if calls.authorization != authorization {
			t.Fatalf("the discard saw authorization %q, want %q", calls.authorization, authorization)
		}
	}
	if attempts := discardTestAuthAttempts(t) - attemptsBefore; attempts != 0 {
		t.Fatalf("discarded requests attempted authentication %v times", attempts)
	}
	for _, phase := range []string{"authenticate", "controller"} {
		for _, outcome := range controlHttpOutcomes {
			if got := testutil.ToFloat64(metrics.completions.WithLabelValues(phase, outcome)); got != 0 {
				t.Fatalf("a discarded request entered %s (%s %v)", phase, outcome, got)
			}
		}
	}
	for _, phase := range []string{"prepare", "response"} {
		if got := testutil.ToFloat64(metrics.completions.WithLabelValues(phase, "ok")); got != 2 {
			t.Fatalf("%s completions = %v, want 2", phase, got)
		}
	}
	if got := testutil.ToFloat64(metrics.requests.WithLabelValues("unmarked", "response", "ok")); got != 2 {
		t.Fatalf("discarded requests completed %v times, want 2", got)
	}
}

// Input the discard declines authenticates exactly as before, and a nil
// discard declines everything: without a credential the request is a 401,
// and neither prepare nor impl runs.
func TestWrapUnlessAnsweredAuthenticatesWhatItDoesNotDiscard(t *testing.T) {
	metrics := newControlHttpMetrics(prometheus.NewRegistry())
	declined := &discardTestCalls{}
	nilDiscard := &discardTestCalls{}
	for _, c := range []struct {
		name    string
		calls   *discardTestCalls
		discard func(*http.Request, discardTestInput) (string, bool)
		body    string
	}{
		{name: "declined", calls: declined, discard: declined.discard, body: `{"discard":false}`},
		{name: "nil discard", calls: nilDiscard, discard: nil, body: `{"discard":true}`},
	} {
		attemptsBefore := discardTestAuthAttempts(t)
		writer := discardTestServe(metrics, c.calls, c.discard, c.body, "")
		if writer.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status %d, want 401", c.name, writer.Code)
		}
		if c.calls.prepares != 0 || c.calls.impls != 0 {
			t.Fatalf("%s: calls = %+v", c.name, c.calls)
		}
		if attempts := discardTestAuthAttempts(t) - attemptsBefore; attempts != 1 {
			t.Fatalf("%s: %v authentication attempts, want 1", c.name, attempts)
		}
	}
	if declined.discards != 1 || nilDiscard.discards != 0 {
		t.Fatalf("discard calls = %d and %d, want 1 and 0", declined.discards, nilDiscard.discards)
	}
	if got := testutil.ToFloat64(metrics.completions.WithLabelValues("authenticate", "rejected")); got != 2 {
		t.Fatalf("authenticate rejections = %v, want 2", got)
	}
}

// The residence of discarded requests, from the collector's own registry.
func discardTestDiscardedSeconds(t *testing.T, registry *prometheus.Registry) (count uint64, sum float64) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "urnetwork_connect_control_http_discarded_seconds" {
			summary := family.GetMetric()[0].GetSummary()
			return summary.GetSampleCount(), summary.GetSampleSum()
		}
	}
	t.Fatal("the discarded request collector is not registered")
	return 0, 0
}

// A discarded request's handler residence counts as a discard's, so the
// route's mean can leave it out (SIGNALS.md §2.19e); a request that
// authenticates does not count.
func TestWrapUnlessAnsweredTimesDiscardedRequests(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newControlHttpMetrics(registry)
	now := time.Unix(1_700_000_000, 0)
	metrics.now = func() time.Time { return now }
	calls := &discardTestCalls{}
	slowDiscard := func(req *http.Request, input discardTestInput) (string, bool) {
		now = now.Add(3 * time.Millisecond)
		return calls.discard(req, input)
	}
	if writer := discardTestServe(metrics, calls, slowDiscard, `{"discard":true}`, ""); writer.Code != http.StatusOK {
		t.Fatalf("discarded request: status %d", writer.Code)
	}
	if writer := discardTestServe(metrics, calls, slowDiscard, `{"discard":false}`, ""); writer.Code != http.StatusUnauthorized {
		t.Fatalf("declined request: status %d", writer.Code)
	}
	count, sum := discardTestDiscardedSeconds(t, registry)
	if count != 1 || math.Abs(sum-0.003) > 1e-9 {
		t.Fatalf("discarded residence = %d requests, %v s; want 1 request, 0.003 s", count, sum)
	}
	if total := testutil.ToFloat64(metrics.requests.WithLabelValues("unmarked", "response", "ok")) + testutil.ToFloat64(metrics.requests.WithLabelValues("unmarked", "authenticate", "rejected")); total != 2 {
		t.Fatalf("logical requests = %v, want 2", total)
	}
}

type discardTestWriter struct{ http.ResponseWriter }

func (self *discardTestWriter) Unwrap() http.ResponseWriter { return self.ResponseWriter }

// The cancellation subtraction must count the same terminal outcomes as the
// router, including cancellation after the control observer has finished and
// dependency cancellation while the request context is still live.
func TestWrapUnlessAnsweredCountsDiscardedRouterCancellations(t *testing.T) {
	for _, fixture := range []struct {
		name               string
		discard            bool
		cancelReply        bool
		cancelAfterHandler bool
		replyPanic         any
		wrapWriter         bool
		status, outcome    string
		canceledDiscards   float64
	}{
		{name: "completed discard", discard: true, status: "200", outcome: "completed"},
		{name: "canceled discard", discard: true, cancelReply: true, status: "200", outcome: "canceled", canceledDiscards: 1},
		{name: "late cancellation", discard: true, cancelAfterHandler: true, status: "200", outcome: "canceled", canceledDiscards: 1},
		{name: "wrapped writer", discard: true, cancelReply: true, wrapWriter: true, status: "200", outcome: "canceled", canceledDiscards: 1},
		{name: "dependency cancellation", discard: true, replyPanic: context.Canceled, status: "503", outcome: "canceled", canceledDiscards: 1},
		{name: "canceled panic", discard: true, cancelReply: true, replyPanic: "synthetic failure", status: "500", outcome: "panic"},
		{name: "canceled abort", discard: true, cancelReply: true, replyPanic: http.ErrAbortHandler, status: "none", outcome: "aborted"},
		{name: "ordinary cancellation", cancelAfterHandler: true, status: "401", outcome: "canceled"},
		{name: "ordinary completion", status: "401", outcome: "completed"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			controlMetrics := newControlHttpMetrics(registry)
			httpMetrics := newHttpMetrics(registry)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requestCtx, cancelRequest := context.WithCancel(ctx)
			defer cancelRequest()
			calls := &discardTestCalls{}
			route := NewRoute(http.MethodPost, "/connect/control", func(w http.ResponseWriter, r *http.Request) {
				if fixture.cancelAfterHandler {
					defer cancelRequest() // after finish, before the router's outcome
				}
				r, finish := observeConnectControl(r, controlMetrics)
				defer finish()
				if fixture.wrapWriter {
					w = &discardTestWriter{w}
				}
				WrapWithInputRequireClientUnlessAnswered(
					calls.discard, calls.prepare,
					func(_ context.Context, answer string) (map[string]string, error) {
						if fixture.cancelReply {
							cancelRequest()
						}
						if fixture.replyPanic != nil {
							panic(fixture.replyPanic)
						}
						return map[string]string{"answer": answer}, nil
					},
					calls.impl, w, r,
				)
			})
			router := NewRouter(ctx, []*Route{route})
			router.metrics = httpMetrics
			body := `{"discard":false}`
			if fixture.discard {
				body = `{"discard":true}`
			}
			request := httptest.NewRequest(http.MethodPost, "/connect/control", strings.NewReader(body)).WithContext(requestCtx)
			func() {
				defer func() {
					recovered := recover()
					if fixture.outcome == "aborted" {
						if recovered != http.ErrAbortHandler {
							t.Fatalf("abort panic = %v", recovered)
						}
					} else if recovered != nil {
						t.Fatalf("unexpected panic: %v", recovered)
					}
				}()
				router.ServeHTTP(httptest.NewRecorder(), request)
			}()
			if got := testutil.ToFloat64(httpMetrics.requests.WithLabelValues(route.id, fixture.status, fixture.outcome)); got != 1 {
				t.Fatalf("router %s/%s completions = %v, want 1", fixture.status, fixture.outcome, got)
			}
			wantDiscards := uint64(0)
			if fixture.discard {
				wantDiscards = 1
			}
			if count, _ := discardTestDiscardedSeconds(t, registry); count != wantDiscards {
				t.Fatalf("discard count = %d, want %d", count, wantDiscards)
			}
			families, err := registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			for _, family := range families {
				if family.GetName() == "urnetwork_connect_control_http_discarded_canceled_requests_total" {
					if len(family.Metric) != 1 || len(family.Metric[0].Label) != 0 || family.Metric[0].GetCounter().GetValue() != fixture.canceledDiscards {
						t.Fatalf("canceled discard metric = %v, want one unlabeled counter at %v", family, fixture.canceledDiscards)
					}
					return
				}
			}
			t.Fatal("the discarded cancellation counter is not registered")
		})
	}
}
