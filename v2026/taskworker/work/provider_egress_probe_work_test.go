package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/operator-proxy/v2026/fleetprobe"
	"github.com/urnetwork/operator-proxy/v2026/ingest"
	"github.com/urnetwork/operator-proxy/v2026/prober"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func testProviderEgressProbeSettings(shardCount int) providerEgressProbeSettings {
	settings := defaultProviderEgressProbeSettings("example.test")
	settings.ShardCount = shardCount
	return settings
}

func withProviderEgressProbeSettings(
	t testing.TB,
	settings providerEgressProbeSettings,
) {
	t.Helper()
	previous := getProviderEgressProbeSettings
	getProviderEgressProbeSettings = func() (providerEgressProbeSettings, error) {
		return settings, nil
	}
	t.Cleanup(func() {
		getProviderEgressProbeSettings = previous
	})
}

func TestProviderEgressProbeArgsCoverEveryShardExactlyOnce(t *testing.T) {
	settings := testProviderEgressProbeSettings(5)
	allArgs := allProviderEgressProbeArgs(settings)
	if len(allArgs) != 5 {
		t.Fatalf("task args = %d, want exactly 5 shards", len(allArgs))
	}

	seen := map[int]bool{}
	for _, args := range allArgs {
		if seen[args.ShardIndex] {
			t.Fatalf("duplicate task args for shard %d", args.ShardIndex)
		}
		seen[args.ShardIndex] = true
		if args.ShardCount != settings.ShardCount {
			t.Fatalf("shard %d carries count %d, want %d", args.ShardIndex, args.ShardCount, settings.ShardCount)
		}
		if args.APIURL != "https://api.example.test" {
			t.Fatalf("shard %d api url = %q", args.ShardIndex, args.APIURL)
		}
		if args.PlatformURL != "wss://connect.example.test" {
			t.Fatalf("shard %d platform url = %q", args.ShardIndex, args.PlatformURL)
		}
		if args.Full.Limit == 0 || args.Blackhole.Limit == 0 {
			t.Fatalf("shard %d does not carry both probe batches: %+v", args.ShardIndex, args)
		}
	}
	for shardIndex := range settings.ShardCount {
		if !seen[shardIndex] {
			t.Fatalf("missing task args for shard %d/%d", shardIndex, settings.ShardCount)
		}
	}
}

func TestProviderEgressProbeSettingsLoadTaskArgumentsFromConfig(t *testing.T) {
	t.Setenv("WARP_DOMAIN", "example.test")
	pop := server.Config.PushSimpleResource("provider_egress_probe.yml", []byte(`
shard_count: 7
idle_delay_seconds: 123
max_time_seconds: 456
api_url: https://api.example.test
platform_url: wss://connect.example.test
full:
  limit: 9
  concurrency: 3
blackhole:
  limit: 70
  concurrency: 7
`))
	t.Cleanup(pop)

	settings, err := loadProviderEgressProbeSettings()
	if err != nil {
		t.Fatalf("loadProviderEgressProbeSettings: %v", err)
	}
	if settings.ShardCount != 7 || settings.IdleDelaySeconds != 123 || settings.MaxTimeSeconds != 456 {
		t.Fatalf("task settings = %+v", settings)
	}
	if !settings.Enabled {
		t.Fatal("an existing config without enabled lost the deployed default true")
	}
	if settings.Full.Limit != 9 || settings.Full.Concurrency != 3 {
		t.Fatalf("full settings = %+v", settings.Full)
	}
	if settings.Blackhole.Limit != 70 || settings.Blackhole.Concurrency != 7 {
		t.Fatalf("blackhole settings = %+v", settings.Blackhole)
	}
	if settings.APIURL != "https://api.example.test" || settings.PlatformURL != "wss://connect.example.test" {
		t.Fatalf("control endpoints = %q %q", settings.APIURL, settings.PlatformURL)
	}
	if settings.Full.ProbeTimeoutSeconds != 60 {
		t.Fatalf("unspecified full timeout lost its default: %+v", settings.Full)
	}
}

// Explicit disablement must remain valid even when every operational field is
// absent. A simulation should not have to carry usable production endpoints or
// batch geometry for a subsystem that is forbidden from running.
func TestProviderEgressProbeDisabledSettingsSkipOperationalValidation(t *testing.T) {
	settings := providerEgressProbeSettings{Enabled: false}
	if err := settings.validate(); err != nil {
		t.Fatalf("disabled settings: %v", err)
	}
	settings.Enabled = true
	if err := settings.validate(); err == nil {
		t.Fatal("enabled empty settings passed operational validation")
	}
}

