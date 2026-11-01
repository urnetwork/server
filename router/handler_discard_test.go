package router

// The discard of WrapWithInputRequireClientUnlessAnswered answers from the
// decoded body alone, before any credential is read: no authentication is
// attempted, prepare and impl never run, and the request goes from its
// prepare phase straight to its response. Synthetic functions stand in for
// those of /connect/control; nothing here reads a database or Redis.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
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
