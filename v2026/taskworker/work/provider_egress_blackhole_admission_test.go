// Stop new blackhole waves when the parallel full owner finishes; retain
// each already-started check and its unchanged security/timeout contract.
package work

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

type testBlackholeAdmissionCase struct {
	fullBeforeBlackhole bool
	minimum             int
	dueCount            int
	prefix              int
	fullErr             error
	cancel              bool
	check               func(prober.Provider) fleetprobe.BlackholeResult
}

type testBlackholeAdmissionObservation struct {
	selected  int
	result    *ProviderEgressProbeResult
	err       error
	started   int
	submitted []ingest.BlackholeCheck
}

// The first eight independently configured workers are durably held. The full owner finishes
// before they are released, not merely before a wall-clock lower bound.
func testBlackholeAdmissionRun(t *testing.T, test testBlackholeAdmissionCase) testBlackholeAdmissionObservation {
	t.Helper()
	var observation testBlackholeAdmissionObservation
	synctest.Test(t, func(t *testing.T) {
		args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
		args.Blackhole.Limit = 250
		if test.dueCount != 0 {
			args.Blackhole.Limit = test.dueCount
		}
		if test.minimum != 0 {
			args.DarkBatchGuardMinChecks = test.minimum
		}
		observation.selected = args.Blackhole.Limit
		args.Blackhole.Concurrency = 8
		args.Full.Concurrency = 4
		args.Full.Limit = 4
		args.Blackhole.ProbeTimeoutSeconds = 15
		args.Blackhole.IpEchoTimeoutSeconds = 20
		args.LoadAttempts = 3
		args.LoadRetryMeanIntervalSeconds = 300
		args.TunnelRecreateAttempts = 2
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		due := make([]ingest.DueProvider, args.Blackhole.Limit)
		for index := range due {
			due[index] = ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-%03d", index)}
		}
		initialWave := min(8, len(due))
		started := make(chan struct{}, initialWave)
		admitBlackhole := make(chan struct{})
		if !test.fullBeforeBlackhole {
			close(admitBlackhole)
		}
		releaseChecks := make(chan struct{})
		releaseFull := make(chan struct{})
		var calls atomic.Int32
		var unexpectedlyCanceled atomic.Bool
		originalTimeout := 37 * time.Second
		pass := &providerEgressProbePass{
			blackholeDue: func(context.Context, int) ([]ingest.DueProvider, error) { return due, nil },
			fullDue: func(context.Context, int) ([]ingest.DueProvider, error) {
				return testDueProviders("synthetic-full"), nil
			},
			loadPins: func(context.Context) (map[string][]string, error) { return nil, nil },
			blackholeOptions: fleetprobe.BlackholeOptions{
				Timeout:     originalTimeout,
				Concurrency: 23,
				CheckOne: func(checkCtx context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
					call := int(calls.Add(1))
					if test.prefix < call && call <= test.prefix+initialWave {
						started <- struct{}{}
						<-releaseChecks
					}
					if !test.cancel && checkCtx.Err() != nil {
						unexpectedlyCanceled.Store(true)
					}
					return test.check(provider)
				},
			},
			runBlackhole: func(runCtx context.Context, providers []prober.Provider, options fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
				<-admitBlackhole
				if options.Concurrency != 8 || options.Timeout != 15*time.Second ||
					options.IpEchoTimeout != 20*time.Second || options.LoadAttempts != 3 ||
					options.LoadRetryMeanInterval != 300*time.Second || options.TunnelRecreateAttempts != 2 {
					t.Errorf("admission changed the per-check budget or independent pool")
				}
				return fleetprobe.RunBlackhole(runCtx, providers, options)
			},
			runFull: func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
				<-releaseFull
				return prober.Summary{Attempted: 1, Submitted: 1}, test.fullErr
			},
			submitBlackholeChecks: func(submitCtx context.Context, checks []ingest.BlackholeCheck) error {
				if submitCtx.Err() != nil {
					t.Error("completed safe evidence used a canceled publication context")
				}
				observation.submitted = append(observation.submitted, checks...)
				return nil
			},
		}
		done := make(chan struct{})
		go func() {
			observation.result, observation.err = pass.run(ctx, args)
			close(done)
		}()

		if test.fullBeforeBlackhole {
			close(releaseFull)
			synctest.Wait()
			close(admitBlackhole)
		}
		for range initialWave {
			select {
			case <-started:
			case <-done:
				observation.started = int(calls.Load())
				t.Error("full completion suppressed the independent initial blackhole cohort")
				return
			}
		}
		if !test.fullBeforeBlackhole {
			close(releaseFull)
		}
		synctest.Wait()
		select {
		case <-done:
			t.Error("pass returned without joining the active blackhole checks")
		default:
		}
		if test.cancel {
			cancel()
		}
		close(releaseChecks)
		<-done
		synctest.Wait()
		observation.started = int(calls.Load())
		if unexpectedlyCanceled.Load() {
			t.Error("full completion canceled an already-started check")
		}
		if pass.blackholeOptions.Timeout != originalTimeout || pass.blackholeOptions.Concurrency != 23 {
			t.Error("per-call scheduling mutated the shared option template")
		}
	})
	return observation
}