func TestProviderEgressProbeExecutesArbitraryCurrentArgsUnchanged(t *testing.T) {
	settings := testProviderEgressProbeSettings(4)
	withProviderEgressProbeSettings(t, settings)

	args := providerEgressProbeArgs(settings, 2)
	args.Full.Limit = 7
	args.Full.Concurrency = 1
	args.APIURL = "https://api.example.test"

	previous := executeProviderEgressProbe
	called := false
	executeProviderEgressProbe = func(_ context.Context, gotArgs *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
		called = true
		if gotArgs != args {
			t.Fatal("task replaced the persisted argument object before execution")
		}
		return &ProviderEgressProbeResult{FullDue: 7, Full: true}, nil
	}
	t.Cleanup(func() {
		executeProviderEgressProbe = previous
	})

	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result, err := ProviderEgressProbe(args, clientSession)
	if err != nil {
		t.Fatalf("ProviderEgressProbe: %v", err)
	}
	if !called || !result.Full || result.FullDue != 7 {
		t.Fatalf("runner called=%t result=%+v", called, result)
	}
}

func TestProviderEgressProbeRetiresStaleShardGeometryWithoutNetworkWork(t *testing.T) {
	settings := testProviderEgressProbeSettings(3)
	withProviderEgressProbeSettings(t, settings)

	previous := executeProviderEgressProbe
	called := false
	executeProviderEgressProbe = func(_ context.Context, _ *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
		called = true
		return nil, nil
	}
	t.Cleanup(func() {
		executeProviderEgressProbe = previous
	})

	args := providerEgressProbeArgs(testProviderEgressProbeSettings(4), 2)
	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result, err := ProviderEgressProbe(args, clientSession)
	if err != nil {
		t.Fatalf("ProviderEgressProbe: %v", err)
	}
	if !result.Stale {
		t.Fatalf("stale shard result = %+v", result)
	}
	if called {
		t.Fatal("stale shard geometry ran provider network work")
	}
}

// A worker may have claimed an old row just before a deploy disables probing.
// The disabled check must precede even persisted-argument validation: reaching
// executeProviderEgressProbe would read the prober identity and Vault secret
// and construct external clients.
func TestProviderEgressProbeDisabledRetiresClaimedWorkBeforeExecution(t *testing.T) {
	settings := testProviderEgressProbeSettings(3)
	settings.Enabled = false
	withProviderEgressProbeSettings(t, settings)

	previous := executeProviderEgressProbe
	called := false
	executeProviderEgressProbe = func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
		called = true
		return nil, errors.New("disabled probe execution reached")
	}
	t.Cleanup(func() {
		executeProviderEgressProbe = previous
	})

	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result, err := ProviderEgressProbe(nil, clientSession)
	if err != nil {
		t.Fatalf("ProviderEgressProbe: %v", err)
	}
	if result == nil || !result.Stale {
		t.Fatalf("disabled result = %+v, want retired work", result)
	}
	if called {
		t.Fatal("disabled worker entered credential/network execution")
	}
}

// The disabled fast path must not weaken validation when probing is enabled.
func TestProviderEgressProbeEnabledStillRejectsInvalidPersistedArguments(t *testing.T) {
	settings := testProviderEgressProbeSettings(3)
	withProviderEgressProbeSettings(t, settings)

	previous := executeProviderEgressProbe
	called := false
	executeProviderEgressProbe = func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
		called = true
		return nil, nil
	}
	t.Cleanup(func() {
		executeProviderEgressProbe = previous
	})

	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	if _, err := ProviderEgressProbe(nil, clientSession); err == nil {
		t.Fatal("enabled worker accepted missing persisted arguments")
	}
	if called {
		t.Fatal("invalid enabled work reached credential/network execution")
	}
}

