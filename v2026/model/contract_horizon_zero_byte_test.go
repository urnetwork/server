package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Pure: zero-byte contracts are kept 8 days (owner, 2026-11-21), and no
// setting or control reaches inside that.
func TestContractHorizonZeroByteHorizonIsEightDays(t *testing.T) {
	const day = 24 * time.Hour
	settings := DefaultContractHorizonSettings()
	if ContractHorizonZeroByteFloor != 8*day || settings.ZeroByteHorizon != ContractHorizonZeroByteFloor {
		t.Fatalf("the zero-byte horizon is %s with floor %s, want 8 days", settings.ZeroByteHorizon, ContractHorizonZeroByteFloor)
	}
	// it covers the prober shards' generations and a contract's settle time
	if ContractHorizonZeroByteFloor < time.Duration(DefaultProberShardRetentionSettings().RetainedGenerations)*24*time.Hour {
		t.Fatal("the zero-byte floor is inside the prober shard retention")
	}
}

// Pure: the zero-byte pass follows the reaper's mode and rate unless its own
// fields narrow them, never runs wider than the reaper, and keeps its horizon
// between the 8-day floor and the reaper's horizon, where it is off. A field
// that does not parse turns it off.
func TestNarrowContractHorizonControlZeroByte(t *testing.T) {
	const day = 24 * time.Hour
	if ContractHorizonControlFieldZeroByteMode != "zeroByteMode" ||
		ContractHorizonControlFieldZeroByteMaxRowsPerSecond != "zeroByteMaxRowsPerSecond" ||
		ContractHorizonControlFieldZeroByteHorizonDays != "zeroByteHorizonDays" {
		t.Fatal("the zero-byte control fields were renamed")
	}
	on := map[string]string{"mode": "on", "maxRowsPerSecond": "2000", "horizonDays": "35"}
	with := func(fields map[string]string) map[string]string {
		merged := map[string]string{}
		for field, value := range on {
			merged[field] = value
		}
		for field, value := range fields {
			merged[field] = value
		}
		return merged
	}
	for _, sample := range []struct {
		name    string
		fields  map[string]string
		mutate  func(*ContractHorizonSettings)
		mode    ContractHorizonMode
		rate    float64
		horizon time.Duration
	}{
		{name: "absent, the reaper off", fields: map[string]string{}, mode: ContractHorizonModeOff, rate: 200, horizon: 8 * day},
		{name: "follows the reaper on", fields: on, mode: ContractHorizonModeOn, rate: 2000, horizon: 8 * day},
		{name: "follows the reaper's dry run", fields: with(map[string]string{"mode": "dry_run"}), mode: ContractHorizonModeDryRun, rate: 2000, horizon: 8 * day},
		{name: "off", fields: with(map[string]string{"zeroByteMode": "off"}), mode: ContractHorizonModeOff, rate: 2000, horizon: 8 * day},
		{name: "dry run under on", fields: with(map[string]string{"zeroByteMode": " dry_run "}), mode: ContractHorizonModeDryRun, rate: 2000, horizon: 8 * day},
		{name: "never wider than the reaper", fields: with(map[string]string{"mode": "dry_run", "zeroByteMode": "on"}), mode: ContractHorizonModeDryRun, rate: 2000, horizon: 8 * day},
		{name: "on under an off reaper", fields: with(map[string]string{"mode": "off", "zeroByteMode": "on"}), mode: ContractHorizonModeOff, rate: 2000, horizon: 8 * day},
		{name: "unparsable mode is off", fields: with(map[string]string{"zeroByteMode": "yes"}), mode: ContractHorizonModeOff, rate: 2000, horizon: 8 * day},
		{name: "own rate", fields: with(map[string]string{"zeroByteMaxRowsPerSecond": "500"}), mode: ContractHorizonModeOn, rate: 500, horizon: 8 * day},
		{name: "own rate under a slower reaper", fields: with(map[string]string{"maxRowsPerSecond": "100", "zeroByteMaxRowsPerSecond": "1500"}), mode: ContractHorizonModeOn, rate: 1500, horizon: 8 * day},
		{name: "rate raised to the ceiling", fields: with(map[string]string{"zeroByteMaxRowsPerSecond": "50000"}), mode: ContractHorizonModeOn, rate: 2000, horizon: 8 * day},
		{name: "zero rate is off", fields: with(map[string]string{"zeroByteMaxRowsPerSecond": "0"}), mode: ContractHorizonModeOff, rate: 0, horizon: 8 * day},
		{name: "unparsable rate is off", fields: with(map[string]string{"zeroByteMaxRowsPerSecond": "fast"}), mode: ContractHorizonModeOff, rate: 0, horizon: 8 * day},
		{name: "NaN rate is off", fields: with(map[string]string{"zeroByteMaxRowsPerSecond": "NaN"}), mode: ContractHorizonModeOff, rate: 0, horizon: 8 * day},
		{name: "ten days", fields: with(map[string]string{"zeroByteHorizonDays": " 10 "}), mode: ContractHorizonModeOn, rate: 2000, horizon: 10 * day},
		{name: "the floor", fields: with(map[string]string{"zeroByteHorizonDays": "8"}), mode: ContractHorizonModeOn, rate: 2000, horizon: 8 * day},
		{name: "a day below the floor", fields: with(map[string]string{"zeroByteHorizonDays": "7"}), mode: ContractHorizonModeOn, rate: 2000, horizon: 8 * day},
		{name: "zero", fields: with(map[string]string{"zeroByteHorizonDays": "0"}), mode: ContractHorizonModeOn, rate: 2000, horizon: 8 * day},
		{name: "the most negative", fields: with(map[string]string{"zeroByteHorizonDays": "-9223372036854775808"}), mode: ContractHorizonModeOn, rate: 2000, horizon: 8 * day},
		{name: "a day inside the horizon", fields: with(map[string]string{"zeroByteHorizonDays": "34"}), mode: ContractHorizonModeOn, rate: 2000, horizon: 34 * day},
		{name: "the horizon is off", fields: with(map[string]string{"zeroByteHorizonDays": "35"}), mode: ContractHorizonModeOff, rate: 2000, horizon: 35 * day},
		{name: "past the horizon is off", fields: with(map[string]string{"zeroByteHorizonDays": "9223372036854775807"}), mode: ContractHorizonModeOff, rate: 2000, horizon: 35 * day},
		{name: "follows a longer horizon", fields: with(map[string]string{"horizonDays": "60", "zeroByteHorizonDays": "50"}), mode: ContractHorizonModeOn, rate: 2000, horizon: 50 * day},
		{name: "unparsable days are off", fields: with(map[string]string{"zeroByteHorizonDays": "8d"}), mode: ContractHorizonModeOff, rate: 2000, horizon: 35 * day},
		{name: "a code horizon below the floor", fields: on, mutate: func(s *ContractHorizonSettings) { s.ZeroByteHorizon = 0 }, mode: ContractHorizonModeOn, rate: 2000, horizon: 8 * day},
		{name: "a code horizon past the horizon is off", fields: on, mutate: func(s *ContractHorizonSettings) { s.ZeroByteHorizon = 91 * day }, mode: ContractHorizonModeOff, rate: 2000, horizon: 35 * day},
		{name: "the code switch off", fields: on, mutate: func(s *ContractHorizonSettings) { s.Enable = false }, mode: ContractHorizonModeOff, rate: 2000, horizon: 8 * day},
	} {
		settings := DefaultContractHorizonSettings()
		if sample.mutate != nil {
			sample.mutate(settings)
		}
		control := NarrowContractHorizonControl(sample.fields, settings)
		if control.ZeroByteMode != sample.mode || control.ZeroByteMaxRowsPerSecond != sample.rate || control.ZeroByteHorizon != sample.horizon {
			t.Errorf("%s: zero-byte mode %s rate %v horizon %s, want %s %v %s", sample.name,
				control.ZeroByteMode, control.ZeroByteMaxRowsPerSecond, control.ZeroByteHorizon, sample.mode, sample.rate, sample.horizon)
		}
		if control.ZeroByteHorizon < ContractHorizonZeroByteFloor || control.Horizon < control.ZeroByteHorizon {
			t.Errorf("%s: zero-byte horizon %s outside [8 days, %s]", sample.name, control.ZeroByteHorizon, control.Horizon)
		}
	}
}

