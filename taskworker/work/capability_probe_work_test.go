// The capability probe's task without tunnels or a database.
package work

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/capprobe"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Replaces the probe's settings and execution until the test ends.
func withCapabilityProbeSeams(t *testing.T, settings func() (capprobe.Settings, error), execute func(context.Context, capprobe.Settings, capprobe.Sweep, time.Duration) (capprobe.Sweep, *capprobe.PassResult, error)) {
	t.Helper()
	previousSettings, previousExecute := getCapabilityProbeSettings, executeCapabilityProbe
	getCapabilityProbeSettings, executeCapabilityProbe = settings, execute
	t.Cleanup(func() {
		getCapabilityProbeSettings, executeCapabilityProbe = previousSettings, previousExecute
	})
}

// The probe's slot is the shard registry's last, a valid prober shard key,
// and it is refused while the URL probe's configured shards reach it.
func TestCapabilityProbeShardSlotIsApartFromTheUrlProbe(t *testing.T) {
	connect.AssertEqual(t, capabilityProbeShardIndex, 255)
	connect.AssertEqual(t, capabilityProbeShardCount, 256)
	connect.AssertEqual(t, capabilityProbeShardIndex < capabilityProbeShardCount, true)
	for _, c := range []struct {
		urlShardCount int
		available     bool
	}{
		{urlShardCount: defaultProviderEgressProbeShardCount, available: true},
		{urlShardCount: 8, available: true},
		{urlShardCount: capabilityProbeShardIndex, available: true},
		{urlShardCount: capabilityProbeShardIndex + 1, available: false},
		{urlShardCount: maxProviderEgressProbeShardCount, available: false},
	} {
		if err := capabilityProbeShardAvailable(c.urlShardCount); (err == nil) != c.available {
			t.Errorf("url shard_count %d: available %v, want %t", c.urlShardCount, err, c.available)
		}
	}
}

// A pass's private balance covers its geometry ten times over and never
// falls under the shared shard headroom; a larger geometry never gets less.
func TestCapabilityProbeShardCredit(t *testing.T) {
	settings := capprobe.DefaultSettings()
	credit, err := capabilityProbeShardCredit(settings)
	if err != nil {
		t.Fatal(err)
	}
	if credit < model.ProberShardTransferHeadroom {
		t.Fatalf("credit %d is under the shard headroom %d", credit, model.ProberShardTransferHeadroom)
	}
	larger := settings
	larger.Concurrency, larger.Limit = 256, 4096
	largerCredit, err := capabilityProbeShardCredit(larger)
	if err != nil {
		t.Fatal(err)
	}
	if largerCredit < credit {
		t.Fatalf("a larger geometry got less credit: %d < %d", largerCredit, credit)
	}
}

// The next pass: within a sweep after the pass interval, between sweeps at
// the next sweep's start or a waiting pass ten minutes on, whichever is
// first, and after a result-less finish with the pass's own sweep. A
// switched-off probe's pass has no successor: the chain ends.
func TestCapabilityProbeSuccessor(t *testing.T) {
	settings := capprobe.DefaultSettings()
	settings.Enabled = true
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	cursor := server.NewId()
	inSweep := capprobe.Sweep{Cursor: cursor, StartedAt: now.Add(-time.Hour)}
	betweenSweeps := capprobe.Sweep{NextSweepAt: now.Add(5 * time.Hour)}
	for _, c := range []struct {
		name   string
		args   *CapabilityProbeArgs
		result *CapabilityProbeResult
		sweep  capprobe.Sweep
		runAt  time.Time
	}{
		{name: "within a sweep", result: &CapabilityProbeResult{Sweep: inSweep}, sweep: inSweep, runAt: now.Add(settings.PassInterval())},
		{name: "between sweeps", result: &CapabilityProbeResult{Sweep: betweenSweeps}, sweep: betweenSweeps, runAt: now.Add(10 * time.Minute)},
		{name: "just before the next sweep", result: &CapabilityProbeResult{Sweep: capprobe.Sweep{NextSweepAt: now.Add(7 * time.Minute)}}, sweep: capprobe.Sweep{NextSweepAt: now.Add(7 * time.Minute)}, runAt: now.Add(7 * time.Minute)},
		{name: "a next sweep already due", result: &CapabilityProbeResult{Sweep: capprobe.Sweep{NextSweepAt: now.Add(-time.Minute)}}, sweep: capprobe.Sweep{NextSweepAt: now.Add(-time.Minute)}, runAt: now.Add(settings.PassInterval())},
		{name: "no result", args: &CapabilityProbeArgs{Sweep: inSweep}, sweep: inSweep, runAt: now.Add(settings.PassInterval())},
	} {
		args, runAt, scheduled := capabilityProbeSuccessor(c.args, c.result, settings, now)
		if !scheduled || !reflect.DeepEqual(args.Sweep, c.sweep) || !runAt.Equal(c.runAt) {
			t.Errorf("%s: next %+v at %s (scheduled %t), want %+v at %s", c.name, args, runAt, scheduled, c.sweep, c.runAt)
		}
	}
	for _, switchedOff := range []capprobe.Settings{capprobe.DefaultSettings(), settings} {
		if args, runAt, scheduled := capabilityProbeSuccessor(nil, &CapabilityProbeResult{Disabled: true, Sweep: inSweep}, switchedOff, now); scheduled {
			t.Errorf("a switched-off probe's pass scheduled a successor at %s with %+v", runAt, args)
		}
	}
}

