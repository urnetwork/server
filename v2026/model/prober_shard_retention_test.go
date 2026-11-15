package model

import (
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"
)

// The cutoff and a pass's drop time agree: a pass is in a dropped generation
// exactly when its drop time has come.
func TestProberShardRetentionCutoffAndDropTime(t *testing.T) {
	day := 24 * time.Hour
	now := time.Date(2026, 11, 9, 13, 30, 0, 0, time.UTC)
	// eight generations kept on 11-09: 11-02 through 11-09
	connect.AssertEqual(t, ProberShardRetentionCutoff(now, day, 8), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	// two kept: yesterday and today
	connect.AssertEqual(t, ProberShardRetentionCutoff(now, day, 2), time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC))
	// a pass created in the last minute of 11-01 drops when 11-09 begins
	connect.AssertEqual(t, ProberShardDropTime(time.Date(2026, 11, 1, 23, 59, 0, 0, time.UTC), day, 8), time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	// generations of six hours align to UTC too
	connect.AssertEqual(t, ProberShardRetentionCutoff(now, 6*time.Hour, 4), time.Date(2026, 11, 8, 18, 0, 0, 0, time.UTC))

	start := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	for _, generation := range []time.Duration{6 * time.Hour, day} {
		for _, retained := range []int{2, 8, 30} {
			for createStep := time.Duration(0); createStep < 40*day; createStep += 7 * time.Hour {
				createTime := start.Add(createStep)
				for nowStep := time.Duration(0); nowStep < 80*day; nowStep += 5 * time.Hour {
					runNow := start.Add(nowStep)
					due := createTime.Before(ProberShardRetentionCutoff(runNow, generation, retained))
					dropped := !runNow.Before(ProberShardDropTime(createTime, generation, retained))
					if due != dropped {
						t.Fatalf("generation=%s retained=%d create=%s now=%s: due=%t but drop time reached=%t",
							generation, retained, createTime, runNow, due, dropped)
					}
				}
			}
		}
	}
}

func TestParseProberShardRetentionMode(t *testing.T) {
	for _, mode := range []ProberShardRetentionMode{ProberShardRetentionModeOff, ProberShardRetentionModeDryRun, ProberShardRetentionModeOn} {
		parsed, ok := ParseProberShardRetentionMode(" " + mode.String() + " ")
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, parsed, mode)
	}
	parsed, ok := ParseProberShardRetentionMode("onn")
	connect.AssertEqual(t, ok, false)
	connect.AssertEqual(t, parsed, ProberShardRetentionModeOff)
	connect.AssertEqual(t, ProberShardRetentionMode(7).String(), "off")
}