// Pure: the zero-byte pass's position round trips through the task args, a
// cursor a build from before stored reads with none, and a new pass of the
// horizon keeps it: the zero-byte pass never starts over.
func TestContractHorizonCursorRoundTripsZeroByte(t *testing.T) {
	contractId := server.ContractIdFloor(time.Date(2026, time.October, 20, 1, 2, 3, 0, time.UTC))
	zeroByteId := server.ContractIdFloor(time.Date(2026, time.November, 12, 4, 5, 6, 0, time.UTC))
	earlier := ContractHorizonCursor{}
	if err := json.Unmarshal([]byte(`{"contract_id":"`+contractId.String()+`","pass_start_time":"2026-11-15T23:05:00Z","batch_size":2000}`), &earlier); err != nil {
		t.Fatal(err)
	}
	if earlier.ContractId != contractId || earlier.ZeroByteContractId != (server.Id{}) || earlier.ZeroByteBatchSize != 0 {
		t.Fatalf("a cursor from before has a zero-byte position: %+v", earlier)
	}
	stored, err := json.Marshal(earlier)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "zero_byte") {
		t.Fatalf("a cursor without a zero-byte position stores one: %s", stored)
	}

	cursor := earlier
	cursor.ZeroByteContractId = zeroByteId
	cursor.ZeroByteBatchSize = 750
	args, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	var read ContractHorizonCursor
	if err := json.Unmarshal(args, &read); err != nil {
		t.Fatal(err)
	}
	if read.ZeroByteContractId != zeroByteId || read.ZeroByteBatchSize != 750 || read.ContractId != contractId {
		t.Fatalf("the zero-byte position did not round trip: %s read as %+v", args, read)
	}

	now := time.Date(2026, time.November, 22, 23, 5, 0, 0, time.UTC)
	rescan := read.startPass(now)
	if rescan.ContractId != (server.Id{}) || !rescan.PassStartTime.Equal(now) || rescan.BatchSize != 2000 ||
		rescan.ZeroByteContractId != zeroByteId || rescan.ZeroByteBatchSize != 750 {
		t.Fatalf("a new pass of the horizon lost the zero-byte position: %+v", rescan)
	}
}

