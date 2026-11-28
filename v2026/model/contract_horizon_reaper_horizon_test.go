package model

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Pure: the control's horizonDays picks the horizon between the 14-day floor
// and the code's horizon, 91 days. It reads whole days. Absent or unparsable,
// it keeps the code's horizon, the longest; a value past either end counts as
// that end, so no value reaches inside 14 days, including one whose days would
// overflow a duration. It moves no other field, and no other field moves it.
// The C1 transition's 18 days (owner, 2026-11-23) is admitted as set; before
// the floor moved from 35 days it counted as 35, as did everything from 14 to
// 34.
func TestNarrowContractHorizonControlHorizonDays(t *testing.T) {
	const day = 24 * time.Hour
	if ContractHorizonFloor != 14*day || ContractHorizonControlFieldHorizonDays != "horizonDays" {
		t.Fatal("the floor is not 14 days, or the field is not horizonDays", ContractHorizonFloor, ContractHorizonControlFieldHorizonDays)
	}
	codeHorizon := func(horizon time.Duration) func(*ContractHorizonSettings) {
		return func(settings *ContractHorizonSettings) {
			settings.Horizon = horizon
		}
	}
	for _, sample := range []struct {
		name    string
		fields  map[string]string
		mutate  func(*ContractHorizonSettings)
		horizon time.Duration
	}{
		{name: "absent", fields: map[string]string{}, horizon: 91 * day},
		{name: "absent beside the other fields", fields: map[string]string{"mode": "on", "maxRowsPerSecond": "100", "batchSize": "500"}, horizon: 91 * day},
		{name: "the floor", fields: map[string]string{"horizonDays": "14"}, horizon: 14 * day},
		{name: "a day past the floor", fields: map[string]string{"horizonDays": "15"}, horizon: 15 * day},
		{name: "the transition's horizon", fields: map[string]string{"mode": "on", "horizonDays": "18"}, horizon: 18 * day},
		{name: "three weeks", fields: map[string]string{"horizonDays": "21"}, horizon: 21 * day},
		{name: "thirty days", fields: map[string]string{"horizonDays": "30"}, horizon: 30 * day},
		{name: "a day below the earlier floor", fields: map[string]string{"horizonDays": "34"}, horizon: 34 * day},
		{name: "the earlier floor", fields: map[string]string{"horizonDays": "35"}, horizon: 35 * day},
		{name: "a day past the earlier floor", fields: map[string]string{"horizonDays": "36"}, horizon: 36 * day},
		{name: "the floor before that", fields: map[string]string{"horizonDays": "45"}, horizon: 45 * day},
		{name: "between", fields: map[string]string{"mode": "on", "horizonDays": " 60 "}, horizon: 60 * day},
		{name: "the code's horizon", fields: map[string]string{"horizonDays": "91"}, horizon: 91 * day},
		{name: "a day below the floor", fields: map[string]string{"horizonDays": "13"}, horizon: 14 * day},
		{name: "a week", fields: map[string]string{"horizonDays": "7"}, horizon: 14 * day},
		{name: "one day", fields: map[string]string{"horizonDays": "1"}, horizon: 14 * day},
		{name: "zero", fields: map[string]string{"horizonDays": "0"}, horizon: 14 * day},
		{name: "negative", fields: map[string]string{"horizonDays": "-30"}, horizon: 14 * day},
		{name: "the most negative", fields: map[string]string{"horizonDays": "-9223372036854775808"}, horizon: 14 * day},
		{name: "a day above the code's horizon", fields: map[string]string{"horizonDays": "92"}, horizon: 91 * day},
		{name: "a year", fields: map[string]string{"horizonDays": "365"}, horizon: 91 * day},
		// 106752 days is past the largest duration: multiplied first, it
		// would wrap negative and clamp to the floor
		{name: "days past the largest duration", fields: map[string]string{"horizonDays": "106752"}, horizon: 91 * day},
		{name: "the largest", fields: map[string]string{"horizonDays": "9223372036854775807"}, horizon: 91 * day},
		{name: "out of range", fields: map[string]string{"horizonDays": "99999999999999999999"}, horizon: 91 * day},
		{name: "empty", fields: map[string]string{"horizonDays": ""}, horizon: 91 * day},
		{name: "garbage", fields: map[string]string{"mode": "on", "horizonDays": "soon"}, horizon: 91 * day},
		{name: "fractional", fields: map[string]string{"horizonDays": "18.5"}, horizon: 91 * day},
		{name: "with a unit", fields: map[string]string{"horizonDays": "18d"}, horizon: 91 * day},
		{name: "a duration", fields: map[string]string{"horizonDays": "432h"}, horizon: 91 * day},
		{name: "hexadecimal", fields: map[string]string{"horizonDays": "0x12"}, horizon: 91 * day},
		{name: "NaN", fields: map[string]string{"horizonDays": "NaN"}, horizon: 91 * day},
		{name: "beside the code switch off", fields: map[string]string{"mode": "on", "horizonDays": "18"}, mutate: func(s *ContractHorizonSettings) { s.Enable = false },
			horizon: 18 * day},
		{name: "a code horizon below the floor", fields: map[string]string{}, mutate: codeHorizon(10 * day), horizon: 14 * day},
		{name: "a code horizon below the floor caps the control", fields: map[string]string{"horizonDays": "60"}, mutate: codeHorizon(10 * day), horizon: 14 * day},
		{name: "a code horizon of 30 days", fields: map[string]string{}, mutate: codeHorizon(30 * day), horizon: 30 * day},
		{name: "inside a code horizon of 30 days", fields: map[string]string{"horizonDays": "18"}, mutate: codeHorizon(30 * day), horizon: 18 * day},
		{name: "past a code horizon of 30 days", fields: map[string]string{"horizonDays": "60"}, mutate: codeHorizon(30 * day), horizon: 30 * day},
		{name: "a longer code horizon", fields: map[string]string{}, mutate: codeHorizon(120 * day), horizon: 120 * day},
		{name: "inside a longer code horizon", fields: map[string]string{"horizonDays": "100"}, mutate: codeHorizon(120 * day), horizon: 100 * day},
		{name: "past a longer code horizon", fields: map[string]string{"horizonDays": "200"}, mutate: codeHorizon(120 * day), horizon: 120 * day},
		{name: "past a code horizon of part days", fields: map[string]string{"horizonDays": "92"}, mutate: codeHorizon(91*day + 12*time.Hour), horizon: 91*day + 12*time.Hour},
		{name: "the last whole day of a code horizon of part days", fields: map[string]string{"horizonDays": "91"}, mutate: codeHorizon(91*day + 12*time.Hour), horizon: 91 * day},
	} {
		settings := DefaultContractHorizonSettings()
		if sample.mutate != nil {
			sample.mutate(settings)
		}
		control := NarrowContractHorizonControl(sample.fields, settings)
		if control.Horizon != sample.horizon {
			t.Errorf("%s: horizon %s, want %s", sample.name, control.Horizon, sample.horizon)
		}
		others := maps.Clone(sample.fields)
		delete(others, ContractHorizonControlFieldHorizonDays)
		without := NarrowContractHorizonControl(others, settings)
		if control.Present != (0 < len(sample.fields)) || control.Mode != without.Mode ||
			control.MaxRowsPerSecond != without.MaxRowsPerSecond || control.BatchSize != without.BatchSize {
			t.Errorf("%s: horizonDays moved another field: %+v, without it %+v", sample.name, control, without)
		}
	}
}

