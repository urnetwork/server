package work

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/qualityprobe/prober"
)

type rejectingFullAttemptIngest struct{ egressProbeIngest }

func (self rejectingFullAttemptIngest) ReportAttempt(context.Context, string, string) error {
	return errors.New("synthetic attempt acknowledgement failure")
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