func TestProviderEgressProbePassRunsBothSchedulesWithOnePinSnapshot(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(4), 2)
	var eventsLock sync.Mutex
	events := []string{}
	recordEvent := func(event string) {
		eventsLock.Lock()
		defer eventsLock.Unlock()
		events = append(events, event)
	}
	pins := map[string][]string{"source.example": {"leaf", "intermediate"}}
	pass := &providerEgressProbePass{
		blackholeDue: func(_ context.Context, limit int) ([]string, error) {
			recordEvent("blackhole-due")
			if limit != args.Blackhole.Limit {
				t.Fatalf("blackhole limit = %d, want %d", limit, args.Blackhole.Limit)
			}
			return []string{"blackhole-1", "blackhole-2"}, nil
		},
		fullDue: func(_ context.Context, limit int) ([]string, error) {
			recordEvent("full-due")
			if limit != args.Full.Limit {
				t.Fatalf("full limit = %d, want %d", limit, args.Full.Limit)
			}
			return []string{"full-1"}, nil
		},
		loadPins: func(context.Context) (map[string][]string, error) {
			recordEvent("pins")
			return pins, nil
		},
		submitBlackholeChecks: func(_ context.Context, checks []ingest.BlackholeCheck) error {
			recordEvent("blackhole-submit")
			if len(checks) != 2 {
				t.Errorf("submitted blackhole checks = %d, want 2", len(checks))
			}
			return nil
		},
		runBlackhole: func(_ context.Context, clientIds []string, options fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			recordEvent("blackhole-run")
			if !slices.Equal(clientIds, []string{"blackhole-1", "blackhole-2"}) {
				t.Errorf("blackhole client ids = %v", clientIds)
			}
			wantConcurrency := args.Blackhole.Concurrency - args.Full.Concurrency
			if options.Concurrency != wantConcurrency || options.Timeout != time.Duration(args.Blackhole.ProbeTimeoutSeconds)*time.Second {
				t.Errorf("blackhole options = %+v, want concurrency %d", options, wantConcurrency)
			}
			if options.Pins == nil || !slices.Equal(options.Pins()["source.example"], []string{"leaf", "intermediate"}) {
				t.Errorf("blackhole pins = %v", options.Pins)
			}
			return fleetprobe.BlackholeSummary{
				Checks: []ingest.BlackholeCheck{
					{ClientId: "blackhole-1"},
					{ClientId: "blackhole-2"},
				},
				Dark:         1,
				TunnelFailed: 1,
			}, nil
		},
		runFull: func(_ context.Context, clientIds []string, options fleetprobe.FullOptions) (prober.Summary, error) {
			recordEvent("full-run")
			if !slices.Equal(clientIds, []string{"full-1"}) {
				t.Errorf("full client ids = %v", clientIds)
			}
			if options.Concurrency != args.Full.Concurrency || options.ProbeTimeout != time.Duration(args.Full.ProbeTimeoutSeconds)*time.Second {
				t.Errorf("full options = %+v", options)
			}
			if options.Pins == nil || !slices.Equal(options.Pins()["source.example"], []string{"leaf", "intermediate"}) {
				t.Errorf("full pins = %v", options.Pins)
			}
			return prober.Summary{Attempted: 1, Submitted: 1}, nil
		},
	}

	result, err := pass.run(context.Background(), args)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	eventsLock.Lock()
	gotEvents := slices.Clone(events)
	eventsLock.Unlock()
	if len(gotEvents) != 6 || !slices.Equal(gotEvents[:3], []string{"blackhole-due", "full-due", "pins"}) {
		t.Fatalf("events = %v, want ordered lookups/pins followed by three lane events", gotEvents)
	}
	eventIndex := map[string]int{}
	for index, event := range gotEvents {
		eventIndex[event] = index
	}
	if eventIndex["blackhole-run"] < 3 || eventIndex["full-run"] < 3 || eventIndex["blackhole-submit"] <= eventIndex["blackhole-run"] {
		t.Fatalf("lane events = %v, want both runs and blackhole submit after its run", gotEvents[3:])
	}
	if result.BlackholeDue != 2 || result.Checked != 2 || result.Dark != 1 || result.TunnelFailed != 1 {
		t.Fatalf("blackhole result = %+v", result)
	}
	if result.FullDue != 1 || result.Attempted != 1 || result.Submitted != 1 {
		t.Fatalf("full result = %+v", result)
	}
}