// Pure: the zero-byte window starts where the horizon's ends, and the
// zero-byte pass reads from its cursor only once that is past the horizon's
// window.
func TestContractHorizonZeroByteWindowFollowsTheHorizon(t *testing.T) {
	const day = 24 * time.Hour
	settings := DefaultContractHorizonSettings()
	now := time.Date(2026, time.November, 21, 22, 30, 0, 0, time.UTC)
	reaper := &contractHorizonReaper{settings: settings, now: now}
	reaper.setHorizon(35 * day)
	reaper.setZeroByteHorizon(8 * day)
	if reaper.windowId != server.ContractIdFloor(now.Add(-35*day-settings.CloseSettleTime)) ||
		reaper.zeroByteWindowId != server.ContractIdFloor(now.Add(-8*day-settings.CloseSettleTime)) ||
		!reaper.zeroByteHorizonTime.Equal(now.Add(-8*day)) || !reaper.zeroByteWindowOpen() {
		t.Fatalf("zero-byte window %s..%s", server.ContractIdTime(reaper.windowId), server.ContractIdTime(reaper.zeroByteWindowId))
	}
	behind := server.ContractIdFloor(now.Add(-40 * day))
	if reaper.zeroByteLower(server.Id{}) != reaper.windowId || reaper.zeroByteLower(behind) != reaper.windowId {
		t.Fatal("a zero-byte cursor behind the horizon's window does not start at it")
	}
	ahead := server.ContractIdFloor(now.Add(-20 * day))
	if reaper.zeroByteLower(ahead) != ahead {
		t.Fatal("a zero-byte cursor past the horizon's window does not resume at itself")
	}
	reaper.setZeroByteHorizon(35 * day)
	if reaper.zeroByteWindowOpen() {
		t.Fatal("a zero-byte horizon at the horizon has a window")
	}
}
