package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Pure: a pass is due for the window step once its deadline is
// WindowMinDeadlineAge past. A drained, settled due pass loses its windows; a
// due pass that is not settled, or still live, is waited at until
// WindowSkipAge, then passed over; a pass whose identity went is gone. Before
// the window step a settled pass's windows had no action short of its
// generation's drop, eight days on.
func TestProberShardWindowAction(t *testing.T) {
	settings := DefaultProberShardRetentionSettings()
	connect.AssertEqual(t, settings.WindowMinDeadlineAge, time.Hour)
	connect.AssertEqual(t, settings.WindowSkipAge, 6*time.Hour)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time {
		return now.Add(-d)
	}
	for _, c := range []struct {
		state    string
		deadline time.Time
		settled  bool
		action   string
	}{
		// a pass still inside its deadline, or within the hour after it
		{state: "active", deadline: now.Add(10 * time.Minute), action: proberShardWindowActionNotDue},
		{state: "draining", deadline: ago(30 * time.Minute), settled: true, action: proberShardWindowActionNotDue},
		{state: "draining", deadline: ago(time.Hour), settled: true, action: proberShardWindowActionNotDue},
		// due and settled: its windows go, however old
		{state: "draining", deadline: ago(time.Hour + time.Second), settled: true, action: proberShardWindowActionRemove},
		{state: "draining", deadline: ago(8 * 24 * time.Hour), settled: true, action: proberShardWindowActionRemove},
		// due, not settled: waited at, then passed over
		{state: "draining", deadline: ago(2 * time.Hour), action: proberShardWindowActionWait},
		{state: "draining", deadline: ago(6 * time.Hour), action: proberShardWindowActionWait},
		{state: "draining", deadline: ago(6*time.Hour + time.Second), action: proberShardWindowActionSkip},
		// still live past its deadline: never removed, settled or not
		{state: "active", deadline: ago(2 * time.Hour), settled: true, action: proberShardWindowActionWait},
		{state: "active", deadline: ago(7 * time.Hour), settled: true, action: proberShardWindowActionSkip},
		// its identity went with its generation
		{state: "deleted", deadline: ago(9 * 24 * time.Hour), settled: true, action: proberShardWindowActionGone},
		{state: "closed", deadline: ago(2 * time.Hour), action: proberShardWindowActionGone},
	} {
		if action := proberShardWindowAction(c.state, c.deadline, c.settled, now, settings); action != c.action {
			t.Errorf("%s pass with deadline %s ago, settled=%t: %s, want %s", c.state, now.Sub(c.deadline), c.settled, action, c.action)
		}
	}
}

// Pure: the control turns the window step off, paces it within the code's
// bound, and a rate that does not parse or is not positive turns off the
// window step alone, not the drop.
func TestNarrowProberShardRetentionControlWindows(t *testing.T) {
	settings := DefaultProberShardRetentionSettings()
	narrow := func(fields map[string]string) *ProberShardRetentionControl {
		return NarrowProberShardRetentionControl(fields, settings)
	}
	absent := narrow(map[string]string{})
	connect.AssertEqual(t, absent.DropWindows, true)
	connect.AssertEqual(t, absent.MaxWindowShardsPerMinute, float64(4))

	connect.AssertEqual(t, narrow(map[string]string{"mode": "on", "windows": "off"}).DropWindows, false)
	connect.AssertEqual(t, narrow(map[string]string{"mode": "on", "windows": " on "}).DropWindows, true)
	connect.AssertEqual(t, narrow(map[string]string{"mode": "on", "windows": "yes"}).DropWindows, false)

	connect.AssertEqual(t, narrow(map[string]string{"mode": "on", "maxWindowShardsPerMinute": "12"}).MaxWindowShardsPerMinute, float64(12))
	connect.AssertEqual(t, narrow(map[string]string{"mode": "on", "maxWindowShardsPerMinute": "1e6"}).MaxWindowShardsPerMinute, float64(120))
	for _, value := range []string{"0", "-1", "fast", "NaN"} {
		control := narrow(map[string]string{"mode": "on", "maxWindowShardsPerMinute": value})
		if control.DropWindows || 0 < control.MaxWindowShardsPerMinute {
			t.Fatalf("window rate %q left the window step on at %v", value, control.MaxWindowShardsPerMinute)
		}
		if control.Mode != ProberShardRetentionModeOn {
			t.Fatalf("window rate %q turned the drop %s", value, control.Mode)
		}
	}

	noWindows := *settings
	noWindows.DropWindows = false
	connect.AssertEqual(t, NarrowProberShardRetentionControl(map[string]string{"windows": "on"}, &noWindows).DropWindows, false)
	noBatch := *settings
	noBatch.WindowBatchSize = 0
	connect.AssertEqual(t, NarrowProberShardRetentionControl(map[string]string{"windows": "on"}, &noBatch).DropWindows, false)
	// the drop's own rates still turn the mode, and so the window step, off
	connect.AssertEqual(t, narrow(map[string]string{"mode": "on", "maxShardsPerMinute": "0"}).Mode, ProberShardRetentionModeOff)
}

// Pure: the cursor round trips through the task args, and the empty args of a
// chain's first run read as the oldest pass.
func TestProberShardWindowCursorRoundTripsThroughTaskArgs(t *testing.T) {
	cursor := ProberShardWindowCursor{
		CreateTime: time.Date(2026, 10, 22, 11, 25, 9, 333663000, time.UTC),
		TaskId:     server.NewId(),
		Epoch:      server.NewId(),
	}
	encoded, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ProberShardWindowCursor
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.CreateTime.Equal(cursor.CreateTime) || decoded.TaskId != cursor.TaskId || decoded.Epoch != cursor.Epoch {
		t.Fatalf("the cursor did not round trip: %s read as %+v", encoded, decoded)
	}
	var first ProberShardWindowCursor
	if err := json.Unmarshal([]byte(`{}`), &first); err != nil {
		t.Fatal(err)
	}
	if !first.CreateTime.IsZero() || first.TaskId != (server.Id{}) || first.Epoch != (server.Id{}) {
		t.Fatalf("empty args read as a cursor: %+v", first)
	}
}

// Pure: a window run that caught up, waits at a pass, found the step off or
// its control unreadable, or only dry ran comes back after the idle interval;
// one that spent its budget, met a held pass or was canceled comes back soon.
func TestProberShardWindowReportIdle(t *testing.T) {
	for stop, idle := range map[string]bool{
		proberShardRetentionStopCaughtUp: true,
		proberShardRetentionStopOff:      true,
		proberShardRetentionStopControl:  true,
		proberShardRetentionStopDryRun:   true,
		proberShardRetentionStopBudget:   false,
		proberShardRetentionStopBusy:     false,
		proberShardRetentionStopCanceled: false,
		proberShardRetentionStopError:    false,
	} {
		report := &ProberShardWindowReport{Stop: stop}
		if report.Idle() != idle {
			t.Errorf("stop %s: idle %t, want %t", stop, !idle, idle)
		}
	}
}
