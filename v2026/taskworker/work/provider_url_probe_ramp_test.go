// The capacity-only ramp preserves URL policy and bounds actual owner admission.
package work

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
)

// Synthetic endpoints use the exact optional/default overlay, never a live config.
func testUrlProbeRampSettings(t *testing.T, shardCount, concurrency int) providerEgressProbeSettings {
	t.Helper()
	t.Setenv("WARP_DOMAIN", "probe.example")
	pop := server.Config.PushSimpleResource("provider_egress_probe.yml", []byte(fmt.Sprintf(`
enabled: true
shard_count: %d
idle_delay_seconds: 5
max_time_seconds: 900
api_url: https://api.probe.example
platform_url: wss://platform.probe.example
tunnel_recreate_attempts: 2
url_probe_result_version: 1
url_success_interval_seconds: 1200
url_failure_interval_seconds: 60
url_probe:
  limit: %d
  concurrency: %d
  probe_timeout_seconds: 60
`, shardCount, concurrency, concurrency)))
	t.Cleanup(pop)
	settings, err := loadProviderEgressProbeSettings()
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

// Only the three capacity scalars may differ; all policy/default fields survive.
func TestUrlProbeRampLoaderPreservesPolicyAndPrivateBudget(t *testing.T) {
	baseline := testUrlProbeRampSettings(t, 4, 8)
	settings := testUrlProbeRampSettings(t, 8, 64)
	if baseline.ShardCount*baseline.UrlProbe.Concurrency == 512 {
		t.Fatal("baseline unexpectedly already supplies ramp capacity")
	}
	baseline.ShardCount = 8
	baseline.UrlProbe.Limit, baseline.UrlProbe.Concurrency = 64, 64
	if !reflect.DeepEqual(settings, baseline) {
		t.Fatal("capacity ramp changed a non-capacity setting or default")
	}
	allArgs := allProviderEgressProbeArgs(settings)
	if len(allArgs) != 8 {
		t.Fatalf("shard snapshots=%d, want eight", len(allArgs))
	}
	for index, args := range allArgs {
		if args.ShardIndex != index || args.ShardCount != 8 || args.UrlProbe == nil ||
			args.UrlProbe.Limit != 64 || args.UrlProbe.Concurrency != 64 || args.UrlProbe.ProbeTimeoutSeconds != 60 ||
			args.MaxTimeSeconds != 900 || args.IdleDelaySeconds != 5 || args.TunnelRecreateAttempts != 2 ||
			args.UrlProbeResultVersion != 1 || args.UrlSuccessIntervalSeconds != 1200 || args.UrlFailureIntervalSeconds != 60 {
			t.Fatal("ramp snapshot lost shard affinity, bounded geometry, or URL policy")
		}
		if args.UrlProbe.Bandwidth || args.UrlProbe.AllDestinations || args.UrlProbe.IpEchoTimeoutSeconds != 0 ||
			args.UrlProbe.TransportBudgetByteCount != 0 || args.UrlProbe.TransportBudgetCount != 0 {
			t.Fatal("ramp enabled retired measurements or changed private budget defaults")
		}
		if err := validateProviderEgressProbeArgs(args); err != nil {
			t.Fatal(err)
		}
		if !providerEgressProbeArgsMatchSettings(args, settings) {
			t.Fatal("current ramp snapshot is incorrectly stale")
		}
		if got := providerUrlProbeRunBudget(args); got != 220*time.Second || got+3*providerEgressControlPlaneTimeout != 310*time.Second {
			t.Fatalf("run/publication reserve changed: run=%s", got)
		}
		if got, err := providerUrlProbeCreditMinimum(args); err != nil || got != 2*1024*1024*1024*1024 {
			t.Fatalf("ordinary credit geometry changed: got=%d error=%v", got, err)
		}
	}
	first := providerEgressProbeTunnelConfig(providertunnel.Config{}, *settings.UrlProbe)
	second := providerEgressProbeTunnelConfig(providertunnel.Config{}, *settings.UrlProbe)
	for _, config := range []providertunnel.Config{first, second} {
		limits := config.PlatformTransportBudget.Stats()
		if limits.TotalByteCount != 16*1024*1024 || limits.MaxTransportCount != 16 {
			t.Fatalf("default owner budget changed: bytes=%d slots=%d", limits.TotalByteCount, limits.MaxTransportCount)
		}
	}
	if first.PlatformTransportBudget == second.PlatformTransportBudget {
		t.Fatal("independent owners share mutable admission")
	}
	allArgs[0].UrlProbe.Concurrency = 1
	if allArgs[1].UrlProbe.Concurrency != 64 || settings.UrlProbe.Concurrency != 64 {
		t.Fatal("one snapshot mutation changed a sibling or its settings")
	}
	invalid := providerEgressProbeArgs(settings, 0)
	invalid.UrlProbe.Limit = 63
	if validateProviderEgressProbeArgs(invalid) == nil {
		t.Fatal("limit below active concurrency was accepted")
	}
	invalid = providerEgressProbeArgs(settings, 0)
	invalid.UrlProbe.TransportBudgetByteCount = 16 * 1024 * 1024
	if validateProviderEgressProbeArgs(invalid) == nil {
		t.Fatal("one-sided owner budget override was accepted")
	}
}

// Both the old shard count and old per-shard pool must retire before execution.
func TestUrlProbeRampRetiresOldGeometryBeforeNetwork(t *testing.T) {
	old := testUrlProbeRampSettings(t, 4, 8)
	current := testUrlProbeRampSettings(t, 8, 64)
	withProviderEgressProbeSettings(t, current)
	driftTaskRunner(t, func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
		t.Fatal("old capacity reached network execution")
		return nil, nil
	})
	for _, settings := range []providerEgressProbeSettings{old, current} {
		args := providerEgressProbeArgs(settings, 0)
		args.UrlProbe.Limit, args.UrlProbe.Concurrency = 8, 8
		result, err := ProviderEgressProbe(args, driftTaskSession(t))
		if err != nil || result == nil || !result.Stale {
			t.Fatalf("old geometry did not retire: result=%+v error=%v", result, err)
		}
	}
}

