// Completed evidence and canceled work meet at the real fleet-to-task boundary.
package work

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/operator-proxy/egresshealth"
	"github.com/urnetwork/operator-proxy/fleetprobe"
	"github.com/urnetwork/operator-proxy/ingest"
	"github.com/urnetwork/operator-proxy/prober"
	"github.com/urnetwork/server/model"
)

// One worker completes every ready check before its final sibling cancels it.
// Receiving the next job proves the preceding result was stored by RunBlackhole.
func testBlackholeFinalizeCanceledBatch(t *testing.T, ready []ingest.BlackholeCheck, submissionErr error) (fleetprobe.BlackholeSummary, []ingest.BlackholeCheck, error) {
	t.Helper()
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	original := slices.Clone(ready)
	providers := make([]prober.Provider, 0, len(ready)+1)
	for _, check := range ready {
		providers = append(providers, prober.Provider{ClientId: check.ClientId})
	}
	providers = append(providers, prober.Provider{ClientId: "synthetic-late-provider"})
	started := 0
	submissions := 0
	var accepted []ingest.BlackholeCheck
	pass := &providerEgressProbePass{
		readiness: &providerEgressProbeReadiness{
			minimum:   1,
			available: func(context.Context) (model.ByteCount, error) { return 1, nil },
		},
		blackholeOptions: fleetprobe.BlackholeOptions{
			CheckOne: func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
				index := started
				started++
				if index == len(ready) {
					cancel()
					return fleetprobe.BlackholeResult{
						Check: ingest.BlackholeCheck{ClientId: provider.ClientId, Failure: egresshealth.FailureAllDestinationsFailed},
						Dark:  true,
					}
				}
				check := ready[index]
				return fleetprobe.BlackholeResult{Check: check, Dark: !check.Ok && !check.NotMeasured, NotMeasured: check.NotMeasured}
			},
		},
		runBlackhole: fleetprobe.RunBlackhole,
		submitBlackholeChecks: func(publishCtx context.Context, checks []ingest.BlackholeCheck) error {
			submissions++
			if err := publishCtx.Err(); err != nil {
				return err
			}
			deadline, bounded := publishCtx.Deadline()
			if !bounded || time.Until(deadline) <= 0 || 30*time.Second < time.Until(deadline) {
				t.Error("retained evidence submission has no live bounded finalization deadline")
			}
			if submissionErr != nil {
				return submissionErr
			}
			accepted = slices.Clone(checks)
			return nil
		},
	}
	summary, tripped, err := pass.runBlackholeBatch(ctx, args, nil, nil, 1, providers)
	if started != len(providers) || tripped || !reflect.DeepEqual(ready, original) {
		t.Fatalf("fixture ordering/guard/raw evidence changed: started=%d want=%d guard=%t", started, len(providers), tripped)
	}
	if 0 < len(summary.Checks) && submissions != 1 {
		t.Fatalf("retained summary submission calls=%d, want1", submissions)
	}
	if len(summary.Checks) == 0 && submissions != 0 {
		t.Fatalf("empty summary submitted %d times", submissions)
	}
	return summary, accepted, err
}

// A passing completed check survives a later sibling's cancellation.
func TestBlackholeFinalizePassingEvidenceSurvives(t *testing.T) {
	ready := []ingest.BlackholeCheck{{ClientId: "synthetic-pass", Ok: true}}
	summary, accepted, err := testBlackholeFinalizeCanceledBatch(t, ready, nil)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(accepted, ready) || len(summary.Checks) != 1 {
		t.Fatalf("completed pass lost: accepted=%d checks=%d error=%v", len(accepted), len(summary.Checks), err)
	}
}

// A real TLS failure completed before cancellation remains an integrity fact.
func TestBlackholeFinalizeTlsEvidenceSurvives(t *testing.T) {
	ready := []ingest.BlackholeCheck{{ClientId: "synthetic-tls", Failure: egresshealth.FailureTlsAuthentication}}
	summary, accepted, err := testBlackholeFinalizeCanceledBatch(t, ready, nil)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(accepted, ready) || summary.Dark != 1 {
		t.Fatalf("completed TLS evidence lost: accepted=%d dark=%d error=%v", len(accepted), summary.Dark, err)
	}
}

// A completed not-measured report advances retry state without a dark verdict.
func TestBlackholeFinalizeNotMeasuredSurvives(t *testing.T) {
	ready := []ingest.BlackholeCheck{{ClientId: "synthetic-unmeasured", Failure: egresshealth.FailureNotMeasured, NotMeasured: true}}
	summary, accepted, err := testBlackholeFinalizeCanceledBatch(t, ready, nil)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(accepted, ready) || summary.NotMeasured != 1 || summary.Dark != 0 {
		t.Fatalf("completed unknown evidence lost or condemned: accepted=%d summary=%+v error=%v", len(accepted), summary, err)
	}
}

// Ordinary negatives still need live funding; cancellation retains only the
// independent evidence and excludes the in-flight post-cancellation result.
func TestBlackholeFinalizeMixedEvidenceKeepsReadinessBoundary(t *testing.T) {
	ready := []ingest.BlackholeCheck{
		{ClientId: "synthetic-pass", Ok: true},
		{ClientId: "synthetic-tls", Failure: egresshealth.FailureTlsAuthentication},
		{ClientId: "synthetic-negative", Failure: egresshealth.FailureAllDestinationsFailed},
		{ClientId: "synthetic-unmeasured", Failure: egresshealth.FailureNotMeasured, NotMeasured: true},
	}
	summary, accepted, err := testBlackholeFinalizeCanceledBatch(t, ready, nil)
	want := []ingest.BlackholeCheck{ready[0], ready[1], ready[3]}
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(accepted, want) || !reflect.DeepEqual(summary.Checks, want) || summary.Dark != 1 || summary.NotMeasured != 1 {
		t.Fatalf("mixed retained evidence wrong: accepted=%d summary=%+v error=%v", len(accepted), summary, err)
	}
}

