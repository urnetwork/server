// Backlog visibility: the published depth reflects what producers met at the
// claim, and an owner that stops reading stays visible by its observed time.
// The in-memory shard and gauge reader are the drain tests' fixtures.
package model

import (
	"strconv"
	"testing"
	"time"
)

// DrainProviderEgressTallies publishes the drain's Backlog. At 96 runs/s with
// 100 s claims a shard is beyond its two-claim buffer and refuses handoffs, so
// its published fill must be at the top of the buffer. Publishing the drain's
// last call instead reported the trough its own cleanup left, about half full
// while handoffs were refused, below the 0.5 warning.
func TestProviderEgressTallyDrainPublishesTheClaimsPeakAtSaturation(t *testing.T) {
	peakFill := func(rate float64, cycle time.Duration) (peak float64, full int) {
		queue := newTestingTallyQueue(t)
		start := queue.now
		size := testingTallySize(t, testingUrlProbeTally(start, 0))
		cursor := "0-0"
		nextClaim := start.Add(cycle)
		for i := 0; ; i++ {
			at := start.Add(time.Duration(float64(i) / rate * float64(time.Second)))
			if !at.Before(start.Add(time.Hour)) {
				break
			}
			for !nextClaim.After(at) {
				queue.now = nextClaim
				drain, err := drainProviderEgressTallies(t.Context(), cursor, false, defaultProviderEgressTallyDrainBudget(), queue.reader(cursor), queue.clock)
				if err != nil {
					t.Fatal(err)
				}
				peak = max(peak, drain.Backlog.FillRatio())
				cursor = drain.NextCursor
				nextClaim = nextClaim.Add(cycle)
			}
			if !queue.push(at.UnixMilli(), testingUrlProbeTally(at, i%64), size) {
				full += 1
			}
		}
		return peak, full
	}
	peak, full := peakFill(96, 100*time.Second)
	t.Logf("96 runs/s, 100s claims: published peak fill %.3f, refused %d", peak, full)
	// The first call deletes at most one page before it measures.
	if full == 0 || peak < float64(providerEgressTallyQueueLength-providerEgressTallyPageLength)/float64(providerEgressTallyQueueLength) {
		t.Fatalf("a shard refusing %d handoffs published a peak fill of %.3f", full, peak)
	}
	// Twice the wave peak on the hottest shard at 50 s claims stays quiet.
	peak, full = peakFill(0.354*180, 50*time.Second)
	t.Logf("63.7 runs/s, 50s claims: published peak fill %.3f, refused %d", peak, full)
	if 0 < full || 0.5 <= peak {
		t.Fatalf("a shard keeping up refused %d handoffs or published a peak fill of %.3f", full, peak)
	}
}

// A shard last read here more than five minutes ago withholds its depth but
// not its observed time, and a shard not read here since start reports zero,
// so a stuck owner shows as an aging or zero read time instead of vanishing.
func TestProviderEgressTallyBacklogGaugeKeepsEveryObservedTime(t *testing.T) {
	now := time.Date(2026, 10, 14, 2, 40, 0, 0, time.UTC)
	collector := newProviderEgressTallyBacklogCollector(func() time.Time { return now })
	if values := testingGaugeValues(t, collector); len(values) != 0 {
		t.Fatalf("a process that never drained published tally gauges: %+v", values)
	}
	fresh := now.Add(-10 * time.Second)
	stale := now.Add(-6 * time.Minute)
	collector.observe(3, ProviderEgressTallyBacklog{Records: 8192, ObservedAt: fresh})
	collector.observe(7, ProviderEgressTallyBacklog{Records: 16128, ObservedAt: stale})
	values := testingGaugeValues(t, collector)
	if len(values) != ProviderEgressTallyShardCount {
		t.Fatalf("observed times published for %d of %d shards: %+v", len(values), ProviderEgressTallyShardCount, values)
	}
	for shard := range ProviderEgressTallyShardCount {
		gauges := values[strconv.Itoa(shard)]
		observed, ok := gauges["queue_observed_timestamp_seconds"]
		_, depth := gauges["queue_records"]
		switch shard {
		case 3:
			if !ok || observed != float64(fresh.Unix()) || !depth {
				t.Fatalf("fresh shard gauges=%+v", gauges)
			}
		case 7:
			if !ok || observed != float64(stale.Unix()) || depth {
				t.Fatalf("stale shard hid its read time or kept a stale depth: %+v", gauges)
			}
		default:
			if !ok || observed != 0 || depth {
				t.Fatalf("unread shard %d gauges=%+v", shard, gauges)
			}
		}
	}
}
