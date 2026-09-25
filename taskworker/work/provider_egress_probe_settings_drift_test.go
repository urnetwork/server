package work

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

func driftTaskSession(t testing.TB) *session.ClientSession {
	t.Helper()
	s := session.NewLocalClientSession(context.Background(), "192.0.2.1:1234", nil)
	t.Cleanup(s.Cancel)
	return s
}

func driftTaskRunner(t testing.TB, run func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error)) {
	t.Helper()
	previous := executeProviderEgressProbe
	executeProviderEgressProbe = run
	t.Cleanup(func() { executeProviderEgressProbe = previous })
}

// The stale row must reach successful finalization even when its old network
// execution would repeatedly return the task's deadline error.
func TestProviderEgressProbeSettingsDriftRetiresOldCapacityBeforeNetwork(t *testing.T) {
	settings := testProviderEgressProbeSettings(4)
	settings.Blackhole.Concurrency = 250
	withProviderEgressProbeSettings(t, settings)
	old := providerEgressProbeArgs(settings, 1)
	old.Blackhole.Concurrency = 16
	before, _ := json.Marshal(old)
	calls := 0
	driftTaskRunner(t, func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
		calls++
		return nil, context.DeadlineExceeded
	})
	result, err := ProviderEgressProbe(old, driftTaskSession(t))
	if err != nil || result == nil || !result.Stale || calls != 0 {
		t.Fatalf("stale capacity executed: result=%+v error=%v calls=%d", result, err, calls)
	}
	after, _ := json.Marshal(old)
	if string(before) != string(after) {
		t.Fatal("retirement mutated durable arguments")
	}
}

// Homogeneous changes share one invariant: every valid execution-affecting
// field, including slice contents rather than addresses, retires the snapshot.
func TestProviderEgressProbeSettingsDriftCoversMaterialFields(t *testing.T) {
	settings := testProviderEgressProbeSettings(4)
	settings.MaxTimeSeconds = 24000
	withProviderEgressProbeSettings(t, settings)
	calls := 0
	driftTaskRunner(t, func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
		calls++
		return &ProviderEgressProbeResult{}, nil
	})
	changes := []struct {
		name   string
		change func(*ProviderEgressProbeArgs)
	}{
		{name: "idle", change: func(a *ProviderEgressProbeArgs) { a.IdleDelaySeconds++ }},
		{name: "lease", change: func(a *ProviderEgressProbeArgs) { a.MaxTimeSeconds++ }},
		{name: "full", change: func(a *ProviderEgressProbeArgs) { a.Full.Concurrency = 1 }},
		{name: "blackhole", change: func(a *ProviderEgressProbeArgs) { a.Blackhole.ProbeTimeoutSeconds++ }},
		{name: "transport_budget", change: func(a *ProviderEgressProbeArgs) {
			a.Blackhole.TransportBudgetByteCount = 32 * 1024 * 1024
			a.Blackhole.TransportBudgetCount = 32
		}},
		{name: "api", change: func(a *ProviderEgressProbeArgs) { a.APIURL = "https://alternate-api.example" }},
		{name: "platform", change: func(a *ProviderEgressProbeArgs) { a.PlatformURL = "wss://alternate-platform.example" }},
		{name: "public_api", change: func(a *ProviderEgressProbeArgs) { a.PublicAPIURL = "https://alternate-public.example" }},
		{name: "bandwidth", change: func(a *ProviderEgressProbeArgs) { a.BandwidthCDNURL = "https://alternate-bandwidth.example" }},
		{name: "load_attempts", change: func(a *ProviderEgressProbeArgs) { a.LoadAttempts++ }},
		{name: "load_spacing", change: func(a *ProviderEgressProbeArgs) { a.LoadRetryMeanIntervalSeconds++ }},
		{name: "tunnel_recreation", change: func(a *ProviderEgressProbeArgs) { a.TunnelRecreateAttempts++ }},
		{name: "dark_failures", change: func(a *ProviderEgressProbeArgs) { a.DarkConsecutiveFailures++ }},
		{name: "dark_span", change: func(a *ProviderEgressProbeArgs) { a.DarkMinimumSpanSeconds++ }},
		{name: "dark_backoff", change: func(a *ProviderEgressProbeArgs) { a.DarkBackoffSeconds[0]++ }},
		{name: "dark_guard", change: func(a *ProviderEgressProbeArgs) { a.DarkBatchGuard = 0.5 }},
		{name: "dark_minimum", change: func(a *ProviderEgressProbeArgs) { a.DarkBatchGuardMinChecks++ }},
		{name: "run_guard", change: func(a *ProviderEgressProbeArgs) { a.RunBatchGuard = 0.5 }},
		{name: "run_minimum", change: func(a *ProviderEgressProbeArgs) { a.RunBatchGuardMinRuns++ }},
		{name: "city_radius", change: func(a *ProviderEgressProbeArgs) { a.CityConfidentRadiusKm++ }},
	}
	for _, c := range changes {
		args := providerEgressProbeArgs(settings, 1)
		c.change(args)
		if err := validateProviderEgressProbeArgs(args); err != nil {
			t.Fatalf("invalid synthetic %s: %v", c.name, err)
		}
		before := calls
		result, err := ProviderEgressProbe(args, driftTaskSession(t))
		if err != nil || result == nil || !result.Stale || calls != before {
			t.Errorf("%s ran stale work: result=%+v error=%v calls=%d", c.name, result, err, calls-before)
		}
	}
}