// A switched-off probe runs nothing and keeps its sweep's place.
func TestCapabilityProbeTaskRunsNothingWhileDisabled(t *testing.T) {
	disabled := capprobe.DefaultSettings()
	disabled.Enabled = false
	withCapabilityProbeSeams(t,
		func() (capprobe.Settings, error) { return disabled, nil },
		func(context.Context, capprobe.Settings, capprobe.Sweep, time.Duration) (capprobe.Sweep, *capprobe.PassResult, error) {
			t.Fatal("a disabled probe ran a pass")
			return capprobe.Sweep{}, nil, nil
		},
	)
	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	sweep := capprobe.Sweep{Cursor: server.NewId(), StartedAt: server.NowUtc()}
	result, err := CapabilityProbe(&CapabilityProbeArgs{Sweep: sweep}, clientSession)
	if err != nil || result == nil || !result.Disabled || !reflect.DeepEqual(result.Sweep, sweep) {
		t.Fatalf("result %+v err %v", result, err)
	}
}

// An enabled probe hands its pass the sweep and a deadline of the pass's
// budget, and carries the sweep the pass leaves; a failed pass is a task
// error with its cause, retried with the same sweep.
func TestCapabilityProbeTaskCarriesTheSweep(t *testing.T) {
	settings := capprobe.DefaultSettings()
	settings.Enabled = true
	sweep := capprobe.Sweep{Cursor: server.NewId(), StartedAt: server.NowUtc()}
	next := capprobe.Sweep{Cursor: server.NewId(), StartedAt: sweep.StartedAt}
	passErr := errors.New("synthetic pass failure")
	fail := false
	withCapabilityProbeSeams(t,
		func() (capprobe.Settings, error) { return settings, nil },
		func(ctx context.Context, gotSettings capprobe.Settings, gotSweep capprobe.Sweep, maxTime time.Duration) (capprobe.Sweep, *capprobe.PassResult, error) {
			deadline, bounded := ctx.Deadline()
			if !bounded || time.Until(deadline) > settings.MaxTime() || time.Until(deadline) < settings.MaxTime()-time.Minute || maxTime != settings.MaxTime() {
				t.Errorf("the pass's deadline is %s away and its budget %s, want about %s", time.Until(deadline), maxTime, settings.MaxTime())
			}
			if !reflect.DeepEqual(gotSweep, sweep) || gotSettings != settings {
				t.Errorf("the pass got %+v and %+v", gotSweep, gotSettings)
			}
			if fail {
				return gotSweep, nil, passErr
			}
			return next, &capprobe.PassResult{Started: 1, Recorded: 1}, nil
		},
	)
	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result, err := CapabilityProbe(&CapabilityProbeArgs{Sweep: sweep}, clientSession)
	if err != nil || !reflect.DeepEqual(result.Sweep, next) || result.Pass == nil || result.Pass.Recorded != 1 {
		t.Fatalf("result %+v err %v", result, err)
	}
	fail = true
	result, err = CapabilityProbe(&CapabilityProbeArgs{Sweep: sweep}, clientSession)
	if !errors.Is(err, passErr) || result != nil {
		t.Fatalf("a failed pass returned %+v, %v", result, err)
	}
}

