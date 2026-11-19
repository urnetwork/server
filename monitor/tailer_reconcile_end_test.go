package monitor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Exercise actual Warpctl argv after the wall clock advances between pages.
// The aggregate, every configured block and its continuation share one end.
func TestReconcileUsesOneSourceEndForAggregateBlockAndContinuation(t *testing.T) {
	end := time.Date(2026, 11, 18, 16, 50, 0, 123, time.UTC)
	now := end
	boundary := end.Add(-time.Minute).Truncate(time.Microsecond)
	lower := end.Add(-logReconcileLookback)
	serviceArgs := []string{"logs", "fixture", "proxy"}
	type request struct {
		block string
		start time.Time
		full  bool
	}
	want := []request{{"", lower, true}, {"first", lower, true}, {"first", boundary, false}, {"second", lower, false}}
	calls := 0
	line := "[fixture][" + boundary.UTC().Format(time.RFC3339Nano) + "]ordinary\n"
	fullPage := strings.Repeat(line, logReconcileLimit)
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		if calls >= len(want) {
			t.Fatal("unexpected extra query")
		}
		page := want[calls]
		expected := slices.Clone(serviceArgs)
		if page.block != "" {
			expected = append(expected, page.block)
		}
		expected = append(expected, "--since="+page.start.UTC().Format(time.RFC3339Nano),
			"--until="+end.UTC().Format(time.RFC3339Nano), fmt.Sprintf("--limit=%d", logReconcileLimit))
		if name != "warpctl" || !slices.Equal(args, expected) {
			t.Fatalf("logical attempt changed bounds, partition, or budget on request %d", calls+1)
		}
		calls++
		now = now.Add(time.Second)
		if page.full {
			return fullPage, nil
		}
		return line, nil
	}}
	cfg := &monitorConfig{env: "fixture", logServiceBlocks: map[string][]string{"proxy": {"first", "second"}}}
	tailer := newLogTailer("proxy", &probeEnv{cfg: cfg, runner: &sourceRunner{source: source}, now: func() time.Time { return now }})
	tailer.reconcileOnce(context.Background())
	_, _, completed, latest, failure := tailer.reconcileSnapshot()
	if calls != len(want) || failure != "" || !completed.Equal(now) || !latest.end().Equal(end) || !latest.freshAt(now) {
		t.Fatal("finite partitioned read did not complete its original source interval")
	}
}

// Local completion and confirmed source cutoff have different authority.
func TestReconcileSlowCompletedSnapshotCannotClearVisibility(t *testing.T) {
	end := time.Date(2026, 11, 18, 16, 50, 0, 0, time.UTC)
	now := end
	tailer := syntheticReceiptTailer(&now, "proxy")
	tailer.reconcile = func(context.Context, time.Time, time.Time, []string) (string, error) {
		now = end.Add(2 * logReconcileInterval)
		return "", nil
	}
	tailer.reconcileOnce(context.Background())
	probe := &logTailProbe{tailers: []*logTailer{tailer}}
	receipt := probe.reconcileReceipt(fixedReconcileReceiptClock(now))
	row := requireCollectorScope(t, receipt, "proxy")
	_, started, completed, latest, failure := tailer.reconcileSnapshot()
	if row.CoverageState != "stale-window" || receipt.Fresh != 0 || receipt.ConsecutiveTwo != 0 ||
		row.LastSuccessAt == nil || !row.LastSuccessAt.Equal(now) || row.LatestCompletedAt == nil || !row.LatestCompletedAt.Equal(now) ||
		failure != "" || tailerReconcileFinding("proxy", now, started, completed, latest, failure).healthy {
		t.Fatal("historical source coverage became current on local completion")
	}
}

// Even timely terminal records can describe disjoint source windows. An
// exclusive end touching the next inclusive start has no repeated source time.
func TestReconcileFreshPairRequiresPositiveSourceOverlap(t *testing.T) {
	end := time.Date(2026, 11, 18, 16, 50, 0, 0, time.UTC)
	previous := logReconcileSuccess{start: end.Add(-logReconcileLookback), completed: end.Add(80 * time.Second)}
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		lower := end.Add(offset)
		next := logReconcileSuccess{start: lower, completed: lower.Add(logReconcileLookback)}
		if !previous.freshAt(previous.completed) || !next.freshAt(next.completed) || next.completed.Sub(previous.completed) >= 2*logReconcileInterval {
			t.Fatal("fixture failed to isolate source overlap")
		}
		if previous.advancesTo(next) != (offset < 0) {
			t.Fatal("recovery pair misclassified the exclusive source boundary")
		}
	}
}