func testBlackholeAdmissionPass(provider prober.Provider) fleetprobe.BlackholeResult {
	return fleetprobe.BlackholeResult{Check: ingest.BlackholeCheck{
		ClientId: provider.ClientId, Ok: true, CheckedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}}
}

func testBlackholeAdmissionDark(provider prober.Provider) fleetprobe.BlackholeResult {
	result := testBlackholeAdmissionPass(provider)
	result.Check.Ok = false
	result.Check.Failure = egresshealth.FailureAllDestinationsFailed
	result.Dark = true
	return result
}

func testBlackholeAdmissionTls(provider prober.Provider) fleetprobe.BlackholeResult {
	result := testBlackholeAdmissionDark(provider)
	result.Check.Failure = egresshealth.FailureTlsAuthentication
	return result
}

func testBlackholeAdmissionStopped(t *testing.T, observation testBlackholeAdmissionObservation, want int) {
	t.Helper()
	if observation.result == nil {
		t.Fatal("pass produced no result")
	}
	if observation.started != want || observation.result.Checked != want || len(observation.submitted) != want {
		t.Fatalf("after full completion started=%d checked=%d submitted=%d, want only %d admitted checks",
			observation.started, observation.result.Checked, len(observation.submitted), want)
	}
	if observation.result.BlackholeDue != observation.selected || observation.result.FullDue != 1 {
		t.Fatalf("selected due counts were changed into completed coverage: %+v", observation.result)
	}
}

func TestBlackholeAdmissionStopsNextWave(t *testing.T) {
	observation := testBlackholeAdmissionRun(t, testBlackholeAdmissionCase{check: testBlackholeAdmissionPass})
	testBlackholeAdmissionStopped(t, observation, 10)
	if observation.err != nil || observation.result.Submitted != 1 || observation.result.Dark != 0 || observation.result.BlackholeNotMeasured != 0 {
		t.Fatalf("healthy joined outcome: result=%+v err=%v", observation.result, observation.err)
	}
}

func TestBlackholeAdmissionKeepsStartedTlsEvidence(t *testing.T) {
	observation := testBlackholeAdmissionRun(t, testBlackholeAdmissionCase{check: testBlackholeAdmissionTls})
	testBlackholeAdmissionStopped(t, observation, 10)
	if observation.err != nil || observation.result.Dark != 10 || observation.result.BlackholeNotMeasured != 0 {
		t.Fatalf("TLS evidence changed: result=%+v err=%v", observation.result, observation.err)
	}
	for _, check := range observation.submitted {
		if check.NotMeasured || check.Failure != egresshealth.FailureTlsAuthentication {
			t.Fatal("a completed TLS authentication failure was weakened")
		}
	}
}

