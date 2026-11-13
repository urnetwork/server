package work

// Returned outcomes must not turn pre-submit measurements into acknowledgments.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

func TestEgressSubmissionReturnedOutcomeClassification(t *testing.T) {
	for _, test := range []struct {
		err         error
		unsupported error
		want        string
	}{
		{unsupported: ingest.ErrAttemptUnsupported, want: "acknowledged"},
		{err: fmt.Errorf("synthetic wrap: %w", ingest.ErrAttemptUnsupported), unsupported: ingest.ErrAttemptUnsupported, want: "unsupported"},
		{err: fmt.Errorf("synthetic wrap: %w", egresshealth.ErrUnsupported), unsupported: egresshealth.ErrUnsupported, want: "unsupported"},
		{err: fmt.Errorf("synthetic wrap: %w", context.Canceled), unsupported: egresshealth.ErrUnsupported, want: "canceled"},
		{err: context.DeadlineExceeded, unsupported: ingest.ErrAttemptUnsupported, want: "canceled"},
		{err: errors.New("synthetic-private-body.example"), unsupported: egresshealth.ErrUnsupported, want: "error_or_unknown"},
		{err: ingest.ErrAttemptUnsupported, unsupported: egresshealth.ErrUnsupported, want: "error_or_unknown"},
	} {
		if got := egressProbeSubmissionOutcome(test.err, test.unsupported); got != test.want {
			t.Fatalf("outcome = %s, want %s", got, test.want)
		}
	}
}

// Cancellation immediately before the inner return must not relabel either an
// acknowledged call or an unrelated failure. The returned error owns the class.
func TestEgressSubmissionConcurrentCancellationPreservesReturnedOutcome(t *testing.T) {
	for _, kind := range []string{"health", "attempt"} {
		for _, returned := range []error{nil, errors.New("synthetic-private-return.example")} {
			ctx, cancel := context.WithCancel(context.Background())
			inner := newFakeEgressProbeIngest()
			inner.attemptErr, inner.healthErr = returned, returned
			inner.beforeReturn = cancel
			reporter := newEgressProbeMetricsReporter(inner, func(context.Context, string) string { return "" })
			outcome := "acknowledged"
			if returned != nil {
				outcome = "error_or_unknown"
			}
			counter := egressProbeSubmissionOutcomesTotal.WithLabelValues(kind, outcome)
			canceledCounter := egressProbeSubmissionOutcomesTotal.WithLabelValues(kind, "canceled")
			before, canceledBefore := testutil.ToFloat64(counter), testutil.ToFloat64(canceledCounter)
			var err error
			if kind == "health" {
				err = reporter.SubmitEgressHealth(ctx, "synthetic-provider", &egresshealth.Result{})
			} else {
				err = reporter.ReportAttempt(ctx, "synthetic-provider", "synthetic-failure")
			}
			canceledBeforeReturn := ctx.Err() == context.Canceled
			cancel()
			if !canceledBeforeReturn || err != returned || testutil.ToFloat64(counter) != before+1 || testutil.ToFloat64(canceledCounter) != canceledBefore {
				t.Fatalf("%s/%s was relabeled by concurrent cancellation", kind, outcome)
			}
		}
	}
}

func TestEgressSubmissionCountOccursAfterCallAndPreservesError(t *testing.T) {
	for _, kind := range []string{"health", "attempt"} {
		for _, outcome := range []string{"acknowledged", "unsupported", "canceled", "error_or_unknown"} {
			var returned error
			switch outcome {
			case "unsupported":
				returned = ingest.ErrAttemptUnsupported
				if kind == "health" {
					returned = egresshealth.ErrUnsupported
				}
			case "canceled":
				returned = context.Canceled
			case "error_or_unknown":
				returned = errors.New("synthetic-private-return.example")
			}
			counter := egressProbeSubmissionOutcomesTotal.WithLabelValues(kind, outcome)
			before := testutil.ToFloat64(counter)
			inner := newFakeEgressProbeIngest()
			inner.attemptErr, inner.healthErr = returned, returned
			calls := 0
			inner.beforeReturn = func() {
				calls++
				if testutil.ToFloat64(counter) != before {
					t.Fatal("outcome counted before the inner call returned")
				}
			}
			reporter := newEgressProbeMetricsReporter(inner, func(context.Context, string) string { return "" })
			var err error
			if kind == "health" {
				err = reporter.SubmitEgressHealth(context.Background(), "synthetic-provider", &egresshealth.Result{})
			} else {
				err = reporter.ReportAttempt(context.Background(), "synthetic-provider", "synthetic-failure")
			}
			if err != returned || calls != 1 || testutil.ToFloat64(counter) != before+1 {
				t.Fatalf("%s/%s changed forwarding or post-call outcome", kind, outcome)
			}
		}
	}
}

func TestEgressSubmissionNilHealthDoesNotAcknowledgeAndLabelsStayPrivate(t *testing.T) {
	inner := newFakeEgressProbeIngest()
	reporter := newEgressProbeMetricsReporter(inner, func(context.Context, string) string { return "" })
	counter := egressProbeSubmissionOutcomesTotal.WithLabelValues("health", "acknowledged")
	before := testutil.ToFloat64(counter)
	if err := reporter.SubmitEgressHealth(context.Background(), "synthetic-provider", nil); err != nil || inner.healthCalls != 1 {
		t.Fatal("nil result forwarding changed")
	}
	if testutil.ToFloat64(counter) != before {
		t.Fatal("nil no-request health result fabricated an acknowledgment")
	}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(egressProbeSubmissionOutcomesTotal, egressProbeSubmissionObservationEnabled)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "urnetwork_egress_probe_submission_observation_enabled" {
			continue
		}
		if len(family.Metric) != 8 {
			t.Fatal("post-call outcome domain is not fixed at eight")
		}
		for _, metric := range family.Metric {
			if len(metric.Label) != 2 {
				t.Fatal("submission metric gained identity labels")
			}
			for _, label := range metric.Label {
				if label.GetName() != "kind" && label.GetName() != "outcome" {
					t.Fatal("submission metric exposed caller data")
				}
			}
		}
	}
}
