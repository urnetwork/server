package work

import (
	"testing"
	"time"
)

// Passes run on the quarter hour, a fixed period apart, under an hour.
func TestSampleSessionLevelsRunsOnTheQuarterHour(t *testing.T) {
	if SampleSessionLevelsPeriod >= time.Hour {
		t.Fatal("the pass period must stay under an hour", SampleSessionLevelsPeriod)
	}
	for _, c := range []struct{ now, next string }{
		{"2026-11-18T19:00:00Z", "2026-11-18T19:15:00Z"},
		{"2026-11-18T19:07:31Z", "2026-11-18T19:15:00Z"},
		{"2026-11-18T19:59:59Z", "2026-11-18T20:00:00Z"},
	} {
		now, _ := time.Parse(time.RFC3339, c.now)
		next, _ := time.Parse(time.RFC3339, c.next)
		if got := nextSampleSessionLevelsTime(now); !got.Equal(next) {
			t.Fatal(c.now, got)
		}
	}
	if sampleSessionLevelsBudget >= SampleSessionLevelsPeriod {
		t.Fatal("a pass may overrun its period")
	}
}