// An instance-owned barrier holds the real sink, after provisional reporting.
type urlProbeRampPublication struct {
	*recordingEgressProbeIngest
	acknowledge <-chan struct{}
}

// Keep publication pending without opening a network endpoint.
func (self *urlProbeRampPublication) SubmitEgressHealth(ctx context.Context, clientId string, result *egresshealth.Result) error {
	if clientId == "synthetic-ramp-00" {
		<-self.acknowledge
	}
	return self.recordingEgressProbeIngest.SubmitEgressHealth(ctx, clientId, result)
}

// A turn remains occupied through joined close and acknowledged publication.
// Releasing one owner refills one slot while sixty-three unrelated owners wait.
func TestUrlProbeRampRefillsOneOfSixtyFourAfterCloseAndPublication(t *testing.T) {
	settings := testUrlProbeRampSettings(t, 8, 64)
	synctest.Test(t, func(t *testing.T) {
		pass, _, inner := testUrlProbePass()
		args := providerEgressProbeArgs(settings, 7)
		firstHealth, firstClose, remaining := make(chan struct{}), make(chan struct{}), make(chan struct{})
		acknowledge := make(chan struct{})
		pass.fullSink = testFullBatchSink(&urlProbeRampPublication{
			recordingEgressProbeIngest: inner,
			acknowledge:                acknowledge,
		})
		var started, active, peak, closed atomic.Int64
		requests := []int{}
		next := 0
		pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			requests = append(requests, limit)
			if limit < 1 || limit > 64 {
				return nil, fmt.Errorf("due request exceeded free capacity: %d", limit)
			}
			var due []ingest.DueProvider
			for len(due) < limit && next < 65 {
				due = append(due, ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-ramp-%02d", next)})
				next++
			}
			return due, nil
		}
		pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			if len(providers) != 1 || options.Concurrency != 1 || !options.UrlProbe || options.LoadAttempts != 1 || options.AllDestinations {
				return prober.Summary{}, fmt.Errorf("URL turn lost one-provider/one-attempt geometry")
			}
			first := providers[0].ClientId == "synthetic-ramp-00"
			owner := &prober.Prober{
				Open: func(context.Context, string) (*http.Client, func() error, error) {
					started.Add(1)
					current := active.Add(1)
					for previous := peak.Load(); previous < current && !peak.CompareAndSwap(previous, current); previous = peak.Load() {
					}
					return &http.Client{}, func() error {
						if first {
							<-firstClose
						}
						active.Add(-1)
						closed.Add(1)
						return nil
					}, nil
				},
				Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
					if first {
						<-firstHealth
					} else {
						<-remaining
					}
					return testHealthRun(1, 1), nil
				},
				HealthResults: options.HealthResults,
				Attempts:      options.Attempts,
			}
			return (&prober.Scheduler{Prober: owner, Concurrency: options.Concurrency}).Run(ctx, providers), nil
		}
		ctx, cancel := context.WithTimeout(t.Context(), 900*time.Second)
		defer cancel()
		var result *ProviderEgressProbeResult
		var runErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			result, runErr = pass.run(ctx, args)
		}()
		synctest.Wait()
		if started.Load() != 64 || active.Load() != 64 || len(requests) != 1 || requests[0] != 64 {
			t.Errorf("initial admission: started=%d active=%d requests=%v", started.Load(), active.Load(), requests)
		}
		close(firstHealth)
		synctest.Wait()
		if started.Load() != 64 || closed.Load() != 0 || len(requests) != 1 {
			t.Error("turn released its slot before its close joined")
		}
		close(firstClose)
		synctest.Wait()
		if started.Load() != 64 || closed.Load() != 1 || len(requests) != 1 {
			t.Error("turn released its slot before actual publication acknowledged")
		}
		close(acknowledge)
		synctest.Wait()
		inner.stateLock.Lock()
		health, published := inner.health["synthetic-ramp-00"]
		acknowledged := slices.Contains(inner.calls, "attempt synthetic-ramp-00 ")
		inner.stateLock.Unlock()
		if started.Load() != 65 || active.Load() != 64 || closed.Load() != 1 || len(requests) != 2 || requests[1] != 1 ||
			!published || health.OkCount != 1 || !acknowledged {
			t.Errorf("single-slot refill/publication: started=%d active=%d closed=%d requests=%v published=%t acknowledged=%t",
				started.Load(), active.Load(), closed.Load(), requests, published, acknowledged)
		}
		close(remaining)
		<-done
		if runErr != nil || result == nil || result.Attempted != 65 || result.Submitted != 65 ||
			result.UrlDue != 65 || peak.Load() != 64 || active.Load() != 0 || closed.Load() != 65 {
			t.Fatalf("bounded refill lost ownership or accepted work: result=%+v error=%v peak=%d active=%d closed=%d",
				result, runErr, peak.Load(), active.Load(), closed.Load())
		}
	})
}
