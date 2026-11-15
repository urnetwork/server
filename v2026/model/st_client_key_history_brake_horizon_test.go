package model

// The key history retention's brake can set the retention's own horizon,
// `horizonDays`, apart from the contract horizon reaper's, down to the guard's
// 30-day floor. DB-backed, on main's migrations only (the floor function is
// migration 904's).

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// With the reaper on at 35 days, as on main since 2026-11-02, a brake horizon
// of 30 days deletes a client gone 32 days, a client gone 29 days keeps its
// history under the guard's floor, the brake's horizon applies whatever the
// reaper's mode, a value that does not parse keeps 91 days, and removing the
// field returns the retention to the reaper's horizon.
func TestGoneStClientKeyHistoryBrakeSetsItsOwnHorizon(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		const day = 24 * time.Hour
		floorDays := int(server.ClientKeyHistoryRetentionFloor / day)
		belowFloorId, brakeId, reaperId, unparsedId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		for _, clientId := range []server.Id{belowFloorId, brakeId, reaperId, unparsedId} {
			insertGoneKeyHistoryTestRows(t, ctx, clientId, server.NewId())
		}
		setGoneKeyHistoryTestDays(t, ctx, belowFloorId, floorDays-1)
		setGoneKeyHistoryTestDays(t, ctx, brakeId, floorDays+2)
		setGoneKeyHistoryTestDays(t, ctx, reaperId, 36)
		run := func() *StClientKeyHistoryRetentionReport {
			now := goneKeyHistoryTestDbNow(t, ctx)
			report, _ := RemoveGoneStClientKeyHistory(ctx, now, goneKeyHistoryTestDeleteOnly(now), DefaultStClientKeyHistoryRetentionSettings())
			return report
		}
		reaper := func(mode string) {
			setGoneKeyHistoryTestContractHorizon(t, ctx, nil)
			setGoneKeyHistoryTestContractHorizon(t, ctx, map[string]string{ContractHorizonControlFieldMode: mode, ContractHorizonControlFieldHorizonDays: "35"})
		}
		brake := func(horizonDays string) {
			setGoneKeyHistoryTestBrake(t, ctx, nil)
			setGoneKeyHistoryTestBrake(t, ctx, map[string]string{StClientKeyHistoryRetentionControlFieldHorizonDays: horizonDays})
		}
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

		reaper("on")
		setGoneKeyHistoryTestBrake(t, ctx, nil)
		if report := run(); report.RemovedClientCount != 1 || report.Horizon != 35*day {
			t.Fatalf("with the reaper on at 35 days and no brake horizon the retention = %+v, want one client at 35 days", report)
		}
		removed(reaperId, "a client gone 36 days at the reaper's 35")
		kept(brakeId, "a client gone 32 days at the reaper's 35")

		brake("30")
		if report := run(); report.RemovedClientCount != 1 || report.Horizon != 30*day {
			t.Fatalf("a 30-day brake horizon = %+v, want one client at 30 days", report)
		}
		removed(brakeId, "a client gone 32 days at the brake's 30")
		kept(belowFloorId, "a client gone 29 days at the brake's 30")

		brake("1")
		if report := run(); report.RemovedClientCount != 0 || report.Horizon != server.ClientKeyHistoryRetentionFloor {
			t.Fatalf("a one-day brake horizon = %+v, want none removed at the guard's floor %v", report, server.ClientKeyHistoryRetentionFloor)
		}
		kept(belowFloorId, "a client gone a day less than the guard's floor")

		for _, mode := range []string{"dry_run", "off"} {
			goneId := server.NewId()
			insertGoneKeyHistoryTestRows(t, ctx, goneId, server.NewId())
			setGoneKeyHistoryTestDays(t, ctx, goneId, floorDays+1)
			reaper(mode)
			brake("30")
			if report := run(); report.RemovedClientCount != 1 || report.Horizon != 30*day {
				t.Fatalf("with the reaper in %s a 30-day brake horizon = %+v, want one client at 30 days", mode, report)
			}
			removed(goneId, "a client gone 31 days at the brake's 30 with the reaper in "+mode)
		}
		setGoneKeyHistoryTestContractHorizon(t, ctx, nil)
		goneId := server.NewId()
		insertGoneKeyHistoryTestRows(t, ctx, goneId, server.NewId())
		setGoneKeyHistoryTestDays(t, ctx, goneId, floorDays+1)
		brake("30")
		if report := run(); report.RemovedClientCount != 1 || report.Horizon != 30*day {
			t.Fatalf("without a reaper control a 30-day brake horizon = %+v, want one client at 30 days", report)
		}
		removed(goneId, "a client gone 31 days at the brake's 30 without a reaper control")

		reaper("on")
		setGoneKeyHistoryTestDays(t, ctx, unparsedId, 50)
		brake("soon")
		if report := run(); report.RemovedClientCount != 0 || report.Horizon != server.ContractRetention {
			t.Fatalf("a brake horizon that does not parse = %+v, want none removed at 91 days", report)
		}
		kept(unparsedId, "a client gone 50 days under a brake horizon that does not parse")

		setGoneKeyHistoryTestBrake(t, ctx, nil)
		if report := run(); report.RemovedClientCount != 1 || report.Horizon != 35*day {
			t.Fatalf("after the brake horizon is removed the retention = %+v, want one client at the reaper's 35 days", report)
		}
		removed(unparsedId, "a client gone 50 days at the reaper's 35")
		kept(belowFloorId, "a client gone 29 days at the reaper's 35")
	})
}

// Pure: the brake's horizonDays narrows to whole days between the guard's
// floor and the retention's longest horizon, a value that does not parse is
// the longest, and the field leaves the mode and the rate alone.
func TestNarrowStClientKeyHistoryRetentionControlHorizon(t *testing.T) {
	const day = 24 * time.Hour
	settings := DefaultStClientKeyHistoryRetentionSettings()
	floor := server.ClientKeyHistoryRetentionFloor
	for _, test := range []struct {
		value   string
		horizon time.Duration
	}{
		{"30", 30 * day},
		{" 33 ", 33 * day},
		{"45", 45 * day},
		{"29", floor},
		{"1", floor},
		{"0", floor},
		{"-7", floor},
		{"-9223372036854775808", floor},
		{"91", settings.Retention},
		{"92", settings.Retention},
		{"9223372036854775807", settings.Retention},
		{"3.5", settings.Retention},
		{"soon", settings.Retention},
		{"", settings.Retention},
	} {
		control := NarrowStClientKeyHistoryRetentionControl(map[string]string{StClientKeyHistoryRetentionControlFieldHorizonDays: test.value}, settings)
		if control.Horizon == nil || *control.Horizon != test.horizon {
			t.Fatalf("horizonDays %q narrowed to %v, want %v", test.value, control.Horizon, test.horizon)
		}
		if !control.On || control.MaxClientsPerSecond != settings.DefaultDeleteClientsPerSecond {
			t.Fatalf("horizonDays %q changed the brake: %+v", test.value, control)
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
// guard's floor, and otherwise the retention's own, here not followed.
func TestStClientKeyHistoryRunHorizon(t *testing.T) {
	const day = 24 * time.Hour
	ctx := context.Background()
	settings := DefaultStClientKeyHistoryRetentionSettings()
	settings.FollowContractHorizon = false
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
		if horizon := settings.runHorizon(ctx, test.brake); horizon != test.horizon {
			t.Fatalf("brake %+v ran at %v, want %v", test.brake, horizon, test.horizon)
		}
	}
}