func TestBlackholeAdmissionShortSampleCannotPublishOrdinaryDark(t *testing.T) {
	observation := testBlackholeAdmissionRun(t, testBlackholeAdmissionCase{check: func(provider prober.Provider) fleetprobe.BlackholeResult {
		result := testBlackholeAdmissionDark(provider)
		if provider.ClientId == "synthetic-000" {
			result.Check.Failure = egresshealth.FailureNotMeasured
			result.Check.NotMeasured = true
			result.Dark = false
			result.NotMeasured = true
		}
		return result
	}})
	testBlackholeAdmissionStopped(t, observation, 10)
	if observation.err != nil || observation.result.Dark != 0 || observation.result.BlackholeNotMeasured != 10 || observation.result.BlackholeGuardTripped != 0 {
		t.Fatalf("short sample bypassed guard safety or fabricated a guard trip: result=%+v err=%v", observation.result, observation.err)
	}
	for _, check := range observation.submitted {
		if check.Ok || !check.NotMeasured || check.Failure != egresshealth.FailureNotMeasured || check.CheckedAt.IsZero() {
			t.Fatal("short-sample ordinary negative was not retained as unknown")
		}
	}
}

func TestBlackholeAdmissionMixedShortSamplePreservesIndependentEvidence(t *testing.T) {
	observation := testBlackholeAdmissionRun(t, testBlackholeAdmissionCase{
		check: func(provider prober.Provider) fleetprobe.BlackholeResult {
			switch provider.ClientId {
			case "synthetic-000", "synthetic-001":
				return testBlackholeAdmissionPass(provider)
			case "synthetic-002":
				return testBlackholeAdmissionTls(provider)
			case "synthetic-003":
				result := testBlackholeAdmissionDark(provider)
				result.Check.Failure = egresshealth.FailureNotMeasured
				result.Check.NotMeasured = true
				result.Dark = false
				result.NotMeasured = true
				return result
			default:
				return testBlackholeAdmissionDark(provider)
			}
		},
	})
	testBlackholeAdmissionStopped(t, observation, 10)
	if observation.err != nil || observation.result.Dark != 1 || observation.result.BlackholeNotMeasured != 7 || observation.result.BlackholeGuardTripped != 0 {
		t.Fatalf("mixed partial sample: result=%+v err=%v", observation.result, observation.err)
	}
	if !observation.submitted[0].Ok || !observation.submitted[1].Ok || observation.submitted[2].Failure != egresshealth.FailureTlsAuthentication {
		t.Fatal("passing/TLS prefix or due ordering was lost")
	}
}

func TestBlackholeAdmissionRetainsFullFailure(t *testing.T) {
	fullErr := errors.New("synthetic full owner failure")
	observation := testBlackholeAdmissionRun(t, testBlackholeAdmissionCase{check: testBlackholeAdmissionPass, fullErr: fullErr})
	testBlackholeAdmissionStopped(t, observation, 10)
	if !errors.Is(observation.err, fullErr) || observation.result.Attempted != 0 {
		t.Fatalf("full failure was cleared by useful sibling work: result=%+v err=%v", observation.result, observation.err)
	}
}

func TestBlackholeAdmissionEnoughMeasuredRetainsGuard(t *testing.T) {
	observation := testBlackholeAdmissionRun(t, testBlackholeAdmissionCase{prefix: 10, check: testBlackholeAdmissionDark})
	testBlackholeAdmissionStopped(t, observation, 18)
	if observation.err != nil || observation.result.Dark != 0 || observation.result.BlackholeNotMeasured != 18 || observation.result.BlackholeGuardTripped != 1 {
		t.Fatalf("sufficient measured sample lost the existing share guard: result=%+v err=%v", observation.result, observation.err)
	}
}

func TestBlackholeAdmissionParentCancellationStillInvalidatesActiveChecks(t *testing.T) {
	observation := testBlackholeAdmissionRun(t, testBlackholeAdmissionCase{check: testBlackholeAdmissionTls, cancel: true})
	if !errors.Is(observation.err, context.Canceled) || observation.started != 8 || observation.result.Checked != 0 || len(observation.submitted) != 0 {
		t.Fatalf("parent cancellation became an admission-only stop: result=%+v started=%d err=%v", observation.result, observation.started, observation.err)
	}
}