func TestProviderEgressProbePassDrainsBlackholeWhileFullBatchIsBlocked(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	args.Blackhole.Limit = 1
	args.Blackhole.Concurrency = 32
	args.Full.Limit = 1
	args.Full.Concurrency = 2

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fullStarted := make(chan struct{})
	releaseFull := make(chan struct{})
	secondBlackholeSubmitted := make(chan struct{})
	blackholeDueCalls := 0
	blackholeRuns := 0
	blackholeSubmissions := 0
	pinLoads := 0
	pass := &providerEgressProbePass{
		blackholeDue: func(context.Context, int) ([]string, error) {
			blackholeDueCalls++
			switch blackholeDueCalls {
			case 1:
				return []string{"blackhole-a"}, nil
			case 2:
				return []string{"blackhole-b"}, nil
			default:
				return nil, nil
			}
		},
		fullDue: func(context.Context, int) ([]string, error) {
			return []string{"full-a"}, nil
		},
		loadPins: func(context.Context) (map[string][]string, error) {
			pinLoads++
			return map[string][]string{"source.example": {"leaf", "intermediate"}}, nil
		},
		runFull: func(ctx context.Context, _ []string, options fleetprobe.FullOptions) (prober.Summary, error) {
			if options.Concurrency != 2 {
				t.Errorf("full concurrency = %d, want 2", options.Concurrency)
			}
			close(fullStarted)
			select {
			case <-releaseFull:
				return prober.Summary{Attempted: 1, Submitted: 1}, nil
			case <-ctx.Done():
				return prober.Summary{}, ctx.Err()
			}
		},
		runBlackhole: func(ctx context.Context, clientIds []string, options fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			if options.Concurrency != 30 {
				t.Errorf("blackhole concurrency = %d, want 30 reserved beside the 2-slot full pool", options.Concurrency)
			}
			select {
			case <-fullStarted:
			case <-ctx.Done():
				return fleetprobe.BlackholeSummary{}, ctx.Err()
			}
			blackholeRuns++
			return fleetprobe.BlackholeSummary{
				Checks: []ingest.BlackholeCheck{{ClientId: clientIds[0]}},
			}, nil
		},
		submitBlackholeChecks: func(context.Context, []ingest.BlackholeCheck) error {
			blackholeSubmissions++
			if blackholeSubmissions == 2 {
				close(secondBlackholeSubmitted)
			}
			return nil
		},
	}

	type runOutcome struct {
		result *ProviderEgressProbeResult
		err    error
	}
	done := make(chan runOutcome, 1)
	go func() {
		result, err := pass.run(ctx, args)
		done <- runOutcome{result: result, err: err}
	}()

	select {
	case <-secondBlackholeSubmitted:
		close(releaseFull)
	case <-time.After(2 * time.Second):
		cancel()
		close(releaseFull)
		<-done
		t.Fatal("a blocked full batch prevented the second blackhole batch")
	}
	outcome := <-done
	if outcome.err != nil {
		t.Fatalf("run: %v", outcome.err)
	}
	if pinLoads != 1 {
		t.Fatalf("pin snapshots = %d, want 1", pinLoads)
	}
	if blackholeDueCalls < 2 || blackholeRuns != 2 || blackholeSubmissions != 2 {
		t.Fatalf("blackhole due=%d runs=%d submissions=%d, want at least 2/2/2", blackholeDueCalls, blackholeRuns, blackholeSubmissions)
	}
	if outcome.result.BlackholeDue != 2 || outcome.result.Checked != 2 {
		t.Fatalf("drained blackhole result = %+v, want two batches", outcome.result)
	}
	if outcome.result.FullDue != 1 || outcome.result.Attempted != 1 || outcome.result.Submitted != 1 {
		t.Fatalf("concurrent full result = %+v", outcome.result)
	}
}

func TestProviderEgressProbePassKeepsOneBlackholeBatchWhenFullIsNotDue(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	args.Blackhole.Limit = 1
	blackholeDueCalls := 0
	pass := &providerEgressProbePass{
		blackholeDue: func(context.Context, int) ([]string, error) {
			blackholeDueCalls++
			return []string{"blackhole-a"}, nil
		},
		fullDue: func(context.Context, int) ([]string, error) {
			return nil, nil
		},
		loadPins: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"source.example": {"leaf", "intermediate"}}, nil
		},
		runBlackhole: func(context.Context, []string, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			return fleetprobe.BlackholeSummary{
				Checks: []ingest.BlackholeCheck{{ClientId: "blackhole-a"}},
			}, nil
		},
		submitBlackholeChecks: func(context.Context, []ingest.BlackholeCheck) error {
			return nil
		},
	}

	result, err := pass.run(context.Background(), args)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if blackholeDueCalls != 1 {
		t.Fatalf("blackhole due lookups = %d, want one without a concurrent full batch", blackholeDueCalls)
	}
	if result.BlackholeDue != 1 || result.Checked != 1 || !result.Full {
		t.Fatalf("one-batch result = %+v", result)
	}
}

