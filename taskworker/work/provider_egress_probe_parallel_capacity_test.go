// High parallelism is per shard/check; selected work and security rules stay fixed.
package work

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
)

// Only synthetic deployment values enter the test configuration.
func testProviderEgressParallelArgs(t *testing.T) *ProviderEgressProbeArgs {
	t.Helper()
	t.Setenv("WARP_DOMAIN", "probe.example")
	pop := server.Config.PushSimpleResource("provider_egress_probe.yml", []byte(`
enabled: true
shard_count: 4
idle_delay_seconds: 300
max_time_seconds: 4500
api_url: https://api.probe.example
platform_url: wss://platform.probe.example
public_api_url: https://api.probe.example
load_attempts: 3
load_retry_mean_interval_seconds: 300
tunnel_recreate_attempts: 2
dark_consecutive_failures: 3
dark_minimum_span_seconds: 1800
dark_backoff_seconds: [300, 900, 1800]
dark_batch_guard: 0.2
dark_batch_guard_min_checks: 10
run_batch_guard: 0.3
run_batch_guard_min_runs: 3
full:
  limit: 8
  concurrency: 8
  probe_timeout_seconds: 60
  bandwidth: true
  bandwidth_timeout_seconds: 5
blackhole:
  limit: 250
  concurrency: 250
  probe_timeout_seconds: 15
  ip_echo_timeout_seconds: 60
`))
	t.Cleanup(pop)
	settings, err := loadProviderEgressProbeSettings()
	if err != nil {
		t.Fatalf("valid high-parallel settings rejected: %v", err)
	}
	args := providerEgressProbeArgs(settings, 0)
	if err := validateProviderEgressProbeArgs(args); err != nil {
		t.Fatalf("durable high-parallel arguments rejected: %v", err)
	}
	return args
}

func TestProviderEgressParallelSettingsKeepSecurityGeometry(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	if args.ShardCount != 4 || args.Blackhole.Concurrency != 250 || args.Blackhole.Limit != 250 ||
		args.Full.Limit != 8 || args.Full.Concurrency != 8 || args.MaxTimeSeconds != 4500 || args.IdleDelaySeconds != 300 ||
		args.LoadAttempts != 3 || args.LoadRetryMeanIntervalSeconds != 300 || args.TunnelRecreateAttempts != 2 ||
		args.Blackhole.ProbeTimeoutSeconds != 15 || args.Blackhole.IpEchoTimeoutSeconds != 60 ||
		args.DarkBatchGuard != 0.2 || args.DarkBatchGuardMinChecks != 10 || args.DarkConsecutiveFailures != 3 ||
		args.RunBatchGuard != 0.3 || args.RunBatchGuardMinRuns != 3 {
		t.Fatal("parallel settings altered selection size, retry, security or task lease")
	}
	if want := 32*time.Minute + 45*time.Second; providerEgressBlackholeCheckBudget(args) != want {
		t.Fatalf("one check budget changed: %s, want %s", providerEgressBlackholeCheckBudget(args), want)
	}
}

type testProviderEgressParallelObservation struct {
	initialStarted int
	allStarted     int
	peak           int
	fullStarted    bool
	result         *ProviderEgressProbeResult
	err            error
	checks         []ingest.BlackholeCheck
}

