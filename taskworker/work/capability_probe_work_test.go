// The capability probe's task without tunnels or a database.
package work

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/capprobe"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Replaces the probe's settings and execution until the test ends.
func withCapabilityProbeSeams(t *testing.T, settings func() (capprobe.Settings, error), execute func(context.Context, capprobe.Settings, capprobe.Sweep) (capprobe.Sweep, *capprobe.PassResult, error)) {
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
// first, a switched-off probe after its recheck, and after a result-less
// finish with the pass's own sweep.
func TestCapabilityProbeSuccessor(t *testing.T) {
	settings := capprobe.DefaultSettings()
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
		{name: "switched off", result: &CapabilityProbeResult{Disabled: true, Sweep: inSweep}, sweep: inSweep, runAt: now.Add(capabilityProbeDisabledRecheck)},
		{name: "no result", args: &CapabilityProbeArgs{Sweep: inSweep}, sweep: inSweep, runAt: now.Add(settings.PassInterval())},
	} {
		args, runAt := capabilityProbeSuccessor(c.args, c.result, settings, now)
		if args.Sweep != c.sweep || !runAt.Equal(c.runAt) {
			t.Errorf("%s: next %+v at %s, want %+v at %s", c.name, args.Sweep, runAt, c.sweep, c.runAt)
		}
	}
}

// A switched-off probe runs nothing and keeps its sweep's place.
func TestCapabilityProbeTaskRunsNothingWhileDisabled(t *testing.T) {
	disabled := capprobe.DefaultSettings()
	disabled.Enabled = false
	withCapabilityProbeSeams(t,
		func() (capprobe.Settings, error) { return disabled, nil },
		func(context.Context, capprobe.Settings, capprobe.Sweep) (capprobe.Sweep, *capprobe.PassResult, error) {
			t.Fatal("a disabled probe ran a pass")
			return capprobe.Sweep{}, nil, nil
		},
	)
	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	sweep := capprobe.Sweep{Cursor: server.NewId(), StartedAt: server.NowUtc()}
	result, err := CapabilityProbe(&CapabilityProbeArgs{Sweep: sweep}, clientSession)
	if err != nil || result == nil || !result.Disabled || result.Sweep != sweep {
		t.Fatalf("result %+v err %v", result, err)
	}
}

// An enabled probe hands its pass the sweep and a deadline of the pass's
// budget, and carries the sweep the pass leaves; a failed pass is a task
// error with its cause, retried with the same sweep.
func TestCapabilityProbeTaskCarriesTheSweep(t *testing.T) {
	settings := capprobe.DefaultSettings()
	sweep := capprobe.Sweep{Cursor: server.NewId(), StartedAt: server.NowUtc()}
	next := capprobe.Sweep{Cursor: server.NewId(), StartedAt: sweep.StartedAt}
	passErr := errors.New("synthetic pass failure")
	fail := false
	withCapabilityProbeSeams(t,
		func() (capprobe.Settings, error) { return settings, nil },
		func(ctx context.Context, gotSettings capprobe.Settings, gotSweep capprobe.Sweep) (capprobe.Sweep, *capprobe.PassResult, error) {
			deadline, bounded := ctx.Deadline()
			if !bounded || time.Until(deadline) > settings.MaxTime() || time.Until(deadline) < settings.MaxTime()-time.Minute {
				t.Errorf("the pass's deadline is %s away, want about %s", time.Until(deadline), settings.MaxTime())
			}
			if gotSweep != sweep || gotSettings != settings {
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
	if err != nil || result.Sweep != next || result.Pass == nil || result.Pass.Recorded != 1 {
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
		func(context.Context, capprobe.Settings, capprobe.Sweep) (capprobe.Sweep, *capprobe.PassResult, error) {
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
// and the cleanup failure is counted apart; a pass that failed before or in
// FinishPass still leaves the sweep, counted once.
func TestCapabilityProbeKeepsAFinishedPassWhoseShardCleanupFailed(t *testing.T) {
	sweep := capprobe.Sweep{Cursor: server.NewId(), StartedAt: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	finishedSweep := capprobe.Sweep{Cursor: server.NewId(), StartedAt: sweep.StartedAt}
	pass := &capprobe.PassResult{Scanned: 4, Due: 3, Eligible: 2, Started: 2, Recorded: 1}
	outcome := capabilityProbeOutcome{sweep: finishedSweep, pass: pass}
	cleanupErr := &proberShardCleanupError{err: errors.New("capability probe shard cleanup: synthetic reap failure")}
	finishErr := errors.New("synthetic evidence write failure")
	readinessErr := errors.New("synthetic readiness failure")
	failedLabels := map[string]string{"result": capprobe.PassResultFailed}
	for _, c := range []struct {
		name      string
		finished  bool
		finishErr error
		err       error
		sweep     capprobe.Sweep
		returnErr error
		failed    float64
		cleanup   float64
	}{
		{name: "finished, cleanup failed", finished: true, err: fmt.Errorf("wrapped: %w", cleanupErr), sweep: finishedSweep, failed: 0, cleanup: 1},
		{name: "finished, cleaned up", finished: true, sweep: finishedSweep},
		{name: "finish failed, cleanup failed", finished: true, finishErr: finishErr, err: errors.Join(finishErr, cleanupErr), sweep: sweep, returnErr: finishErr, failed: 0, cleanup: 1},
		{name: "failed before finish", err: readinessErr, sweep: sweep, returnErr: readinessErr, failed: 1},
		{name: "failed before finish, cleanup failed", err: errors.Join(readinessErr, cleanupErr), sweep: sweep, returnErr: readinessErr, failed: 1, cleanup: 1},
	} {
		failedBefore := capabilityProbeCounterValue(t, "urnetwork_capability_probe_passes_total", failedLabels)
		cleanupBefore := capabilityProbeCounterValue(t, "urnetwork_capability_probe_shard_cleanup_errors_total", map[string]string{})
		next, result, err := capabilityProbeShardPassResult(sweep, outcome, c.finished, c.finishErr, c.err)
		if next != c.sweep || result != pass {
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
