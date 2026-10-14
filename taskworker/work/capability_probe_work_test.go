package work

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/capprobe"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The capability probe's task, without tunnels or a database: its shard
// slot, funding, cadence, and what a pass is handed and hands back.

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
// the next sweep's start, a switched-off probe after its recheck, and after
// a result-less finish with the pass's own sweep.
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
		{name: "between sweeps", result: &CapabilityProbeResult{Sweep: betweenSweeps}, sweep: betweenSweeps, runAt: betweenSweeps.NextSweepAt},
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