func TestProviderEgressProbePassDoesNotLetBlackholeFailureStarveFullProbe(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	blackholeErr := errors.New("blackhole transport failed")
	fullRan := false
	pass := &providerEgressProbePass{
		blackholeDue: func(context.Context, int) ([]string, error) {
			return []string{"blackhole-1"}, nil
		},
		fullDue: func(context.Context, int) ([]string, error) {
			return []string{"full-1"}, nil
		},
		loadPins: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"source.example": {"leaf", "intermediate"}}, nil
		},
		submitBlackholeChecks: func(context.Context, []ingest.BlackholeCheck) error {
			t.Error("a failed blackhole run must not submit")
			return nil
		},
		runBlackhole: func(context.Context, []string, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			return fleetprobe.BlackholeSummary{}, blackholeErr
		},
		runFull: func(context.Context, []string, fleetprobe.FullOptions) (prober.Summary, error) {
			fullRan = true
			return prober.Summary{Attempted: 1, Submitted: 1}, nil
		},
	}

	result, err := pass.run(context.Background(), args)
	if !errors.Is(err, blackholeErr) {
		t.Fatalf("run error = %v, want blackhole failure", err)
	}
	if !fullRan {
		t.Fatal("full probe did not run after the independent blackhole batch failed")
	}
	if result.Attempted != 1 || result.Submitted != 1 {
		t.Fatalf("full result after blackhole failure = %+v", result)
	}
}

func TestProviderEgressProbePassDoesNotLetBlackholeSubmissionFailureStarveFullProbe(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	submitErr := errors.New("blackhole submission failed")
	fullRan := false
	pass := &providerEgressProbePass{
		blackholeDue: func(context.Context, int) ([]string, error) {
			return []string{"blackhole-1"}, nil
		},
		fullDue: func(context.Context, int) ([]string, error) {
			return []string{"full-1"}, nil
		},
		loadPins: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"source.example": {"leaf", "intermediate"}}, nil
		},
		submitBlackholeChecks: func(context.Context, []ingest.BlackholeCheck) error {
			return submitErr
		},
		runBlackhole: func(context.Context, []string, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			return fleetprobe.BlackholeSummary{
				Checks: []ingest.BlackholeCheck{{ClientId: "blackhole-1"}},
			}, nil
		},
		runFull: func(context.Context, []string, fleetprobe.FullOptions) (prober.Summary, error) {
			fullRan = true
			return prober.Summary{Attempted: 1, Submitted: 1}, nil
		},
	}

	result, err := pass.run(context.Background(), args)
	if !errors.Is(err, submitErr) {
		t.Fatalf("run error = %v, want blackhole submission failure", err)
	}
	if !fullRan {
		t.Fatal("full probe did not run after the independent blackhole submission failed")
	}
	if result.Checked != 1 || result.Attempted != 1 || result.Submitted != 1 {
		t.Fatalf("combined result after blackhole submission failure = %+v", result)
	}
}

func TestProviderEgressProbePassDoesNotLetFullDueFailureStarveBlackholeProbe(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	fullDueErr := errors.New("full due lookup failed")
	blackholeSubmitted := false
	pass := &providerEgressProbePass{
		blackholeDue: func(context.Context, int) ([]string, error) {
			return []string{"blackhole-1"}, nil
		},
		fullDue: func(context.Context, int) ([]string, error) {
			return nil, fullDueErr
		},
		loadPins: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"source.example": {"leaf", "intermediate"}}, nil
		},
		submitBlackholeChecks: func(_ context.Context, checks []ingest.BlackholeCheck) error {
			blackholeSubmitted = len(checks) == 1
			return nil
		},
		runBlackhole: func(context.Context, []string, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			return fleetprobe.BlackholeSummary{
				Checks: []ingest.BlackholeCheck{{ClientId: "blackhole-1"}},
			}, nil
		},
	}

	result, err := pass.run(context.Background(), args)
	if !errors.Is(err, fullDueErr) {
		t.Fatalf("run error = %v, want full due failure", err)
	}
	if !blackholeSubmitted || result.Checked != 1 {
		t.Fatalf("blackhole work was starved by full due failure: submitted=%t result=%+v", blackholeSubmitted, result)
	}
}

