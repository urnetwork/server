package work

import (
	"encoding/json"
	"testing"
	"time"
)

// The contract chain reaches each day's partitions a minute into the day: at
// the cadence's time unless the day's first analyze comes first, at once when
// that passed while the run ran, and on the cadence alone for a result from a
// build without it (or before the cutover). Before, a run at 23:55 UTC left
// the new day to its next run at 00:10, or to autovacuum.
func TestMaintainContractPartitionsNextRunAtMeetsTheOpenedDay(t *testing.T) {
	settings := DefaultMaintainContractPartitionsTaskSettings()
	before := time.Date(2026, 11, 25, 23, 55, 0, 0, time.UTC)
	opened := time.Date(2026, 11, 26, 0, 1, 0, 0, time.UTC)
	for _, c := range []struct {
		result MaintainContractPartitionsResult
		now    time.Time
		want   time.Time
	}{
		{result: MaintainContractPartitionsResult{OpenLeafAnalyzeAt: opened}, now: before, want: opened},
		{result: MaintainContractPartitionsResult{More: true, OpenLeafAnalyzeAt: opened}, now: before, want: before.Add(settings.RetryInterval)},
		{result: MaintainContractPartitionsResult{OpenLeafAnalyzeAt: opened}, now: before.Add(-time.Hour), want: before.Add(-time.Hour).Add(settings.Interval)},
		// a run that started before 00:01 and ended after it
		{result: MaintainContractPartitionsResult{OpenLeafAnalyzeAt: opened}, now: opened.Add(4 * time.Minute), want: opened.Add(4 * time.Minute)},
		{result: MaintainContractPartitionsResult{}, now: before, want: before.Add(settings.Interval)},
	} {
		if got := maintainContractPartitionsNextRunAt(settings, &c.result, c.now); !got.Equal(c.want) {
			t.Errorf("result %+v at %s: next run %s, want %s", c.result, c.now, got, c.want)
		}
	}
}

// The chain's cadence keeps a retry sooner than a run, and the budget inside
// the run's max time; a result carries its open-leaf time through the task
// queue's JSON, and a result written before the field decodes to none.
func TestMaintainContractPartitionsTaskSettingsAndResult(t *testing.T) {
	settings := DefaultMaintainContractPartitionsTaskSettings()
	if settings.StartDelay <= 0 || settings.RetryInterval <= 0 || settings.Interval <= settings.RetryInterval ||
		settings.Budget <= 0 || settings.MaxTime <= settings.Budget {
		t.Fatalf("contract partition task settings %+v", settings)
	}
	opened := time.Date(2026, 11, 26, 0, 1, 0, 0, time.UTC)
	encoded, err := json.Marshal(&MaintainContractPartitionsResult{More: true, OpenLeafAnalyzeAt: opened})
	if err != nil {
		t.Fatal(err)
	}
	var decoded MaintainContractPartitionsResult
	if err := json.Unmarshal(encoded, &decoded); err != nil || !decoded.More || !decoded.OpenLeafAnalyzeAt.Equal(opened) {
		t.Fatalf("decoded %+v from %s: %v", decoded, encoded, err)
	}
	var older MaintainContractPartitionsResult
	if err := json.Unmarshal([]byte(`{"cursor":{},"more":false}`), &older); err != nil || !older.OpenLeafAnalyzeAt.IsZero() {
		t.Fatalf("an older result decoded to %+v: %v", older, err)
	}
}
