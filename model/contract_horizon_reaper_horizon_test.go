package model

import (
	"maps"
	"testing"
	"time"
)

// Pure: the control's horizonDays picks the horizon between the 45-day floor
// and the code's horizon, 91 days. It reads whole days. Absent or unparsable,
// it keeps the code's horizon, the longest; a value past either end counts as
// that end, so no value reaches inside 45 days, including one whose days would
// overflow a duration. It moves no other field, and no other field moves it.
func TestNarrowContractHorizonControlHorizonDays(t *testing.T) {
	const day = 24 * time.Hour
	if ContractHorizonFloor != 45*day || ContractHorizonControlFieldHorizonDays != "horizonDays" {
		t.Fatal("the floor is not 45 days, or the field is not horizonDays", ContractHorizonFloor, ContractHorizonControlFieldHorizonDays)
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
		{name: "the floor", fields: map[string]string{"horizonDays": "45"}, horizon: 45 * day},
		{name: "a day past the floor", fields: map[string]string{"horizonDays": "46"}, horizon: 46 * day},
		{name: "between", fields: map[string]string{"mode": "on", "horizonDays": " 60 "}, horizon: 60 * day},
		{name: "the code's horizon", fields: map[string]string{"horizonDays": "91"}, horizon: 91 * day},
		{name: "a day below the floor", fields: map[string]string{"horizonDays": "44"}, horizon: 45 * day},
		{name: "one day", fields: map[string]string{"horizonDays": "1"}, horizon: 45 * day},
		{name: "zero", fields: map[string]string{"horizonDays": "0"}, horizon: 45 * day},
		{name: "negative", fields: map[string]string{"horizonDays": "-30"}, horizon: 45 * day},
		{name: "the most negative", fields: map[string]string{"horizonDays": "-9223372036854775808"}, horizon: 45 * day},
		{name: "a day above the code's horizon", fields: map[string]string{"horizonDays": "92"}, horizon: 91 * day},
		{name: "a year", fields: map[string]string{"horizonDays": "365"}, horizon: 91 * day},
		// 106752 days is past the largest duration: multiplied first, it
		// would wrap negative and clamp to the floor
		{name: "days past the largest duration", fields: map[string]string{"horizonDays": "106752"}, horizon: 91 * day},
		{name: "the largest", fields: map[string]string{"horizonDays": "9223372036854775807"}, horizon: 91 * day},
		{name: "out of range", fields: map[string]string{"horizonDays": "99999999999999999999"}, horizon: 91 * day},
		{name: "empty", fields: map[string]string{"horizonDays": ""}, horizon: 91 * day},
		{name: "garbage", fields: map[string]string{"mode": "on", "horizonDays": "soon"}, horizon: 91 * day},
		{name: "fractional", fields: map[string]string{"horizonDays": "45.5"}, horizon: 91 * day},
		{name: "with a unit", fields: map[string]string{"horizonDays": "45d"}, horizon: 91 * day},
		{name: "a duration", fields: map[string]string{"horizonDays": "1080h"}, horizon: 91 * day},
		{name: "hexadecimal", fields: map[string]string{"horizonDays": "0x2d"}, horizon: 91 * day},
		{name: "NaN", fields: map[string]string{"horizonDays": "NaN"}, horizon: 91 * day},
		{name: "beside the code switch off", fields: map[string]string{"mode": "on", "horizonDays": "45"}, mutate: func(s *ContractHorizonSettings) { s.Enable = false },
			horizon: 45 * day},
		{name: "a code horizon below the floor", fields: map[string]string{}, mutate: codeHorizon(30 * day), horizon: 45 * day},
		{name: "a code horizon below the floor caps the control", fields: map[string]string{"horizonDays": "60"}, mutate: codeHorizon(30 * day), horizon: 45 * day},
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

// Pure: a count of days becomes the horizon only between the floor and the
// ceiling, clamped before it is a duration.
func TestContractHorizonFromDays(t *testing.T) {
	const day = 24 * time.Hour
	for _, sample := range []struct {
		days    int64
		ceiling time.Duration
		want    time.Duration
	}{
		{days: 45, ceiling: 91 * day, want: 45 * day},
		{days: 60, ceiling: 91 * day, want: 60 * day},
		{days: 91, ceiling: 91 * day, want: 91 * day},
		{days: 44, ceiling: 91 * day, want: 45 * day},
		{days: -1 << 63, ceiling: 91 * day, want: 45 * day},
		{days: 92, ceiling: 91 * day, want: 91 * day},
		{days: 1<<63 - 1, ceiling: 91 * day, want: 91 * day},
		// a ceiling at the floor admits nothing else
		{days: 46, ceiling: 45 * day, want: 45 * day},
		{days: 46, ceiling: 45*day + time.Hour, want: 45*day + time.Hour},
	} {
		if got := contractHorizonFromDays(sample.days, sample.ceiling); got != sample.want {
			t.Errorf("%d days under %s: %s, want %s", sample.days, sample.ceiling, got, sample.want)
		}
	}
}