func TestProviderEgressProbeSettingsDriftKeepsMatchingCustomSnapshot(t *testing.T) {
	settings := testProviderEgressProbeSettings(4)
	settings.Full.Limit = 7
	settings.Full.Concurrency = 1
	settings.APIURL = "https://custom-api.example"
	settings.Blackhole.Concurrency = 250
	withProviderEgressProbeSettings(t, settings)
	args := providerEgressProbeArgs(settings, 2)
	want := &ProviderEgressProbeResult{Full: true, FullDue: 7}
	calls := 0
	driftTaskRunner(t, func(_ context.Context, got *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
		calls++
		if got != args {
			t.Fatal("matching args replaced")
		}
		return want, nil
	})
	result, err := ProviderEgressProbe(args, driftTaskSession(t))
	if err != nil || result != want || calls != 1 {
		t.Fatalf("current custom snapshot altered: result=%+v error=%v calls=%d", result, err, calls)
	}
	if !reflect.DeepEqual(args.DarkBackoffSeconds, settings.DarkBackoffSeconds) {
		t.Fatal("rules mutated")
	}
}

func TestProviderEgressProbeSettingsDriftPreservesCurrentExecutionError(t *testing.T) {
	settings := testProviderEgressProbeSettings(4)
	withProviderEgressProbeSettings(t, settings)
	driftTaskRunner(t, func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
		return nil, context.DeadlineExceeded
	})
	argsJSON, err := json.Marshal(providerEgressProbeArgs(settings, 0))
	if err != nil {
		t.Fatal(err)
	}
	target := task.NewTaskTargetWithPost(ProviderEgressProbe, ProviderEgressProbePost)
	_, post, err := target.RunSpecific(context.Background(), &task.Task{ArgsJson: string(argsJSON), ClientAddress: "192.0.2.1:1234", RunMaxTimeSeconds: settings.MaxTimeSeconds})
	if !errors.Is(err, context.DeadlineExceeded) || post != nil {
		t.Fatalf("current execution failure was falsely retired: error=%v has_post=%t", err, post != nil)
	}
}

func TestProviderEgressProbeSettingsDriftPreservesInvalidArgumentError(t *testing.T) {
	settings := testProviderEgressProbeSettings(4)
	withProviderEgressProbeSettings(t, settings)
	driftTaskRunner(t, func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
		t.Fatal("invalid work executed")
		return nil, nil
	})
	args := providerEgressProbeArgs(settings, 0)
	args.Blackhole.Concurrency = 0
	result, err := ProviderEgressProbe(args, driftTaskSession(t))
	if err == nil || result != nil {
		t.Fatal("invalid snapshot was masked as stale success")
	}
}

func TestProviderEgressProbeSettingsDriftPreservesSettingsUnavailable(t *testing.T) {
	previous := getProviderEgressProbeSettings
	want := errors.New("synthetic settings unavailable")
	getProviderEgressProbeSettings = func() (providerEgressProbeSettings, error) { return providerEgressProbeSettings{}, want }
	t.Cleanup(func() { getProviderEgressProbeSettings = previous })
	driftTaskRunner(t, func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
		t.Fatal("unobservable settings executed")
		return nil, nil
	})
	result, err := ProviderEgressProbe(providerEgressProbeArgs(testProviderEgressProbeSettings(4), 0), driftTaskSession(t))
	if result != nil || err != want {
		t.Fatal("settings failure was masked")
	}
}

// Uses the actual task evaluator, Post, RunOnce insert and PG decoder. No
// timeout sleeps, provider network, duplicate task chain or manual row edit.
func TestProviderEgressProbeSettingsDriftFinalizesCurrentSuccessor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		settings := testProviderEgressProbeSettings(4)
		settings.Blackhole.Concurrency = 250
		withProviderEgressProbeSettings(t, settings)
		old := providerEgressProbeArgs(settings, 2)
		old.Blackhole.Concurrency = 16
		calls := 0
		driftTaskRunner(t, func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
			calls++
			return nil, context.DeadlineExceeded
		})
		oldJSON, err := json.Marshal(old)
		if err != nil {
			t.Fatal(err)
		}
		target := task.NewTaskTargetWithPost(ProviderEgressProbe, ProviderEgressProbePost)
		before := server.NowUtc()
		result, post, err := target.RunSpecific(context.Background(), &task.Task{ArgsJson: string(oldJSON), ClientAddress: "192.0.2.1:1234", RunMaxTimeSeconds: old.MaxTimeSeconds})
		if err != nil || post == nil || result == nil || !result.Stale || calls != 0 {
			t.Fatalf("stale task cannot finalize: result=%+v error=%v has_post=%t calls=%d", result, err, post != nil, calls)
		}
		ctx := context.Background()
		server.Tx(ctx, func(tx server.PgTx) {
			if err := post(tx); err != nil {
				t.Fatal(err)
			}
		})
		var nextJSON []byte
		var runAt time.Time
		var maxTime int
		rows := 0
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT args_json,run_at,run_max_time_seconds FROM pending_task WHERE run_once_key='["provider_egress_probe",2]'`)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					rows++
					server.Raise(result.Scan(&nextJSON, &runAt, &maxTime))
				}
			})
		})
		var next ProviderEgressProbeArgs
		if err := json.Unmarshal(nextJSON, &next); err != nil {
			t.Fatal(err)
		}
		if rows != 1 || !reflect.DeepEqual(&next, providerEgressProbeArgs(settings, 2)) || maxTime != settings.MaxTimeSeconds {
			t.Fatalf("successor wrong: rows=%d concurrency=%d max_time=%d", rows, next.Blackhole.Concurrency, maxTime)
		}
		if runAt.Before(before.Add(-time.Second)) || server.NowUtc().Add(time.Second).Before(runAt) {
			t.Fatal("stale successor delayed by idle cadence")
		}
		if old.Blackhole.Concurrency != 16 {
			t.Fatal("old immutable args changed")
		}
	})
}