// Both the failed publication and the lifecycle failure stay available to retry.
func TestBlackholeFinalizeSubmissionErrorIsRetained(t *testing.T) {
	submitErr := errors.New("synthetic publication unavailable")
	_, accepted, err := testBlackholeFinalizeCanceledBatch(t, []ingest.BlackholeCheck{{ClientId: "synthetic-pass", Ok: true}}, submitErr)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, submitErr) || len(accepted) != 0 {
		t.Fatalf("publication error collapsed: accepted=%d error=%v", len(accepted), err)
	}
}

// A canceled batch that finished nothing is not a successful empty measurement.
func TestBlackholeFinalizeCanceledEmptyIsNotHealthy(t *testing.T) {
	summary, accepted, err := testBlackholeFinalizeCanceledBatch(t, nil, nil)
	if !errors.Is(err, context.Canceled) || len(accepted) != 0 || len(summary.Checks) != 0 {
		t.Fatalf("canceled empty batch lost lifecycle error: summary=%+v error=%v", summary, err)
	}
}

// Normal completion still publishes the same ready passing check.
func TestBlackholeFinalizeHealthyCompletionControl(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	var accepted []ingest.BlackholeCheck
	pass := &providerEgressProbePass{
		blackholeOptions: fleetprobe.BlackholeOptions{CheckOne: func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
			return fleetprobe.BlackholeResult{Check: ingest.BlackholeCheck{ClientId: provider.ClientId, Ok: true}}
		}},
		runBlackhole: fleetprobe.RunBlackhole,
		submitBlackholeChecks: func(ctx context.Context, checks []ingest.BlackholeCheck) error {
			accepted = slices.Clone(checks)
			return ctx.Err()
		},
	}
	summary, tripped, err := pass.runBlackholeBatch(context.Background(), args, nil, nil, 1, []prober.Provider{{ClientId: "synthetic-pass"}})
	if err != nil || tripped || len(accepted) != 1 || !accepted[0].Ok || !reflect.DeepEqual(accepted, summary.Checks) {
		t.Fatalf("healthy baseline changed: accepted=%+v summary=%+v error=%v", accepted, summary, err)
	}
}

// No result finishing after cancellation may become even an integrity verdict.
func TestBlackholeFinalizeInFlightTlsIsNotEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	summary, err := fleetprobe.RunBlackhole(ctx, []prober.Provider{{ClientId: "synthetic-late-tls"}}, fleetprobe.BlackholeOptions{
		Timeout:     time.Second,
		Concurrency: 1,
		CheckOne: func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
			cancel()
			return fleetprobe.BlackholeResult{Check: ingest.BlackholeCheck{ClientId: provider.ClientId, Failure: egresshealth.FailureTlsAuthentication}, Dark: true}
		},
	})
	if err != nil || len(summary.Checks) != 0 || summary.Dark != 0 {
		t.Fatalf("in-flight canceled result escaped worker ownership: summary=%+v error=%v", summary, err)
	}
}

// The minimum-sized all-failing measured batch still uses its existing guard.
func TestBlackholeFinalizeGuardPolicyControl(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	providers := make([]prober.Provider, args.DarkBatchGuardMinChecks)
	for i := range providers {
		providers[i] = prober.Provider{ClientId: fmt.Sprintf("synthetic-failed-%d", i)}
	}
	var accepted []ingest.BlackholeCheck
	pass := &providerEgressProbePass{
		blackholeOptions: fleetprobe.BlackholeOptions{CheckOne: func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
			return fleetprobe.BlackholeResult{Check: ingest.BlackholeCheck{ClientId: provider.ClientId, Failure: egresshealth.FailureAllDestinationsFailed}, Dark: true}
		}},
		runBlackhole: fleetprobe.RunBlackhole,
		submitBlackholeChecks: func(ctx context.Context, checks []ingest.BlackholeCheck) error {
			accepted = slices.Clone(checks)
			return ctx.Err()
		},
	}
	summary, tripped, err := pass.runBlackholeBatch(context.Background(), args, nil, nil, 1, providers)
	if err != nil || !tripped || summary.Dark != 0 || len(accepted) != len(providers) || summary.NotMeasured != len(providers) {
		t.Fatalf("guard semantics changed: accepted=%d summary=%+v tripped=%t error=%v", len(accepted), summary, tripped, err)
	}
	for _, check := range accepted {
		if check.Ok || !check.NotMeasured || check.Failure != egresshealth.FailureNotMeasured {
			t.Fatal("ordinary negative escaped the protective guard")
		}
	}
}

// A task canceled before admission starts neither a tunnel nor finalization.
func TestBlackholeFinalizePreCanceledControl(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pass := &providerEgressProbePass{
		readiness: &providerEgressProbeReadiness{minimum: 1, available: func(context.Context) (model.ByteCount, error) {
			t.Error("credit read after cancellation")
			return 1, nil
		}},
		runBlackhole: func(context.Context, []prober.Provider, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			t.Error("run after cancellation")
			return fleetprobe.BlackholeSummary{}, nil
		},
		submitBlackholeChecks: func(context.Context, []ingest.BlackholeCheck) error {
			t.Error("publish without measurement")
			return nil
		},
	}
	_, _, err := pass.runBlackholeBatch(ctx, args, nil, nil, 1, []prober.Provider{{ClientId: "synthetic-unstarted"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled admission error=%v", err)
	}
}
