package model

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Pure: a pass reads a contract only once a contract that lives as long as
// the longest normal contract has closed before the horizon, with margin for
// an expiry close that runs late.
func TestContractHorizonCloseSettleTimeCoversContractLife(t *testing.T) {
	settings := DefaultContractHorizonSettings()
	if settings.CloseSettleTime != 2*DefaultContractExpiration || settings.CloseSettleTime < CompanionOriginCloseLinger+30*time.Minute {
		t.Fatalf("the close settle time %s does not cover the contract life %s with margin", settings.CloseSettleTime, DefaultContractExpiration)
	}
}

// Pure: a pass that reached the horizon starts over weekly (owner,
// 2026-11-16), no sooner than a recent payment stops keeping a contract, so
// the next pass deletes what this one kept as payment_recent. The thirty days
// before left those contracts three weeks past their payments' discharge.
func TestContractHorizonRescansWeekly(t *testing.T) {
	settings := DefaultContractHorizonSettings()
	if settings.RescanInterval != 7*24*time.Hour || settings.RescanInterval < CompletedContractExpiration {
		t.Fatalf("the rescan interval %s is not a week, or is shorter than a recent payment keeps a contract (%s)", settings.RescanInterval, CompletedContractExpiration)
	}
}

// Pure: the cursor round trips through the task args with its skipped counts,
// and a cursor a build from before 2026-11-15 stored, with only its id, pass
// start and batch size, reads with no counts and keeps its pass start, which
// decides whether it starts over.
func TestContractHorizonCursorRoundTrips(t *testing.T) {
	contractId := server.ContractIdFloor(time.Date(2026, time.October, 11, 20, 40, 23, 0, time.UTC))
	stored, err := json.Marshal(map[string]any{
		"contract_id":     contractId,
		"pass_start_time": "2026-11-01T15:30:51Z",
		"batch_size":      2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	var earlier ContractHorizonCursor
	if err := json.Unmarshal(stored, &earlier); err != nil {
		t.Fatal(err)
	}
	if earlier.ContractId != contractId || earlier.BatchSize != 2000 || earlier.SkippedReasonCounts != nil ||
		!earlier.PassStartTime.Equal(time.Date(2026, time.November, 1, 15, 30, 51, 0, time.UTC)) ||
		!earlier.PassStartTime.Before(contractHorizonSettledPassTime) {
		t.Fatalf("a cursor from before does not read as a pass that began before reads were settled: %+v", earlier)
	}

	cursor := newContractHorizonPass(time.Date(2026, time.November, 15, 22, 0, 0, 0, time.UTC), 1500)
	cursor.ContractId = contractId
	cursor.addSkipped(map[string]int64{contractHorizonProtectedRecentClose: 2, contractHorizonProtectedOpen: 0})
	cursor.addSkipped(map[string]int64{contractHorizonProtectedRecentClose: 1, contractHorizonProtectedPaymentHold: 4})
	args, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	var read ContractHorizonCursor
	if err := json.Unmarshal(args, &read); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{contractHorizonProtectedRecentClose: 3, contractHorizonProtectedPaymentHold: 4}
	if read.ContractId != contractId || read.BatchSize != 1500 || !read.PassStartTime.Equal(cursor.PassStartTime) ||
		!maps.Equal(read.SkippedReasonCounts, want) || read.PassStartTime.Before(contractHorizonSettledPassTime) {
		t.Fatalf("the cursor did not round trip: %s read as %+v", args, read)
	}
}

// Pure: the control turns the reaper on, off or to a dry run and slows it,
// but never past the code settings, and a field that does not parse narrows
// as far as it can.
func TestNarrowContractHorizonControl(t *testing.T) {
	settings := DefaultContractHorizonSettings()
	for _, sample := range []struct {
		name      string
		fields    map[string]string
		mutate    func(*ContractHorizonSettings)
		mode      ContractHorizonMode
		rate      float64
		batchSize int
	}{
		{name: "absent", fields: map[string]string{}, mode: ContractHorizonModeOff, rate: 200, batchSize: 2000},
		{name: "code default on", fields: map[string]string{}, mutate: func(s *ContractHorizonSettings) { s.DefaultMode = ContractHorizonModeOn },
			mode: ContractHorizonModeOn, rate: 200, batchSize: 2000},
		{name: "on", fields: map[string]string{"mode": "on"}, mode: ContractHorizonModeOn, rate: 200, batchSize: 2000},
		{name: "dry run", fields: map[string]string{"mode": " dry_run "}, mode: ContractHorizonModeDryRun, rate: 200, batchSize: 2000},
		{name: "off over code default on", fields: map[string]string{"mode": "off"}, mutate: func(s *ContractHorizonSettings) { s.DefaultMode = ContractHorizonModeOn },
			mode: ContractHorizonModeOff, rate: 200, batchSize: 2000},
		{name: "unparsable mode is off", fields: map[string]string{"mode": "yes"}, mutate: func(s *ContractHorizonSettings) { s.DefaultMode = ContractHorizonModeOn },
			mode: ContractHorizonModeOff, rate: 200, batchSize: 2000},
		{name: "rate raised to the ceiling", fields: map[string]string{"mode": "on", "maxRowsPerSecond": "50000"}, mode: ContractHorizonModeOn, rate: 5000, batchSize: 2000},
		{name: "rate raised inside the ceiling", fields: map[string]string{"mode": "on", "maxRowsPerSecond": "3000"}, mode: ContractHorizonModeOn, rate: 3000, batchSize: 2000},
		{name: "rate lowered", fields: map[string]string{"mode": "on", "maxRowsPerSecond": "25.5"}, mode: ContractHorizonModeOn, rate: 25.5, batchSize: 2000},
		{name: "zero rate is off", fields: map[string]string{"mode": "on", "maxRowsPerSecond": "0"}, mode: ContractHorizonModeOff, rate: 0, batchSize: 2000},
		{name: "negative rate is off", fields: map[string]string{"mode": "on", "maxRowsPerSecond": "-5"}, mode: ContractHorizonModeOff, rate: -5, batchSize: 2000},
		{name: "unparsable rate is off", fields: map[string]string{"mode": "on", "maxRowsPerSecond": "fast"}, mode: ContractHorizonModeOff, rate: 0, batchSize: 2000},
		{name: "NaN rate is off", fields: map[string]string{"mode": "on", "maxRowsPerSecond": "NaN"}, mode: ContractHorizonModeOff, rate: 0, batchSize: 2000},
		{name: "batch lowered", fields: map[string]string{"mode": "on", "batchSize": "500"}, mode: ContractHorizonModeOn, rate: 200, batchSize: 500},
		{name: "batch above the code size", fields: map[string]string{"mode": "on", "batchSize": "100000"}, mode: ContractHorizonModeOn, rate: 200, batchSize: 2000},
		{name: "batch below the minimum", fields: map[string]string{"mode": "on", "batchSize": "1"}, mode: ContractHorizonModeOn, rate: 200, batchSize: 125},
		{name: "unparsable batch is the minimum", fields: map[string]string{"mode": "on", "batchSize": "big"}, mode: ContractHorizonModeOn, rate: 200, batchSize: 125},
		{name: "code switch off", fields: map[string]string{"mode": "on", "maxRowsPerSecond": "100"}, mutate: func(s *ContractHorizonSettings) { s.Enable = false },
			mode: ContractHorizonModeOff, rate: 100, batchSize: 2000},
	} {
		sampleSettings := *settings
		if sample.mutate != nil {
			sample.mutate(&sampleSettings)
		}
		control := NarrowContractHorizonControl(sample.fields, &sampleSettings)
		if control.Present != (0 < len(sample.fields)) || control.Mode != sample.mode || control.MaxRowsPerSecond != sample.rate || control.BatchSize != sample.batchSize {
			t.Errorf("%s: control %+v, want mode %s rate %v batch %d", sample.name, control, sample.mode, sample.rate, sample.batchSize)
		}
	}
}

// Pure: every mode round trips through its control value, and anything else
// parses as off.
func TestParseContractHorizonMode(t *testing.T) {
	for _, mode := range []ContractHorizonMode{ContractHorizonModeOff, ContractHorizonModeDryRun, ContractHorizonModeOn} {
		parsed, ok := ParseContractHorizonMode(mode.String())
		if !ok || parsed != mode {
			t.Errorf("%s parsed as %s, %t", mode, parsed, ok)
		}
	}
	if parsed, ok := ParseContractHorizonMode("ON"); ok || parsed != ContractHorizonModeOff {
		t.Errorf("an unknown value parsed as %s, %t", parsed, ok)
	}
	if ContractHorizonMode(7).String() != "off" {
		t.Error("an out of range mode does not print off")
	}
}

// Pure: the code's horizon is the owner's 91 days. A shorter setting applies
// down to the reaper's 35-day floor and no closer, and the floor is the
// reaper's own: server.ContractRetention, which the partition drop and the
// other retentions share, stays 91 days.
func TestContractHorizonNeverBelowFloor(t *testing.T) {
	settings := DefaultContractHorizonSettings()
	if settings.horizon() != 91*24*time.Hour || server.ContractRetention != 91*24*time.Hour {
		t.Fatal("default horizon is not 91 days", settings.horizon(), server.ContractRetention)
	}
	settings.Horizon = 60 * 24 * time.Hour
	if settings.horizon() != 60*24*time.Hour {
		t.Fatal("a horizon setting above the floor was not applied", settings.horizon())
	}
	settings.Horizon = 40 * 24 * time.Hour
	if settings.horizon() != 40*24*time.Hour {
		t.Fatal("a 40-day horizon setting was not applied", settings.horizon())
	}
	settings.Horizon = 30 * 24 * time.Hour
	if settings.horizon() != 35*24*time.Hour {
		t.Fatal("a horizon setting below the floor was not raised to it", settings.horizon())
	}
	settings.Horizon = 0
	if settings.horizon() != 35*24*time.Hour {
		t.Fatal("a zero horizon setting was not raised to the floor", settings.horizon())
	}
	settings.Horizon = 120 * 24 * time.Hour
	if settings.horizon() != 120*24*time.Hour {
		t.Fatal("a longer horizon setting was not applied", settings.horizon())
	}
}

// Pure: a batch that runs past the target halves the next, a fast one grows
// it by half, and the size stays between the minimum and the control's size.
func TestNextContractHorizonBatchSize(t *testing.T) {
	settings := DefaultContractHorizonSettings()
	settings.MinBatchSize = 100
	settings.TargetBatchDuration = 8 * time.Second
	for _, sample := range []struct {
		size     int
		duration time.Duration
		ceiling  int
		want     int
	}{
		{size: 2000, duration: 9 * time.Second, ceiling: 2000, want: 1000},
		{size: 150, duration: time.Minute, ceiling: 2000, want: 100},
		{size: 100, duration: time.Minute, ceiling: 2000, want: 100},
		{size: 1000, duration: time.Second, ceiling: 2000, want: 1500},
		{size: 1500, duration: time.Second, ceiling: 2000, want: 2000},
		{size: 1000, duration: 4 * time.Second, ceiling: 2000, want: 1000},
		{size: 1000, duration: 8 * time.Second, ceiling: 2000, want: 1000},
		{size: 1000, duration: 2 * time.Second, ceiling: 2000, want: 1000},
		// the control lowered its size since the last batch
		{size: 2000, duration: 4 * time.Second, ceiling: 500, want: 500},
	} {
		if got := nextContractHorizonBatchSize(sample.size, sample.duration, sample.ceiling, settings); got != sample.want {
			t.Errorf("size %d after %s under %d: %d, want %d", sample.size, sample.duration, sample.ceiling, got, sample.want)
		}
	}
}

// Pure: the pause after a batch keeps the pass at the rate, counting the time
// the batch itself took, and never waits for a missing rate.
func TestContractHorizonPaceDelay(t *testing.T) {
	for _, sample := range []struct {
		scanned  int64
		rate     float64
		duration time.Duration
		want     time.Duration
	}{
		{scanned: 2000, rate: 200, duration: 2 * time.Second, want: 8 * time.Second},
		{scanned: 2000, rate: 200, duration: 12 * time.Second, want: 0},
		{scanned: 500, rate: 1000, duration: 0, want: 500 * time.Millisecond},
		{scanned: 2000, rate: 0, duration: 0, want: 0},
		{scanned: 0, rate: 200, duration: time.Second, want: 0},
	} {
		if got := contractHorizonPaceDelay(sample.scanned, sample.rate, sample.duration); got != sample.want {
			t.Errorf("%d rows at %v/s after %s: %s, want %s", sample.scanned, sample.rate, sample.duration, got, sample.want)
		}
	}
}

// Pure: removing ordered ids keeps the others in order.
func TestWithoutContractIds(t *testing.T) {
	ids := make([]server.Id, 6)
	for i := range ids {
		ids[i] = server.NewId()
	}
	slices.SortFunc(ids, func(a, b server.Id) int { return a.Cmp(b) })
	kept := withoutContractIds(ids, []server.Id{ids[0], ids[3], ids[5]})
	if !slices.Equal(kept, []server.Id{ids[1], ids[2], ids[4]}) {
		t.Fatal("wrong ids kept", kept)
	}
	if !slices.Equal(withoutContractIds(ids, nil), ids) {
		t.Fatal("removing nothing changed the ids")
	}
}
