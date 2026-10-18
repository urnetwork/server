package localoperator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe"
	"github.com/urnetwork/server/qualityprobe/ingest"
)

// Observations in one method's series of a summary.
func testSummaryCount(t *testing.T, summary *prometheus.SummaryVec, method string) uint64 {
	t.Helper()
	var metric dto.Metric
	if err := summary.WithLabelValues(method).(prometheus.Metric).Write(&metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetSummary().GetSampleCount()
}

// In-process calls keep, per operator method, the series the API router kept
// per route: calls running in their slots, their duration and their errors by
// kind, and the wait for a slot with the calls waiting now. A call that gave up
// waiting is counted as unadmitted and never as a run; a refusal, a panicking
// claim and a caller that gave up while its call ran are each their own kind;
// a success and the due maintenance page are no error.
func TestOperatorPublishesPerMethodCallSeries(t *testing.T) {
	metrics := newOperatorMetrics(prometheus.NewPedanticRegistry())
	entered := make(chan struct{}, 1)
	releaseAll, release := context.WithCancel(context.Background())
	defer release()
	waiting := make(chan struct{}, 1)
	operator := testOperatorWithConcurrency(t, 1, testHeldClaim(entered, nil, releaseAll), nil)
	operator.metrics = metrics
	operator.slotWait = func() { waiting <- struct{}{} }

	held := make(chan error, 1)
	go func() {
		_, err := operator.Due(context.Background(), 1)
		held <- err
	}()
	<-entered
	if got := testutil.ToFloat64(metrics.inflight.WithLabelValues("due")); got != 1 {
		t.Fatalf("a running due call is %v in flight, want 1", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	attempt := make(chan error, 1)
	go func() {
		attempt <- operator.ReportAttempt(ctx, "not-a-client-id", "")
	}()
	select {
	case <-waiting:
	case err := <-attempt:
		t.Fatalf("an attempt report ran beside the held claim in a one-slot operator: %v", err)
	}
	if got := testutil.ToFloat64(metrics.waiting.WithLabelValues("report_attempt")); got != 1 {
		t.Fatalf("a waiting attempt report is %v waiting, want 1", got)
	}
	cancel()
	if err := <-attempt; !errors.Is(err, context.Canceled) {
		t.Fatalf("the waiting attempt report: %v", err)
	}
	release()
	if err := <-held; err != nil {
		t.Fatalf("the held due call: %v", err)
	}

	if err := operator.ReportAttempt(context.Background(), "not-a-client-id", ""); !errors.Is(err, ingest.ErrRejected) {
		t.Fatalf("a malformed attempt report was not refused: %v", err)
	}
	operator.claimDue = func(context.Context, time.Time, int, int, int, *model.ProviderUrlProbeDueObservation) model.ProviderUrlProbeDueResult {
		panic(errors.New("synthetic model failure"))
	}
	if _, err := operator.Due(context.Background(), 1); !errors.Is(err, ingest.ErrRejected) {
		t.Fatalf("a panicking claim: %v", err)
	}
	callerCtx, callerCancel := context.WithCancel(context.Background())
	operator.claimDue = func(context.Context, time.Time, int, int, int, *model.ProviderUrlProbeDueObservation) model.ProviderUrlProbeDueResult {
		callerCancel()
		panic(fmt.Errorf("claim: %w", context.Canceled))
	}
	if _, err := operator.Due(callerCtx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("a claim whose caller gave up: %v", err)
	}
	operator.claimDue = func(context.Context, time.Time, int, int, int, *model.ProviderUrlProbeDueObservation) model.ProviderUrlProbeDueResult {
		return model.ProviderUrlProbeDueResult{Providers: []model.ProviderUrlProbeDue{}, PriorityMaintenancePending: true}
	}
	if _, err := operator.Due(context.Background(), 1); !errors.Is(err, ingest.ErrDuePriorityMaintenancePending) {
		t.Fatalf("the maintenance page: %v", err)
	}

	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{name: "due in flight", got: testutil.ToFloat64(metrics.inflight.WithLabelValues("due")), want: 0},
		{name: "attempt reports in flight", got: testutil.ToFloat64(metrics.inflight.WithLabelValues("report_attempt")), want: 0},
		{name: "attempt reports waiting", got: testutil.ToFloat64(metrics.waiting.WithLabelValues("report_attempt")), want: 0},
		{name: "due runs", got: float64(testSummaryCount(t, metrics.duration, "due")), want: 4},
		{name: "attempt report runs", got: float64(testSummaryCount(t, metrics.duration, "report_attempt")), want: 1},
		{name: "due slot waits", got: float64(testSummaryCount(t, metrics.slotWait, "due")), want: 4},
		{name: "attempt report slot waits", got: float64(testSummaryCount(t, metrics.slotWait, "report_attempt")), want: 2},
		{name: "unadmitted attempt reports", got: testutil.ToFloat64(metrics.errors.WithLabelValues("report_attempt", callErrorUnadmitted)), want: 1},
		{name: "refused attempt reports", got: testutil.ToFloat64(metrics.errors.WithLabelValues("report_attempt", callErrorRefused)), want: 1},
		{name: "failed due calls", got: testutil.ToFloat64(metrics.errors.WithLabelValues("due", callErrorFailed)), want: 1},
		{name: "canceled due calls", got: testutil.ToFloat64(metrics.errors.WithLabelValues("due", callErrorCanceled)), want: 1},
		{name: "refused due calls", got: testutil.ToFloat64(metrics.errors.WithLabelValues("due", callErrorRefused)), want: 0},
		{name: "unadmitted due calls", got: testutil.ToFloat64(metrics.errors.WithLabelValues("due", callErrorUnadmitted)), want: 0},
	} {
		if c.got != c.want {
			t.Errorf("%s: %v, want %v", c.name, c.got, c.want)
		}
	}
}

// A call's error kind is the answer the API would have given: a 4xx refusal is
// refused; a 5xx refusal, a panic and a local failure are failed, even a done
// error inside a call whose caller still waits (the router's 503); a context
// end is canceled; success and the due maintenance page are no error.
func TestCallErrorKindsFollowTheApiAnswer(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		kind string
	}{
		{name: "success", err: nil, kind: ""},
		{name: "maintenance page", err: ingest.ErrDuePriorityMaintenancePending, kind: ""},
		{name: "bad request", err: badRequest(), kind: callErrorRefused},
		{name: "no budget", err: &handlerRefusal{status: http.StatusTooManyRequests, message: "budget"}, kind: callErrorRefused},
		{name: "pool unavailable", err: &handlerRefusal{status: http.StatusInternalServerError, message: "pool"}, kind: callErrorFailed},
		{name: "panic", err: &handlerFailure{status: http.StatusInternalServerError, cause: errors.New("synthetic")}, kind: callErrorFailed},
		{name: "done error", err: &handlerFailure{status: http.StatusServiceUnavailable, cause: fmt.Errorf("claim: %w", server.DbContextDoneError)}, kind: callErrorFailed},
		{name: "caller gone", err: context.Canceled, kind: callErrorCanceled},
		{name: "call bound", err: context.DeadlineExceeded, kind: callErrorCanceled},
		{name: "receipt mismatch", err: qualityprobe.ErrUrlProbeCompletionUnsupported, kind: callErrorFailed},
	} {
		if kind := callErrorKind(c.err); kind != c.kind {
			t.Errorf("%s: kind %q, want %q", c.name, kind, c.kind)
		}
	}
}