// Each control field narrows the code settings, and a field that does not
// parse narrows as far as it can.
func TestNarrowProberShardRetentionControl(t *testing.T) {
	settings := DefaultProberShardRetentionSettings()
	narrow := func(fields map[string]string) *ProberShardRetentionControl {
		return NarrowProberShardRetentionControl(fields, settings)
	}

	absent := narrow(map[string]string{})
	connect.AssertEqual(t, absent.Present, false)
	connect.AssertEqual(t, absent.Mode, ProberShardRetentionModeOff)
	connect.AssertEqual(t, absent.RetainedGenerations, 8)
	connect.AssertEqual(t, absent.MaxContractsPerSecond, float64(200))
	connect.AssertEqual(t, absent.DropContracts, true)

	on := narrow(map[string]string{"mode": "on"})
	connect.AssertEqual(t, on.Present, true)
	connect.AssertEqual(t, on.Mode, ProberShardRetentionModeOn)
	connect.AssertEqual(t, narrow(map[string]string{"mode": "dry_run"}).Mode, ProberShardRetentionModeDryRun)
	connect.AssertEqual(t, narrow(map[string]string{"mode": "yes"}).Mode, ProberShardRetentionModeOff)

	for value, want := range map[string]int{"4": 4, " 12 ": 12, "1": 2, "0": 2, "-3": 2, "99": 30, "eight": 30, "": 30} {
		connect.AssertEqual(t, narrow(map[string]string{"mode": "on", "retainedGenerations": value}).RetainedGenerations, want)
	}

	connect.AssertEqual(t, narrow(map[string]string{"mode": "on", "maxContractsPerSecond": "500"}).MaxContractsPerSecond, float64(500))
	connect.AssertEqual(t, narrow(map[string]string{"mode": "on", "maxContractsPerSecond": "99999"}).MaxContractsPerSecond, float64(2000))
	for _, value := range []string{"0", "-1", "fast", "NaN"} {
		control := narrow(map[string]string{"mode": "on", "maxContractsPerSecond": value})
		if 0 < control.MaxContractsPerSecond || control.Mode != ProberShardRetentionModeOff {
			t.Fatalf("rate %q left the drop %s at %v", value, control.Mode, control.MaxContractsPerSecond)
		}
	}
	connect.AssertEqual(t, math.IsNaN(narrow(map[string]string{"maxContractsPerSecond": "NaN"}).MaxContractsPerSecond), false)

	connect.AssertEqual(t, absent.MaxShardsPerMinute, float64(30))
	connect.AssertEqual(t, narrow(map[string]string{"mode": "on", "maxShardsPerMinute": "120"}).MaxShardsPerMinute, float64(120))
	connect.AssertEqual(t, narrow(map[string]string{"mode": "on", "maxShardsPerMinute": "100000"}).MaxShardsPerMinute, float64(600))
	for _, value := range []string{"0", "-2", "slow"} {
		control := narrow(map[string]string{"mode": "on", "maxShardsPerMinute": value})
		if 0 < control.MaxShardsPerMinute || control.Mode != ProberShardRetentionModeOff {
			t.Fatalf("shard rate %q left the drop %s at %v", value, control.Mode, control.MaxShardsPerMinute)
		}
	}

	connect.AssertEqual(t, narrow(map[string]string{"contracts": "off"}).DropContracts, false)
	connect.AssertEqual(t, narrow(map[string]string{"contracts": "on"}).DropContracts, true)
	connect.AssertEqual(t, narrow(map[string]string{"contracts": "maybe"}).DropContracts, false)

	disabled := *settings
	disabled.Enable = false
	connect.AssertEqual(t, NarrowProberShardRetentionControl(map[string]string{"mode": "on"}, &disabled).Mode, ProberShardRetentionModeOff)
	noContracts := *settings
	noContracts.DropContracts = false
	connect.AssertEqual(t, NarrowProberShardRetentionControl(map[string]string{"contracts": "on"}, &noContracts).DropContracts, false)
}

// A run publishes its clock with the backlog it read, so a reader that takes
// the taskworker with the newest clock (the taskworker dashboard's prober
// retention stats) reads that run's backlog. With every pass closed, the
// oldest is the run's clock.
func TestProberShardRetentionPublishesItsClockWithTheBacklog(t *testing.T) {
	settings := DefaultProberShardRetentionSettings()
	now := time.Date(2026, 11, 9, 13, 30, 0, 0, time.UTC)
	run := &proberShardRetentionRun{settings: settings, now: now, report: newProberShardRetentionReport()}
	oldest := time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC)
	run.publishBacklog(7, &oldest)
	connect.AssertEqual(t, run.report.DueShards, int64(7))
	connect.AssertEqual(t, *run.report.OldestShard, oldest)
	connect.AssertEqual(t, testutil.ToFloat64(proberShardRetentionDueGauge), float64(7))
	connect.AssertEqual(t, testutil.ToFloat64(proberShardRetentionOldestGauge), float64(oldest.Unix()))
	connect.AssertEqual(t, testutil.ToFloat64(proberShardRetentionRunTimestampGauge), float64(now.Unix()))

	later := &proberShardRetentionRun{settings: settings, now: now.Add(5 * time.Minute), report: newProberShardRetentionReport()}
	later.publishBacklog(0, nil)
	connect.AssertEqual(t, later.report.DueShards, int64(0))
	connect.AssertEqual(t, later.report.OldestShard == nil, true)
	connect.AssertEqual(t, testutil.ToFloat64(proberShardRetentionDueGauge), float64(0))
	connect.AssertEqual(t, testutil.ToFloat64(proberShardRetentionOldestGauge), float64(later.now.Unix()))
	connect.AssertEqual(t, testutil.ToFloat64(proberShardRetentionRunTimestampGauge), float64(later.now.Unix()))
}