func TestProviderEgressProbePassDoesNotLoadPinsWhenNothingIsDue(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	refreshes := 0
	pass := &providerEgressProbePass{
		blackholeDue: func(context.Context, int) ([]string, error) {
			return nil, nil
		},
		fullDue: func(context.Context, int) ([]string, error) {
			return nil, nil
		},
		loadPins: func(context.Context) (map[string][]string, error) {
			t.Fatal("an idle shard loaded certificate pins")
			return nil, nil
		},
		refreshFleet: func(context.Context) { refreshes++ },
	}

	result, err := pass.run(context.Background(), args)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Full || result.FullDue != 0 || result.BlackholeDue != 0 {
		t.Fatalf("idle result = %+v", result)
	}
	if refreshes != 1 {
		t.Fatalf("idle fleet refreshes = %d, want 1", refreshes)
	}
}

func TestProviderEgressProbePassDoesNotLaunchBatchesCanceledWhileLoadingPins(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	ctx, cancel := context.WithCancel(context.Background())
	pass := &providerEgressProbePass{
		blackholeDue: func(context.Context, int) ([]string, error) {
			return []string{"blackhole-a"}, nil
		},
		fullDue: func(context.Context, int) ([]string, error) {
			return []string{"full-a"}, nil
		},
		loadPins: func(context.Context) (map[string][]string, error) {
			cancel()
			return map[string][]string{"source.example": {"leaf", "intermediate"}}, nil
		},
		runBlackhole: func(context.Context, []string, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			t.Error("canceled pass launched blackhole work")
			return fleetprobe.BlackholeSummary{}, nil
		},
		runFull: func(context.Context, []string, fleetprobe.FullOptions) (prober.Summary, error) {
			t.Error("canceled pass launched full work")
			return prober.Summary{}, nil
		},
	}

	result, err := pass.run(ctx, args)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want context cancellation", err)
	}
	if result.BlackholeDue != 1 || result.Checked != 0 || result.Attempted != 0 {
		t.Fatalf("canceled pre-launch result = %+v", result)
	}
}

func TestProviderEgressProbePassPropagatesCancellationFromFullProbe(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refreshes := 0
	pass := &providerEgressProbePass{
		blackholeDue: func(context.Context, int) ([]string, error) {
			return nil, nil
		},
		fullDue: func(context.Context, int) ([]string, error) {
			return []string{"full-1"}, nil
		},
		loadPins: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"source.example": {"leaf", "intermediate"}}, nil
		},
		runFull: func(context.Context, []string, fleetprobe.FullOptions) (prober.Summary, error) {
			cancel()
			return prober.Summary{Attempted: 1}, nil
		},
		refreshFleet: func(context.Context) { refreshes++ },
	}

	result, err := pass.run(ctx, args)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want context cancellation", err)
	}
	if result.Attempted != 1 {
		t.Fatalf("cancelled full result = %+v", result)
	}
	if refreshes != 0 {
		t.Fatalf("canceled pass refreshed a partial fleet snapshot %d time(s)", refreshes)
	}
}

func TestScheduleProviderEgressProbeTasksIsIdempotentAndHostIndependent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		settings := testProviderEgressProbeSettings(3)
		withProviderEgressProbeSettings(t, settings)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleProviderEgressProbeTasks(clientSession, tx)
			ScheduleProviderEgressProbeTasks(clientSession, tx)
		})

		type scheduled struct {
			ArgsJson     []byte
			RunOnceKey   string
			MaxTime      int
			ClientByJson *string
		}
		scheduledTasks := []scheduled{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT args_json, run_once_key, run_max_time_seconds, client_by_jwt_json
				FROM pending_task
				WHERE function_name = $1
				ORDER BY run_once_key
			`, "github.com/urnetwork/server/v2026/taskworker/work.ProviderEgressProbe")
			server.WithPgResult(result, err, func() {
				for result.Next() {
					row := scheduled{}
					server.Raise(result.Scan(&row.ArgsJson, &row.RunOnceKey, &row.MaxTime, &row.ClientByJson))
					scheduledTasks = append(scheduledTasks, row)
				}
			})
		})
		if len(scheduledTasks) != 3 {
			t.Fatalf("pending probe tasks = %d, want exactly 3 shards", len(scheduledTasks))
		}

		seen := map[int]bool{}
		for _, scheduledTask := range scheduledTasks {
			var args ProviderEgressProbeArgs
			if err := json.Unmarshal(scheduledTask.ArgsJson, &args); err != nil {
				t.Fatalf("decode args: %v", err)
			}
			if seen[args.ShardIndex] {
				t.Fatalf("duplicate pending task for shard %d", args.ShardIndex)
			}
			seen[args.ShardIndex] = true
			wantRunOnceKey := fmt.Sprintf("[\"provider_egress_probe\",%d]", args.ShardIndex)
			if scheduledTask.RunOnceKey != wantRunOnceKey {
				t.Fatalf("shard %d run-once key = %q, want %q", args.ShardIndex, scheduledTask.RunOnceKey, wantRunOnceKey)
			}
			if scheduledTask.MaxTime != args.MaxTimeSeconds {
				t.Fatalf("shard %d max time = %ds, args carry %ds", args.ShardIndex, scheduledTask.MaxTime, args.MaxTimeSeconds)
			}
			if scheduledTask.ClientByJson != nil {
				t.Fatalf("shard %d is tied to a scheduling account: %q", args.ShardIndex, *scheduledTask.ClientByJson)
			}
		}
	})
}

// Disabled initialization must not create a new recurring chain.
func TestScheduleProviderEgressProbeTasksDisabledSeedsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		settings := testProviderEgressProbeSettings(3)
		settings.Enabled = false
		withProviderEgressProbeSettings(t, settings)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleProviderEgressProbeTasks(clientSession, tx)
		})

		var count int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT count(*)
				FROM pending_task
				WHERE function_name = $1
			`, ProviderEgressProbeTaskFunctionNames()[0])
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&count))
				}
			})
		})
		if count != 0 {
			t.Fatalf("disabled scheduler seeded %d probe tasks", count)
		}
	})
}

