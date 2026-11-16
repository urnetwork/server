package model

// The key history retention's brake can narrow the retention's horizon,
// `horizonDays`, never below the guard's 7-day floor nor above the code's
// Retention. DB-backed tests first, then pure ones.

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The brake Main has carried since 2026-11-02 (horizonDays 30) and raised
// rates no longer keep 30 days: at the default 7-day Retention every
// horizonDays counts as 7, so a client gone 8 days loses its history and one
// gone 6 days keeps it, as under a value below the floor and one that does not
// parse. Before 2026-11-16 horizonDays 30 kept the client gone 8 days.
func TestGoneStClientKeyHistoryBrakeHorizonNarrowsOnlyWithinTheRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		const day = 24 * time.Hour
		kept := func(clientId server.Id, what string) {
			t.Helper()
			if rows := readGoneKeyHistoryTestRows(t, ctx, clientId); rows != (goneKeyHistoryTestRows{history: 1, heads: 1, gone: true}) {
				t.Fatalf("%s lost key history: %+v", what, rows)
			}
		}
		removed := func(clientId server.Id, what string) {
			t.Helper()
			if rows := readGoneKeyHistoryTestRows(t, ctx, clientId); rows != (goneKeyHistoryTestRows{}) {
				t.Fatalf("%s kept key history: %+v", what, rows)
			}
		}
		// one client gone a day short of the horizon and one a day past it
		pair := func(horizonDays int) (keptId server.Id, removedId server.Id) {
			keptId, removedId = server.NewId(), server.NewId()
			for _, clientId := range []server.Id{keptId, removedId} {
				insertGoneKeyHistoryTestRows(t, ctx, clientId, server.NewId())
			}
			setGoneKeyHistoryTestDays(t, ctx, keptId, horizonDays-1)
			setGoneKeyHistoryTestDays(t, ctx, removedId, horizonDays+1)
			return
		}
		run := func(settings *StClientKeyHistoryRetentionSettings) *StClientKeyHistoryRetentionReport {
			now := goneKeyHistoryTestDbNow(t, ctx)
			report, _ := RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), settings)
			return report
		}

		for _, brake := range []map[string]string{
			{StClientKeyHistoryRetentionControlFieldMode: "on", StClientKeyHistoryRetentionControlFieldMaxClientsPerSecond: "300", StClientKeyHistoryRetentionControlFieldHorizonDays: "30"},
			{StClientKeyHistoryRetentionControlFieldHorizonDays: "1"},
			{StClientKeyHistoryRetentionControlFieldHorizonDays: "soon"},
			nil,
		} {
			keptId, removedId := pair(7)
			setGoneKeyHistoryTestBrake(t, ctx, nil)
			if brake != nil {
				setGoneKeyHistoryTestBrake(t, ctx, brake)
			}
			if report := run(DefaultStClientKeyHistoryRetentionSettings()); report.RemovedClientCount != 1 || report.Horizon != 7*day {
				t.Fatalf("under brake %v the retention = %+v, want one client at 7 days", brake, report)
			}
			kept(keptId, "a client gone 6 days")
			removed(removedId, "a client gone 8 days")
		}

		// a longer Retention: the brake narrows within the floor and it
		settings := DefaultStClientKeyHistoryRetentionSettings()
		settings.Retention = 20 * day
		keptId, removedId := pair(10)
		setGoneKeyHistoryTestBrake(t, ctx, nil)
		setGoneKeyHistoryTestBrake(t, ctx, map[string]string{StClientKeyHistoryRetentionControlFieldHorizonDays: "10"})
		if report := run(settings); report.RemovedClientCount != 1 || report.Horizon != 10*day {
			t.Fatalf("a 10-day brake horizon under a 20-day retention = %+v, want one client at 10 days", report)
		}
		kept(keptId, "a client gone 9 days at the brake's 10")
		removed(removedId, "a client gone 11 days at the brake's 10")

		keptId, removedId = pair(20)
		setGoneKeyHistoryTestBrake(t, ctx, nil)
		setGoneKeyHistoryTestBrake(t, ctx, map[string]string{StClientKeyHistoryRetentionControlFieldHorizonDays: "30"})
		if report := run(settings); report.RemovedClientCount != 1 || report.Horizon != 20*day {
			t.Fatalf("a 30-day brake horizon under a 20-day retention = %+v, want one client at 20 days", report)
		}
		kept(keptId, "a client gone 19 days under a 20-day retention")
		removed(removedId, "a client gone 21 days under a 20-day retention")
	})
}