// Hold every initial worker, then release eight while the full owner remains
// blocked. The full selected250 must already have started before that release;
// the independent full owner does not subtract from the blackhole pool.
func testProviderEgressParallelRun(t *testing.T, full bool, guarded bool, cancelRun bool, equalPools ...bool) testProviderEgressParallelObservation {
	t.Helper()
	args := testProviderEgressParallelArgs(t)
	if len(equalPools) != 0 && equalPools[0] {
		args.Full.Limit = args.Blackhole.Limit
		args.Full.Concurrency = args.Blackhole.Concurrency
	}
	var observation testProviderEgressParallelObservation
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
		defer cancel()
		due := make([]ingest.DueProvider, 250)
		for index := range due {
			due[index] = ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-parallel-%03d", index)}
		}
		fullDue := make([]ingest.DueProvider, 0, 8)
		if full {
			for index := range 8 {
				fullDue = append(fullDue, ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-full-%03d", index)})
			}
		}
		releaseFirst := make(chan struct{})
		releaseChecks := make(chan struct{})
		releaseFull := make(chan struct{})
		var started, active, peak atomic.Int32
		var fullStarted atomic.Bool
		var dueReads atomic.Int32
		pass := &providerEgressProbePass{
			blackholeDue: func(context.Context, int) ([]ingest.DueProvider, error) {
				if dueReads.Add(1) == 1 {
					return due, nil
				}
				return nil, nil
			},
			fullDue:  func(context.Context, int) ([]ingest.DueProvider, error) { return fullDue, nil },
			loadPins: func(context.Context) (map[string][]string, error) { return nil, nil },
			blackholeOptions: fleetprobe.BlackholeOptions{CheckOne: func(checkCtx context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
				number := started.Add(1)
				concurrent := active.Add(1)
				for previous := peak.Load(); previous < concurrent && !peak.CompareAndSwap(previous, concurrent); previous = peak.Load() {
				}
				defer active.Add(-1)
				if number <= 8 {
					<-releaseFirst
				} else {
					<-releaseChecks
				}
				if !cancelRun && checkCtx.Err() != nil {
					t.Error("full completion canceled an independent started check")
				}
				result := testBlackholeAdmissionPass(provider)
				if guarded && number > 1 {
					result = testBlackholeAdmissionDark(provider)
					if number == 2 {
						result = testBlackholeAdmissionTls(provider)
					}
				}
				return result
			}},
			runBlackhole: func(runCtx context.Context, providers []prober.Provider, options fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
				wantConcurrency := 250
				if options.Concurrency != wantConcurrency || options.Timeout != 15*time.Second || options.IpEchoTimeout != time.Minute ||
					options.LoadAttempts != 3 || options.LoadRetryMeanInterval != 5*time.Minute || options.TunnelRecreateAttempts != 2 {
					t.Error("parallel worker geometry changed per-check safety settings")
				}
				return fleetprobe.RunBlackhole(runCtx, providers, options)
			},
			runFull: func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
				fullStarted.Store(true)
				<-releaseFull
				return prober.Summary{Attempted: 8, Submitted: 8}, nil
			},
			submitBlackholeChecks: func(submitCtx context.Context, checks []ingest.BlackholeCheck) error {
				if submitCtx.Err() != nil {
					t.Error("completed evidence used canceled publication context")
				}
				observation.checks = append(observation.checks, checks...)
				return nil
			},
		}
		done := make(chan struct{})
		go func() { observation.result, observation.err = pass.run(ctx, args); close(done) }()
		synctest.Wait()
		observation.initialStarted = int(started.Load())
		observation.fullStarted = fullStarted.Load()
		close(releaseFirst)
		synctest.Wait()
		observation.allStarted = int(started.Load())
		close(releaseFull)
		synctest.Wait()
		select {
		case <-done:
			t.Error("pass returned before its active checks joined")
		default:
		}
		if cancelRun {
			cancel()
		}
		close(releaseChecks)
		<-done
		observation.peak = int(peak.Load())
	})
	return observation
}

func TestProviderEgressEqualIndependentPoolsStartTogether(t *testing.T) {
	got := testProviderEgressParallelRun(t, true, false, false, true)
	if !got.fullStarted || got.initialStarted != 250 || got.err != nil || got.result == nil || got.result.Submitted != 8 {
		t.Fatalf("equal-sized full and blackhole pools were serialized: full_started=%t blackhole_started=%d err=%v result=%+v", got.fullStarted, got.initialStarted, got.err, got.result)
	}
}

func TestProviderEgressParallelIndependentPoolsAdmitSelected250(t *testing.T) {
	got := testProviderEgressParallelRun(t, true, false, false)
	if got.initialStarted != 250 || got.allStarted != 250 || got.peak != 250 || got.err != nil ||
		got.result == nil || got.result.Checked != 250 || len(got.checks) != 250 || got.result.Submitted != 8 {
		t.Fatalf("high-parallel independent pools: initial=%d all=%d peak=%d checks=%d err=%v result=%+v", got.initialStarted, got.allStarted, got.peak, len(got.checks), got.err, got.result)
	}
}

func TestProviderEgressParallelNoFullAdmits250(t *testing.T) {
	got := testProviderEgressParallelRun(t, false, false, false)
	if got.initialStarted != 250 || got.allStarted != 250 || got.peak != 250 || got.err != nil ||
		got.result == nil || got.result.Checked != 250 || len(got.checks) != 250 || got.result.Attempted != 0 {
		t.Fatalf("high-parallel no-full: initial=%d all=%d peak=%d checks=%d err=%v result=%+v", got.initialStarted, got.allStarted, got.peak, len(got.checks), got.err, got.result)
	}
}

func TestProviderEgressParallelGuardHoldsOrdinaryNegatives(t *testing.T) {
	got := testProviderEgressParallelRun(t, true, true, false)
	if got.err != nil || got.result == nil || got.result.BlackholeGuardTripped != 1 || got.result.BlackholeNotMeasured != 248 ||
		got.result.Dark != 1 || len(got.checks) != 250 {
		t.Fatalf("parallel guard lost fail-safe: checks=%d err=%v result=%+v", len(got.checks), got.err, got.result)
	}
	passed, tls, held := 0, 0, 0
	for _, check := range got.checks {
		switch {
		case check.Ok:
			passed++
		case check.NotMeasured && check.Failure == egresshealth.FailureNotMeasured:
			held++
		case check.Failure == egresshealth.FailureTlsAuthentication:
			tls++
		default:
			t.Fatal("guard published an ordinary dark verdict")
		}
	}
	if passed != 1 || tls != 1 || held != 248 {
		t.Fatalf("parallel safety evidence pass/tls/held=%d/%d/%d", passed, tls, held)
	}
}

func TestProviderEgressParallelCancellationRetainsCompletedOnly(t *testing.T) {
	got := testProviderEgressParallelRun(t, true, false, true)
	if !errors.Is(got.err, context.Canceled) || got.result == nil || got.result.Checked != 8 || len(got.checks) != 8 || got.result.Dark != 0 {
		t.Fatalf("parallel cancellation fabricated or lost completed evidence: checks=%d err=%v result=%+v", len(got.checks), got.err, got.result)
	}
}