func TestProviderEgressProbePostImmediatelyContinuesAFullBatch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		settings := testProviderEgressProbeSettings(2)
		withProviderEgressProbeSettings(t, settings)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		args := providerEgressProbeArgs(settings, 1)
		before := server.NowUtc()

		server.Tx(ctx, func(tx server.PgTx) {
			if err := ProviderEgressProbePost(args, &ProviderEgressProbeResult{Full: true}, clientSession, tx); err != nil {
				t.Fatalf("ProviderEgressProbePost: %v", err)
			}
		})

		runAt := providerEgressProbeRunAt(t, ctx, args.ShardIndex)
		if runAt.Before(before.Add(-time.Second)) || before.Add(5*time.Second).Before(runAt) {
			t.Fatalf("full-batch successor run_at = %s, want immediate after %s", runAt, before)
		}
	})
}

func TestProviderEgressProbePostRepeatsAnIdleShardAfterItsCadence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		settings := testProviderEgressProbeSettings(2)
		withProviderEgressProbeSettings(t, settings)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		args := providerEgressProbeArgs(settings, 0)
		before := server.NowUtc()

		server.Tx(ctx, func(tx server.PgTx) {
			if err := ProviderEgressProbePost(args, &ProviderEgressProbeResult{}, clientSession, tx); err != nil {
				t.Fatalf("ProviderEgressProbePost: %v", err)
			}
		})

		runAt := providerEgressProbeRunAt(t, ctx, args.ShardIndex)
		want := before.Add(time.Duration(args.IdleDelaySeconds) * time.Second)
		if runAt.Before(want.Add(-time.Second)) || want.Add(5*time.Second).Before(runAt) {
			t.Fatalf("idle successor run_at = %s, want about %s", runAt, want)
		}
	})
}