// Pure: horizonDays narrows to whole days between the guard's floor and the
// code's Retention, a value that does not parse is Retention, and the field
// leaves the mode and the rate alone. At the default Retention, the floor,
// every value is 7 days.
func TestNarrowStClientKeyHistoryRetentionControlHorizon(t *testing.T) {
	const day = 24 * time.Hour
	floor := server.ClientKeyHistoryRetentionFloor
	settings := DefaultStClientKeyHistoryRetentionSettings()
	for _, value := range []string{"30", " 33 ", "8", "7", "6", "1", "0", "-7", "-9223372036854775808", "91", "9223372036854775807", "3.5", "soon", ""} {
		control := NarrowStClientKeyHistoryRetentionControl(map[string]string{StClientKeyHistoryRetentionControlFieldHorizonDays: value}, settings)
		if control.Horizon == nil || *control.Horizon != 7*day {
			t.Fatalf("at the default retention horizonDays %q narrowed to %v, want 7 days", value, control.Horizon)
		}
		if !control.On || control.MaxClientsPerSecond != settings.DefaultDeleteClientsPerSecond {
			t.Fatalf("horizonDays %q changed the brake: %+v", value, control)
		}
	}

	long := DefaultStClientKeyHistoryRetentionSettings()
	long.Retention = 60 * day
	for _, test := range []struct {
		value   string
		horizon time.Duration
	}{
		{"30", 30 * day},
		{" 33 ", 33 * day},
		{"8", 8 * day},
		{"7", floor},
		{"6", floor},
		{"0", floor},
		{"-7", floor},
		{"-9223372036854775808", floor},
		{"60", long.Retention},
		{"61", long.Retention},
		{"9223372036854775807", long.Retention},
		{"3.5", long.Retention},
		{"soon", long.Retention},
		{"", long.Retention},
	} {
		control := NarrowStClientKeyHistoryRetentionControl(map[string]string{StClientKeyHistoryRetentionControlFieldHorizonDays: test.value}, long)
		if control.Horizon == nil || *control.Horizon != test.horizon {
			t.Fatalf("under a 60-day retention horizonDays %q narrowed to %v, want %v", test.value, control.Horizon, test.horizon)
		}
	}
	for _, fields := range []map[string]string{nil, {StClientKeyHistoryRetentionControlFieldMode: "on"}, {StClientKeyHistoryRetentionControlFieldMaxClientsPerSecond: "500"}} {
		if control := NarrowStClientKeyHistoryRetentionControl(fields, settings); control.Horizon != nil {
			t.Fatalf("fields %v without horizonDays set a horizon %v", fields, *control.Horizon)
		}
	}
	// a retention shorter than the floor still narrows to the floor
	short := DefaultStClientKeyHistoryRetentionSettings()
	short.Retention = day
	if control := NarrowStClientKeyHistoryRetentionControl(map[string]string{StClientKeyHistoryRetentionControlFieldHorizonDays: "soon"}, short); *control.Horizon != floor {
		t.Fatalf("a one-day retention narrowed horizonDays to %v, want the floor %v", *control.Horizon, floor)
	}
}

// Pure: a run's horizon is the brake's when it sets one, never below the
// guard's floor, and otherwise Retention.
func TestStClientKeyHistoryRunHorizon(t *testing.T) {
	const day = 24 * time.Hour
	settings := DefaultStClientKeyHistoryRetentionSettings()
	settings.Retention = 60 * day
	thirty, one := 30*day, day
	for _, test := range []struct {
		brake   *StClientKeyHistoryRetentionControl
		horizon time.Duration
	}{
		{&StClientKeyHistoryRetentionControl{On: true, Horizon: &thirty}, 30 * day},
		{&StClientKeyHistoryRetentionControl{On: false, Horizon: &thirty}, 30 * day},
		{&StClientKeyHistoryRetentionControl{On: true, Horizon: &one}, server.ClientKeyHistoryRetentionFloor},
		{&StClientKeyHistoryRetentionControl{On: true}, 60 * day},
		{nil, 60 * day},
	} {
		if horizon := settings.runHorizon(test.brake); horizon != test.horizon {
			t.Fatalf("brake %+v ran at %v, want %v", test.brake, horizon, test.horizon)
		}
	}
	if horizon := DefaultStClientKeyHistoryRetentionSettings().runHorizon(nil); horizon != 7*day {
		t.Fatalf("the default settings ran at %v, want 7 days", horizon)
	}
}
