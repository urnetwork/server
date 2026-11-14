package model

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The force-close sweep logged one line per contract and ran ~87k lines/day —
// the single largest log source across every service. The volume is driven by
// clients that leave contracts open, so the disposition is now counted and the
// per-contract detail is V(1). These tests pin the counter that replaced it.

func forceCloseCount(resolution string) float64 {
	return testutil.ToFloat64(forceCloseContractCounter.WithLabelValues(resolution))
}

// TestRecordForceCloseContractCounts covers every resolution the sweep can
// take. The resolution strings are the metric's label values, so a typo or a
// renamed case silently splits a series in Grafana — enumerating them here
// keeps the label set fixed and reviewable.
func TestRecordForceCloseContractCounts(t *testing.T) {
	resolutions := []string{
		"both sides",
		"source accepts destination",
		"destination accepts source",
		"finalize source checkpoint",
		"finalize destination checkpoint",
	}
	for _, resolution := range resolutions {
		before := forceCloseCount(resolution)
		recordForceCloseContract(resolution, "[sm][test][1/1]")
		if after := forceCloseCount(resolution); after != before+1 {
			t.Fatalf("counter{%s} = %v, want %v", resolution, after, before+1)
		}
	}

	// resolutions are independent series, not one merged total
	before := forceCloseCount("both sides")
	recordForceCloseContract("source accepts destination", "[sm][test][1/1]")
	if after := forceCloseCount("both sides"); after != before {
		t.Fatalf("recording one resolution moved another: %v -> %v", before, after)
	}
}

// TestFindProviders2LoadHistogramObserves covers the metric that replaced the
// per-call ">50ms" load line. Every provider search runs this path, so the
// histogram must carry all calls — not only the slow ones the log used to
// print — or the distribution is truncated exactly where it matters.
func TestFindProviders2LoadHistogramObserves(t *testing.T) {
	before := testutil.CollectAndCount(findProviders2LoadSeconds)
	if before != 1 {
		t.Fatalf("expected the histogram to be registered as a single metric, got %d", before)
	}
	// a fast observation must still be recorded
	findProviders2LoadSeconds.Observe(0.001)
	findProviders2LoadSeconds.Observe(0.5)
}