func TestProviderEgressProbePostConvergesAChangedShardCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		currentSettings := testProviderEgressProbeSettings(3)
		withProviderEgressProbeSettings(t, currentSettings)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		oldArgs := providerEgressProbeArgs(testProviderEgressProbeSettings(4), 2)

		server.Tx(ctx, func(tx server.PgTx) {
			if err := ProviderEgressProbePost(oldArgs, &ProviderEgressProbeResult{Stale: true}, clientSession, tx); err != nil {
				t.Fatalf("ProviderEgressProbePost: %v", err)
			}
		})

		var argsJson []byte
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT args_json
				FROM pending_task
				WHERE run_once_key = '["provider_egress_probe",2]'
			`)
			server.WithPgResult(result, err, func() {
				if !result.Next() {
					t.Fatal("stale shard within the new range did not schedule its current replacement")
				}
				server.Raise(result.Scan(&argsJson))
			})
		})
		var nextArgs ProviderEgressProbeArgs
		if err := json.Unmarshal(argsJson, &nextArgs); err != nil {
			t.Fatalf("decode next args: %v", err)
		}
		if nextArgs.ShardCount != 3 || nextArgs.ShardIndex != 2 {
			t.Fatalf("replacement shard = %d/%d, want 2/3", nextArgs.ShardIndex, nextArgs.ShardCount)
		}
	})
}

// A capacity-only rollout keeps the durable shard keys stable. The currently
// claimed row completes with its immutable arguments, then its post-step must
// snapshot the new worker-pool size into the successor without requiring a
// second task chain or a manual pending_task edit.
func TestProviderEgressProbePostConvergesChangedBatchSettings(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		currentSettings := testProviderEgressProbeSettings(4)
		currentSettings.Blackhole.Concurrency = 32
		withProviderEgressProbeSettings(t, currentSettings)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		oldSettings := testProviderEgressProbeSettings(4)
		oldSettings.Blackhole.Concurrency = 4
		oldArgs := providerEgressProbeArgs(oldSettings, 2)

		server.Tx(ctx, func(tx server.PgTx) {
			if err := ProviderEgressProbePost(oldArgs, &ProviderEgressProbeResult{Full: true}, clientSession, tx); err != nil {
				t.Fatalf("ProviderEgressProbePost: %v", err)
			}
		})

		var argsJSON []byte
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT args_json
				FROM pending_task
				WHERE run_once_key = '["provider_egress_probe",2]'
			`)
			server.WithPgResult(result, err, func() {
				if !result.Next() {
					t.Fatal("capacity rollout did not schedule the shard successor")
				}
				server.Raise(result.Scan(&argsJSON))
			})
		})
		var nextArgs ProviderEgressProbeArgs
		if err := json.Unmarshal(argsJSON, &nextArgs); err != nil {
			t.Fatalf("decode next args: %v", err)
		}
		if nextArgs.ShardCount != 4 || nextArgs.ShardIndex != 2 {
			t.Fatalf("capacity rollout changed shard geometry: %d/%d", nextArgs.ShardIndex, nextArgs.ShardCount)
		}
		if nextArgs.Blackhole.Concurrency != 32 {
			t.Fatalf("successor blackhole concurrency = %d, want 32", nextArgs.Blackhole.Concurrency)
		}
		if oldArgs.Blackhole.Concurrency != 4 {
			t.Fatalf("post-step mutated claimed args to %d, want immutable 4", oldArgs.Blackhole.Concurrency)
		}
	})
}

func TestProviderEgressProbePostRetiresAShardRemovedByConfiguration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		currentSettings := testProviderEgressProbeSettings(3)
		withProviderEgressProbeSettings(t, currentSettings)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		oldArgs := providerEgressProbeArgs(testProviderEgressProbeSettings(4), 3)

		server.Tx(ctx, func(tx server.PgTx) {
			if err := ProviderEgressProbePost(oldArgs, &ProviderEgressProbeResult{Stale: true}, clientSession, tx); err != nil {
				t.Fatalf("ProviderEgressProbePost: %v", err)
			}
		})

		var count int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT count(*)
				FROM pending_task
				WHERE run_once_key = '["provider_egress_probe",3]'
			`)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&count))
				}
			})
		})
		if count != 0 {
			t.Fatalf("removed shard scheduled %d successors, want zero", count)
		}
	})
}

// A task that completed while disablement was racing must not recreate the
// chain. Nil old arguments/results force the disabled guard to be first.
func TestProviderEgressProbePostDisabledSchedulesNoSuccessor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		settings := testProviderEgressProbeSettings(3)
		settings.Enabled = false
		withProviderEgressProbeSettings(t, settings)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		server.Tx(ctx, func(tx server.PgTx) {
			if err := ProviderEgressProbePost(nil, nil, clientSession, tx); err != nil {
				t.Fatalf("ProviderEgressProbePost: %v", err)
			}
		})

		var count int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT count(*)
				FROM pending_task
				WHERE function_name = $1
			`, ProviderEgressProbeTaskFunctionNames()[0])
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&count))
				}
			})
		})
		if count != 0 {
			t.Fatalf("disabled post-step scheduled %d successors", count)
		}
	})
}

func providerEgressProbeRunAt(t testing.TB, ctx context.Context, shardIndex int) time.Time {
	t.Helper()
	runOnceKey := fmt.Sprintf("[\"provider_egress_probe\",%d]", shardIndex)
	var runAt time.Time
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT run_at FROM pending_task WHERE run_once_key = $1`, runOnceKey)
		server.WithPgResult(result, err, func() {
			if !result.Next() {
				t.Fatalf("no successor for %s", runOnceKey)
			}
			server.Raise(result.Scan(&runAt))
		})
	})
	return runAt
}
