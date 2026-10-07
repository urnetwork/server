package work

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

type rejectingFullAttemptIngest struct{ egressProbeIngest }

func (self rejectingFullAttemptIngest) ReportAttempt(context.Context, string, string) error {
	return errors.New("synthetic attempt acknowledgement failure")
}

// An exit-only buffered row must not turn into website-health success.
func TestFullBatchWithoutHealthCannotSucceed(t *testing.T) {
	inner := newRecordingEgressProbeIngest()
	batch := newProviderEgressFullBatch(testFullBatchSink(inner))
	if err := batch.ReportAttempt(context.Background(), "synthetic-provider", ""); err != nil {
		t.Fatal(err)
	}
	failed := batch.release(context.Background(), false, nil, nil, 0.9, nil)
	if failed != 1 || len(inner.calls) != 1 || inner.calls[0] != "attempt synthetic-provider run_not_measured" {
		t.Fatalf("missing health was counted: failures=%d calls=%v", failed, inner.calls)
	}
}

// The final publication edge preserves acknowledged health if only geo fails.
func TestFullBatchOptionalGeoFailurePreservesHealthSuccess(t *testing.T) {
	inner := newRecordingEgressProbeIngest("synthetic-provider")
	batch := newProviderEgressFullBatch(testFullBatchSink(inner))
	run := &egresshealth.Result{OkCount: 1, Total: 1, Checks: []egresshealth.CheckResult{{Name: "synthetic-site", Class: egresshealth.ClassSite, Ok: true}}}
	for _, err := range []error{
		batch.SubmitEgressHealth(context.Background(), "synthetic-provider", run),
		batch.Submit(context.Background(), "synthetic-provider", "192.0.2.21", time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)),
		batch.ReportAttempt(context.Background(), "synthetic-provider", ""),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	failed := batch.release(context.Background(), false, nil, nil, 0.9, nil)
	if failed != 0 || len(inner.calls) != 3 || inner.calls[2] != "attempt synthetic-provider " {
		t.Fatalf("optional geo revoked health: failures=%d calls=%v", failed, inner.calls)
	}
}

func TestFullBatchAttemptPublicationFailureFailsTheTask(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	sink := testFullBatchSink(rejectingFullAttemptIngest{newRecordingEgressProbeIngest()})
	pass := &providerEgressProbePass{
		fullSink: sink,
		runFull: func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			if err := options.Attempts.ReportAttempt(ctx, providers[0].ClientId, prober.FailureTunnel); err != nil {
				return prober.Summary{}, err
			}
			return prober.Summary{Attempted: 1}, nil
		},
	}
	outcome := pass.runFullBatch(context.Background(), args, nil, nil, testDueProviders("synthetic-provider"))
	if outcome.err == nil || !strings.Contains(outcome.err.Error(), "attempt publication failed for 1 providers") {
		t.Errorf("missing durable attempt was acknowledged as successful: %+v", outcome)
	}
}

func TestGuardedFullBatchAttemptPublicationFailureIsCounted(t *testing.T) {
	batch := newProviderEgressFullBatch(testFullBatchSink(rejectingFullAttemptIngest{newRecordingEgressProbeIngest()}))
	if err := batch.ReportAttempt(context.Background(), "synthetic-provider", prober.FailureTunnel); err != nil {
		t.Fatal(err)
	}
	batch.release(context.Background(), true, nil, nil, 0.9, nil)
	if batch.releaseAttemptFailures != 1 {
		t.Errorf("guarded attempt failure count=%d, want 1", batch.releaseAttemptFailures)
	}
}

var _ egressProbeIngest = rejectingFullAttemptIngest{}

// A health rejection must revoke the provisional buffered success, even when
// the attempt endpoint acknowledges it and there is deliberately no exit.
type rejectingFullHealthIngest struct{ egressProbeIngest }

// Simulates the actual publication edge, after the batch guard accepted evidence.
func (self rejectingFullHealthIngest) SubmitEgressHealth(context.Context, string, *egresshealth.Result) error {
	return errors.New("synthetic health acknowledgement failure")
}

// Buffered calls are not durable acknowledgements until release succeeds.
func TestFullBatchHealthPublicationFailureRevokesSuccess(t *testing.T) {
	inner := newRecordingEgressProbeIngest()
	batch := newProviderEgressFullBatch(testFullBatchSink(rejectingFullHealthIngest{inner}))
	run := &egresshealth.Result{OkCount: 1, Total: 1, Checks: []egresshealth.CheckResult{{Name: "synthetic-site", Class: egresshealth.ClassSite, Ok: true}}}
	if err := batch.SubmitEgressHealth(context.Background(), "synthetic-provider", run); err != nil {
		t.Fatal(err)
	}
	if err := batch.ReportAttempt(context.Background(), "synthetic-provider", ""); err != nil {
		t.Fatal(err)
	}
	failed := batch.release(context.Background(), false, nil, nil, 0.9, nil)
	if failed != 1 || len(inner.calls) != 1 || inner.calls[0] != "attempt synthetic-provider submit_failed" {
		t.Fatalf("failed health was counted as success: failed=%d calls=%v", failed, inner.calls)
	}
}