// These complete batches are not truncated admission samples. Their existing
// tail policy and no-full scheduling remain unchanged, not claimed repaired.
func TestBlackholeAdmissionCompleteSmallTailKeepsExistingPolicy(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	submitted := []ingest.BlackholeCheck{}
	pass := &providerEgressProbePass{
		blackholeOptions: fleetprobe.BlackholeOptions{CheckOne: func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
			return testBlackholeAdmissionDark(provider)
		}},
		runBlackhole: fleetprobe.RunBlackhole,
		submitBlackholeChecks: func(_ context.Context, checks []ingest.BlackholeCheck) error {
			submitted = slices.Clone(checks)
			return nil
		},
	}
	summary, tripped, err := pass.runBlackholeBatch(context.Background(), args, nil, nil, 2,
		fleetprobe.ProvidersFromClientIds([]string{"synthetic-a", "synthetic-b"}))
	if err != nil || tripped || summary.Dark != 2 || summary.NotMeasured != 0 || len(submitted) != 2 {
		t.Fatalf("complete tail changed: summary=%+v tripped=%t err=%v", summary, tripped, err)
	}
}

func TestBlackholeAdmissionNoFullLaneKeepsCompleteBatch(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	args.Blackhole.Limit = 20
	due := make([]ingest.DueProvider, 20)
	for index := range due {
		due[index] = ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-%03d", index)}
	}
	submitted := 0
	pass := &providerEgressProbePass{
		blackholeDue: func(context.Context, int) ([]ingest.DueProvider, error) { return due, nil },
		fullDue:      func(context.Context, int) ([]ingest.DueProvider, error) { return nil, nil },
		loadPins:     func(context.Context) (map[string][]string, error) { return nil, nil },
		blackholeOptions: fleetprobe.BlackholeOptions{CheckOne: func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
			return testBlackholeAdmissionPass(provider)
		}},
		runBlackhole: fleetprobe.RunBlackhole,
		submitBlackholeChecks: func(_ context.Context, checks []ingest.BlackholeCheck) error {
			submitted += len(checks)
			return nil
		},
	}
	result, err := pass.run(context.Background(), args)
	if err != nil || result.Checked != 20 || submitted != 20 || result.FullDue != 0 {
		t.Fatalf("no-full existing batch changed: result=%+v submitted=%d err=%v", result, submitted, err)
	}
}

func TestBlackholeAdmissionImmediateFullFailureStillStartsGuardCohort(t *testing.T) {
	fullErr := errors.New("synthetic immediate full failure")
	observation := testBlackholeAdmissionRun(t, testBlackholeAdmissionCase{
		fullBeforeBlackhole: true, fullErr: fullErr, check: testBlackholeAdmissionPass,
	})
	testBlackholeAdmissionStopped(t, observation, 10)
	if !errors.Is(observation.err, fullErr) {
		t.Fatalf("immediate full failure lost: %v", observation.err)
	}
}

func TestBlackholeAdmissionImmediateFullSuccessStillStartsGuardCohort(t *testing.T) {
	observation := testBlackholeAdmissionRun(t, testBlackholeAdmissionCase{
		fullBeforeBlackhole: true, check: testBlackholeAdmissionPass,
	})
	testBlackholeAdmissionStopped(t, observation, 10)
	if observation.err != nil || observation.result.Submitted != 1 {
		t.Fatalf("independent full success lost: result=%+v err=%v", observation.result, observation.err)
	}
}

func TestBlackholeAdmissionUsesConfiguredGuardMinimum(t *testing.T) {
	observation := testBlackholeAdmissionRun(t, testBlackholeAdmissionCase{
		fullBeforeBlackhole: true, minimum: 12, check: testBlackholeAdmissionPass,
	})
	testBlackholeAdmissionStopped(t, observation, 12)
	if observation.err != nil {
		t.Fatal(observation.err)
	}
}

func TestBlackholeAdmissionMinimumNeverInventsDueProviders(t *testing.T) {
	observation := testBlackholeAdmissionRun(t, testBlackholeAdmissionCase{
		fullBeforeBlackhole: true, dueCount: 3, check: testBlackholeAdmissionDark,
	})
	testBlackholeAdmissionStopped(t, observation, 3)
	if observation.err != nil || observation.result.Dark != 3 || observation.result.BlackholeNotMeasured != 0 || observation.result.BlackholeGuardTripped != 0 {
		t.Fatalf("complete three-provider due tail changed policy: result=%+v err=%v", observation.result, observation.err)
	}
}