// Pure: a count of days becomes the horizon only between the floor, 14 days,
// and the ceiling, clamped before it is a duration. 18 is 18 days; before the
// floor moved from 35 days, 18 was 35 days.
func TestContractHorizonFromDays(t *testing.T) {
	const day = 24 * time.Hour
	for _, sample := range []struct {
		days    int64
		ceiling time.Duration
		want    time.Duration
	}{
		{days: 14, ceiling: 91 * day, want: 14 * day},
		{days: 15, ceiling: 91 * day, want: 15 * day},
		{days: 18, ceiling: 91 * day, want: 18 * day},
		{days: 21, ceiling: 91 * day, want: 21 * day},
		{days: 34, ceiling: 91 * day, want: 34 * day},
		{days: 35, ceiling: 91 * day, want: 35 * day},
		{days: 45, ceiling: 91 * day, want: 45 * day},
		{days: 60, ceiling: 91 * day, want: 60 * day},
		{days: 91, ceiling: 91 * day, want: 91 * day},
		{days: 13, ceiling: 91 * day, want: 14 * day},
		{days: 0, ceiling: 91 * day, want: 14 * day},
		{days: -1 << 63, ceiling: 91 * day, want: 14 * day},
		{days: 92, ceiling: 91 * day, want: 91 * day},
		{days: 1<<63 - 1, ceiling: 91 * day, want: 91 * day},
		// a ceiling at the floor admits nothing else
		{days: 15, ceiling: 14 * day, want: 14 * day},
		{days: 15, ceiling: 14*day + time.Hour, want: 14*day + time.Hour},
		{days: 18, ceiling: 18 * day, want: 18 * day},
		{days: 19, ceiling: 18 * day, want: 18 * day},
	} {
		if got := contractHorizonFromDays(sample.days, sample.ceiling); got != sample.want {
			t.Errorf("%d days under %s: %s, want %s", sample.days, sample.ceiling, got, sample.want)
		}
	}
}

// Pure: the contract days retire at the horizon the reaper deletes at, down to
// the same floor (server.ContractDayRetentionFloor). The control's horizon
// reaches the days as it reaches the reaper: 18 days retires a day 18 days
// past its end, where the 35-day day floor kept it 35 days whatever the
// reaper deleted, so a horizon cut returned nothing after the cutover; 13
// days counts as the floor in both.
func TestContractDayRetentionFloorIsTheReaperFloor(t *testing.T) {
	const day = 24 * time.Hour
	if server.ContractDayRetentionFloor != ContractHorizonFloor {
		t.Fatalf("the days retire from %s, the reaper deletes from %s", server.ContractDayRetentionFloor, ContractHorizonFloor)
	}
	for horizonDays, want := range map[string]time.Duration{
		"18": 18 * day,
		"14": 14 * day,
		"13": 14 * day,
		"21": 21 * day,
		"35": 35 * day,
		"91": 91 * day,
		"":   91 * day,
	} {
		fields := map[string]string{"mode": "on"}
		if horizonDays != "" {
			fields[ContractHorizonControlFieldHorizonDays] = horizonDays
		}
		control := NarrowContractHorizonControl(fields, DefaultContractHorizonSettings())
		retention := server.ContractDayRetention(server.WithContractHorizon(context.Background(), control.Horizon))
		if control.Horizon != want || retention != want {
			t.Errorf("horizonDays %q: the reaper deletes at %s and the days retire at %s, want both %s", horizonDays, control.Horizon, retention, want)
		}
	}
}