// Settings that cannot be read fail the pass, never the defaults.
func TestCapabilityProbeTaskFailsOnUnreadableSettings(t *testing.T) {
	settingsErr := errors.New("synthetic unreadable settings")
	withCapabilityProbeSeams(t,
		func() (capprobe.Settings, error) { return capprobe.Settings{}, settingsErr },
		func(context.Context, capprobe.Settings, capprobe.Sweep, time.Duration) (capprobe.Sweep, *capprobe.PassResult, error) {
			t.Fatal("a pass ran without its settings")
			return capprobe.Sweep{}, nil, nil
		},
	)
	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	if _, err := CapabilityProbe(&CapabilityProbeArgs{}, clientSession); !errors.Is(err, settingsErr) {
		t.Fatalf("unreadable settings returned %v", err)
	}
	connect.AssertEqual(t, capabilityProbeSettingsOrDefault(), capprobe.DefaultSettings())
}

// The value of one of the capability probe's counters, by its name and its
// labels, from the registry the probe registers with.
func capabilityProbeCounterValue(t testing.TB, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			matched := len(metric.GetLabel()) == len(labels)
			for _, pair := range metric.GetLabel() {
				if labels[pair.GetName()] != pair.GetValue() {
					matched = false
				}
			}
			if matched {
				return metric.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("no counter %s %v", name, labels)
	return 0
}

// Root cause (review F3): a pass that FinishPass finished, whose prober
// shard's drain or reap then failed, returned the old sweep with the cleanup
// error. The task retried it a minute later, measuring every provider of the
// pass again, those without evidence at once, and the pass counted both
// measured and failed. A finished pass stands with its own sweep and result,
// and the cleanup failure is counted apart; a pass that failed before
// FinishPass leaves the sweep, counted once.
//
// Root cause (re-review finding 5): a pass that FinishPass failed returned
// the old sweep here too, discarding the held downgrades FinishPass returned
// with its error. It returns FinishPass's sweep, which keeps the pass's
// place, and its failure is counted once, by FinishPass.
func TestCapabilityProbeKeepsAFinishedPassWhoseShardCleanupFailed(t *testing.T) {
	sweep := capprobe.Sweep{Cursor: server.NewId(), StartedAt: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	finishedSweep := capprobe.Sweep{Cursor: server.NewId(), StartedAt: sweep.StartedAt}
	// the same place, with the held downgrades as a failed pass left them
	failedSweep := capprobe.Sweep{Cursor: sweep.Cursor, StartedAt: sweep.StartedAt, HeldDowngrades: []capprobe.HeldDowngrade{{ClientId: server.NewId(), SweepStartedAt: sweep.StartedAt}}}
	pass := &capprobe.PassResult{Scanned: 4, Due: 3, Eligible: 2, Started: 2, Recorded: 1}
	finishedOutcome := capabilityProbeOutcome{sweep: finishedSweep, pass: pass}
	failedOutcome := capabilityProbeOutcome{sweep: failedSweep, pass: pass}
	cleanupErr := &proberShardCleanupError{err: errors.New("capability probe shard cleanup: synthetic reap failure")}
	finishErr := errors.New("synthetic evidence write failure")
	readinessErr := errors.New("synthetic readiness failure")
	failedLabels := map[string]string{"result": capprobe.PassResultFailed}
	for _, c := range []struct {
		name      string
		outcome   capabilityProbeOutcome
		finished  bool
		finishErr error
		err       error
		sweep     capprobe.Sweep
		returnErr error
		failed    float64
		cleanup   float64
	}{
		{name: "finished, cleanup failed", outcome: finishedOutcome, finished: true, err: fmt.Errorf("wrapped: %w", cleanupErr), sweep: finishedSweep, failed: 0, cleanup: 1},
		{name: "finished, cleaned up", outcome: finishedOutcome, finished: true, sweep: finishedSweep},
		{name: "finish failed", outcome: failedOutcome, finished: true, finishErr: finishErr, err: finishErr, sweep: failedSweep, returnErr: finishErr},
		{name: "finish failed, cleanup failed", outcome: failedOutcome, finished: true, finishErr: finishErr, err: errors.Join(finishErr, cleanupErr), sweep: failedSweep, returnErr: finishErr, failed: 0, cleanup: 1},
		{name: "failed before finish", outcome: finishedOutcome, err: readinessErr, sweep: sweep, returnErr: readinessErr, failed: 1},
		{name: "failed before finish, cleanup failed", outcome: finishedOutcome, err: errors.Join(readinessErr, cleanupErr), sweep: sweep, returnErr: readinessErr, failed: 1, cleanup: 1},
	} {
		failedBefore := capabilityProbeCounterValue(t, "urnetwork_capability_probe_passes_total", failedLabels)
		cleanupBefore := capabilityProbeCounterValue(t, "urnetwork_capability_probe_shard_cleanup_errors_total", map[string]string{})
		next, result, err := capabilityProbeShardPassResult(sweep, c.outcome, c.finished, c.finishErr, c.err)
		if !reflect.DeepEqual(next, c.sweep) || result != pass {
			t.Errorf("%s: returned sweep %+v and %+v, want %+v and the pass's result", c.name, next, result, c.sweep)
		}
		if c.returnErr == nil && err != nil || c.returnErr != nil && !errors.Is(err, c.returnErr) {
			t.Errorf("%s: returned %v, want %v", c.name, err, c.returnErr)
		}
		if failed := capabilityProbeCounterValue(t, "urnetwork_capability_probe_passes_total", failedLabels) - failedBefore; failed != c.failed {
			t.Errorf("%s: counted %v failed passes, want %v", c.name, failed, c.failed)
		}
		if cleanup := capabilityProbeCounterValue(t, "urnetwork_capability_probe_shard_cleanup_errors_total", map[string]string{}) - cleanupBefore; cleanup != c.cleanup {
			t.Errorf("%s: counted %v cleanup failures, want %v", c.name, cleanup, c.cleanup)
		}
	}
}

// Root cause (review F9): slot 255 was reserved on one side only. The URL
// probe's settings and arguments accepted shard_count 256, whose last shard
// takes slot 255, and the capability probe then refused every pass while a
// URL shard and it would have retired each other's owner. The URL probe's
// settings, its URL arguments and its legacy arguments now refuse a
// shard_count that reaches the capability probe's slot, accept the largest
// that does not, and every geometry they accept leaves the slot free.
func TestUrlProbeShardCountStopsBeforeTheCapabilityProbeSlot(t *testing.T) {
	for _, c := range []struct {
		shardCount int
		valid      bool
	}{
		{shardCount: capabilityProbeShardIndex, valid: true},
		{shardCount: capabilityProbeShardIndex + 1, valid: false},
	} {
		settings := defaultProviderEgressProbeSettings("probe.example")
		settings.ShardCount = c.shardCount
		urlArgs := providerEgressProbeArgs(settings, 0)
		legacyArgs := providerEgressProbeArgs(settings, 0)
		legacyArgs.UrlProbe = nil
		// a legacy pass's own floor, apart from the shard count
		legacyArgs.MaxTimeSeconds = int((providerEgressProbeMinMaxTime(legacyArgs) + time.Second - 1) / time.Second)
		for _, check := range []struct {
			name string
			err  error
		}{
			{name: "settings", err: settings.validate()},
			{name: "URL arguments", err: validateProviderEgressProbeArgsConfig(urlArgs)},
			{name: "legacy arguments", err: validateProviderEgressProbeArgsConfig(legacyArgs)},
		} {
			if (check.err == nil) != c.valid {
				t.Errorf("shard_count %d: %s validate = %v, want valid %t", c.shardCount, check.name, check.err, c.valid)
			}
		}
		if c.valid {
			if err := capabilityProbeShardAvailable(c.shardCount); err != nil {
				t.Errorf("an accepted URL shard_count %d reaches the capability probe's slot: %v", c.shardCount, err)
			}
		}
	}
}

// Root cause (review F10): a pass's deadline came from the settings it read
// when it ran, while its task row's max time came from those read when it
// was scheduled. With max_time_seconds raised in between, the pass would
// admit measurements past the task's max time, which cancels it. The max
// time a pass is scheduled with rides in its arguments, and the pass's
// deadline and budget are that whatever the settings say when it runs. A
// row scheduled without one, or with one no shard could live for, runs on
// the settings' own; a switched-off probe's row takes the default.
func TestCapabilityProbePassRunsWithItsScheduledMaxTime(t *testing.T) {
	scheduledSettings := capprobe.DefaultSettings()
	scheduledSettings.Enabled = true
	raised := scheduledSettings
	raised.MaxTimeSeconds = 2 * scheduledSettings.MaxTimeSeconds
	sweep := capprobe.Sweep{Cursor: server.NewId(), StartedAt: server.NowUtc()}
	scheduledArgs, rowMaxTime := capabilityProbeScheduled(&CapabilityProbeArgs{Sweep: sweep}, scheduledSettings)
	if !reflect.DeepEqual(scheduledArgs.Sweep, sweep) || rowMaxTime != scheduledSettings.MaxTime() || time.Duration(scheduledArgs.MaxTimeSeconds)*time.Second != rowMaxTime {
		t.Fatalf("scheduled %+v with a row max time of %s, want the sweep and %s in both", scheduledArgs, rowMaxTime, scheduledSettings.MaxTime())
	}
	disabled := raised
	disabled.Enabled = false
	if disabledArgs, disabledMaxTime := capabilityProbeScheduled(nil, disabled); disabledMaxTime != capprobe.DefaultSettings().MaxTime() || time.Duration(disabledArgs.MaxTimeSeconds)*time.Second != disabledMaxTime {
		t.Fatalf("a switched-off probe scheduled %+v with %s, want the default max time", disabledArgs, disabledMaxTime)
	}

	var deadline time.Time
	var passMaxTime time.Duration
	withCapabilityProbeSeams(t,
		func() (capprobe.Settings, error) { return raised, nil },
		func(ctx context.Context, settings capprobe.Settings, gotSweep capprobe.Sweep, maxTime time.Duration) (capprobe.Sweep, *capprobe.PassResult, error) {
			deadline, _ = ctx.Deadline()
			passMaxTime = maxTime
			return gotSweep, &capprobe.PassResult{}, nil
		},
	)
	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	for _, c := range []struct {
		name    string
		args    *CapabilityProbeArgs
		maxTime time.Duration
	}{
		{name: "scheduled", args: scheduledArgs, maxTime: rowMaxTime},
		{name: "scheduled without one", args: &CapabilityProbeArgs{Sweep: sweep}, maxTime: raised.MaxTime()},
		{name: "scheduled past a shard's lifetime", args: &CapabilityProbeArgs{Sweep: sweep, MaxTimeSeconds: 25 * 60 * 60}, maxTime: raised.MaxTime()},
	} {
		startedAt := time.Now()
		if _, err := CapabilityProbe(c.args, clientSession); err != nil {
			t.Fatal(err)
		}
		if passMaxTime != c.maxTime || deadline.Before(startedAt.Add(c.maxTime)) || deadline.After(time.Now().Add(c.maxTime)) {
			t.Errorf("%s: the pass ran with a budget of %s and a deadline %s after its start, want %s", c.name, passMaxTime, deadline.Sub(startedAt), c.maxTime)
		}
	}
}

// Root cause (review F11): a capability pass's funding readiness check is
// the URL probe's, and a failure counted in the URL probe's
// urnetwork_egress_probe_pass_errors_total, so a capability probe without
// funding read as a URL probe failing. Each failure step of a capability
// pass counts once in the capability probe's own readiness failures and
// never in the URL probe's pass errors.
func TestCapabilityProbeReadinessFailuresAreItsOwn(t *testing.T) {
	for _, c := range []struct {
		step      string
		available func(context.Context) (model.ByteCount, error)
		err       error
	}{
		{step: "funding_unknown", available: func(context.Context) (model.ByteCount, error) {
			return 0, errors.New("synthetic balance read failure")
		}, err: errProviderEgressProbeFundingUnknown},
		{step: "funding_unavailable", available: func(context.Context) (model.ByteCount, error) {
			return 0, nil
		}, err: errProviderEgressProbeUnfunded},
	} {
		readiness := newCapabilityProbeReadiness(server.NewId())
		readiness.available = c.available
		labels := map[string]string{"step": c.step}
		capabilityBefore := capabilityProbeCounterValue(t, "urnetwork_capability_probe_readiness_failures_total", labels)
		urlBefore := testutil.ToFloat64(egressProbePassErrorsTotal.WithLabelValues(c.step))
		for range 2 {
			if err := readiness.check(context.Background()); !errors.Is(err, c.err) {
				t.Fatalf("%s: check returned %v, want %v", c.step, err, c.err)
			}
		}
		if counted := capabilityProbeCounterValue(t, "urnetwork_capability_probe_readiness_failures_total", labels) - capabilityBefore; counted != 1 {
			t.Errorf("%s: the capability probe counted %v readiness failures, want 1", c.step, counted)
		}
		if counted := testutil.ToFloat64(egressProbePassErrorsTotal.WithLabelValues(c.step)) - urlBefore; counted != 0 {
			t.Errorf("%s: the URL probe's pass errors counted %v of the capability probe's failures", c.step, counted)
		}
	}
}
