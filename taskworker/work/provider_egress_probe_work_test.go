package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/operator-proxy/fleetprobe"
	"github.com/urnetwork/operator-proxy/ingest"
	"github.com/urnetwork/operator-proxy/prober"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func testProviderEgressProbeSettings(shardCount int) providerEgressProbeSettings {
	settings := defaultProviderEgressProbeSettings("bringyour.com")
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
		if args.APIURL != "https://api.bringyour.com" {
			t.Fatalf("shard %d api url = %q", args.ShardIndex, args.APIURL)
		}
		if args.PlatformURL != "wss://connect.bringyour.com" {
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
	t.Setenv("WARP_DOMAIN", "bringyour.com")
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

func TestProviderEgressProbePassRunsBothSchedulesWithOnePinSnapshot(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(4), 2)
	events := []string{}
	pins := map[string][]string{"source.example": {"leaf", "intermediate"}}
	pass := &providerEgressProbePass{
		blackholeDue: func(_ context.Context, limit int) ([]string, error) {
			events = append(events, "blackhole-due")
			if limit != args.Blackhole.Limit {
				t.Fatalf("blackhole limit = %d, want %d", limit, args.Blackhole.Limit)
			}
			return []string{"blackhole-1", "blackhole-2"}, nil
		},
		fullDue: func(_ context.Context, limit int) ([]string, error) {
			events = append(events, "full-due")
			if limit != args.Full.Limit {
				t.Fatalf("full limit = %d, want %d", limit, args.Full.Limit)
			}
			return []string{"full-1"}, nil
		},
		loadPins: func(context.Context) (map[string][]string, error) {
			events = append(events, "pins")
			return pins, nil
		},
		submitBlackholeChecks: func(_ context.Context, checks []ingest.BlackholeCheck) error {
			events = append(events, "blackhole-submit")
			if len(checks) != 2 {
				t.Fatalf("submitted blackhole checks = %d, want 2", len(checks))
			}
			return nil
		},
		runBlackhole: func(_ context.Context, clientIds []string, options fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			events = append(events, "blackhole-run")
			if !slices.Equal(clientIds, []string{"blackhole-1", "blackhole-2"}) {
				t.Fatalf("blackhole client ids = %v", clientIds)
			}
			if options.Concurrency != args.Blackhole.Concurrency || options.Timeout != time.Duration(args.Blackhole.ProbeTimeoutSeconds)*time.Second {
				t.Fatalf("blackhole options = %+v", options)
			}
			if options.Pins == nil || !slices.Equal(options.Pins()["source.example"], []string{"leaf", "intermediate"}) {
				t.Fatalf("blackhole pins = %v", options.Pins)
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
			events = append(events, "full-run")
			if !slices.Equal(clientIds, []string{"full-1"}) {
				t.Fatalf("full client ids = %v", clientIds)
			}
			if options.Concurrency != args.Full.Concurrency || options.ProbeTimeout != time.Duration(args.Full.ProbeTimeoutSeconds)*time.Second {
				t.Fatalf("full options = %+v", options)
			}
			if options.Pins == nil || !slices.Equal(options.Pins()["source.example"], []string{"leaf", "intermediate"}) {
				t.Fatalf("full pins = %v", options.Pins)
			}
			return prober.Summary{Attempted: 1, Submitted: 1}, nil
		},
	}

	result, err := pass.run(context.Background(), args)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wantEvents := []string{
		"blackhole-due",
		"full-due",
		"pins",
		"blackhole-run",
		"blackhole-submit",
		"full-run",
	}
	if !slices.Equal(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
	if result.BlackholeDue != 2 || result.Checked != 2 || result.Dark != 1 || result.TunnelFailed != 1 {
		t.Fatalf("blackhole result = %+v", result)
	}
	if result.FullDue != 1 || result.Attempted != 1 || result.Submitted != 1 {
		t.Fatalf("full result = %+v", result)
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
			t.Fatal("a failed blackhole run must not submit")
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
	}

	result, err := pass.run(context.Background(), args)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Full || result.FullDue != 0 || result.BlackholeDue != 0 {
		t.Fatalf("idle result = %+v", result)
	}
}

func TestProviderEgressProbePassPropagatesCancellationFromFullProbe(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	}

	result, err := pass.run(ctx, args)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want context cancellation", err)
	}
	if result.Attempted != 1 {
		t.Fatalf("cancelled full result = %+v", result)
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
			`, "github.com/urnetwork/server/taskworker/work.ProviderEgressProbe")
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
