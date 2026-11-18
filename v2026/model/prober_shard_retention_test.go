package model

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
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

	connect.AssertEqual(t, absent.MaxWalkRowsPerSecond, float64(3000))
	connect.AssertEqual(t, narrow(map[string]string{"mode": "on", "maxWalkRowsPerSecond": "8000"}).MaxWalkRowsPerSecond, float64(8000))
	connect.AssertEqual(t, narrow(map[string]string{"mode": "on", "maxWalkRowsPerSecond": "1e9"}).MaxWalkRowsPerSecond, float64(20000))
	for _, value := range []string{"0", "-5", "quick", "NaN"} {
		control := narrow(map[string]string{"mode": "on", "maxWalkRowsPerSecond": value})
		if 0 < control.MaxWalkRowsPerSecond || control.Mode != ProberShardRetentionModeOff {
			t.Fatalf("walk rate %q left the drop %s at %v", value, control.Mode, control.MaxWalkRowsPerSecond)
		}
	}

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
	// a batch with no room reads or keeps nothing: the contract step is off
	for _, batch := range []struct{ contracts, walk int }{{contracts: 0, walk: 5000}, {contracts: 500, walk: 0}, {contracts: -1, walk: -1}} {
		empty := *settings
		empty.ContractBatchSize = batch.contracts
		empty.WalkBatchSize = batch.walk
		connect.AssertEqual(t, NarrowProberShardRetentionControl(map[string]string{"contracts": "on"}, &empty).DropContracts, false)
	}
}

// Pure: a pass's generation is walked exactly when it is at or before the
// newest completed walk's generation, and nothing is before the first one.
func TestProberShardContractWalkWalked(t *testing.T) {
	day := 24 * time.Hour
	none := ProberShardContractWalk{}
	connect.AssertEqual(t, none.walkedEnd(day), time.Time{})
	connect.AssertEqual(t, none.walked(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), day), false)

	walk := ProberShardContractWalk{CompleteThrough: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
	connect.AssertEqual(t, walk.walkedEnd(day), time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	for _, c := range []struct {
		createTime time.Time
		walked     bool
	}{
		{createTime: time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC), walked: true},
		{createTime: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC), walked: true},
		{createTime: time.Date(2026, 11, 8, 23, 59, 59, 999000000, time.UTC), walked: true},
		{createTime: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), walked: false},
		{createTime: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC), walked: false},
	} {
		if walk.walked(c.createTime, day) != c.walked {
			t.Errorf("a pass created %s walked %t, want %t", c.createTime, !c.walked, c.walked)
		}
	}
}

// Pure: the walk round trips through the task args with its window and
// cursor, and the empty args of a build from before read as no walk.
func TestProberShardContractWalkRoundTripsThroughTaskArgs(t *testing.T) {
	walk := ProberShardContractWalk{
		CompleteThrough: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC),
		Generation:      time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC),
		Lower:           server.ContractIdFloor(time.Date(2026, 11, 7, 23, 45, 0, 0, time.UTC)),
		Upper:           server.ContractIdFloor(time.Date(2026, 11, 9, 0, 30, 0, 0, time.UTC)),
		Cursor:          server.NewId(),
	}
	encoded, err := json.Marshal(walk)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ProberShardContractWalk
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.CompleteThrough.Equal(walk.CompleteThrough) || !decoded.Generation.Equal(walk.Generation) ||
		decoded.Lower != walk.Lower || decoded.Upper != walk.Upper || decoded.Cursor != walk.Cursor {
		t.Fatalf("the walk did not round trip: %s read as %+v", encoded, decoded)
	}
	var before ProberShardContractWalk
	if err := json.Unmarshal([]byte(`{}`), &before); err != nil {
		t.Fatal(err)
	}
	if !before.CompleteThrough.IsZero() || !before.Generation.IsZero() || before.Cursor != (server.Id{}) {
		t.Fatalf("empty args read as a walk: %+v", before)
	}
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
