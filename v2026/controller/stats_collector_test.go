package controller

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// the per-country provider gauge is pushed by the taskworker until the
// process exits, so a country that lost its last provider must have its
// series deleted on the next replace, not left pushing its final count
// forever. registration is lazy like the scalar stats gauges, so a vec that
// was never replaced is never exported
func TestStatsGaugeVecReplaceDeletesStaleSeries(t *testing.T) {
	vec := newStatsGaugeVec(
		"test_replace_deletes_stale_series",
		"test",
		"country_code",
		"country",
	)
	name := "urnetwork_stats_test_replace_deletes_stale_series"

	if vec.registered {
		t.Fatal("a stats gauge vec must not register before its first replace")
	}

	vec.replace([]statsLabeledValue{
		{labelValues: []string{"AU", "Australia"}, value: 3},
		{labelValues: []string{"JP", "Japan"}, value: 1},
	})
	if !vec.registered {
		t.Fatal("the first replace must register the vec")
	}
	if count := testutil.CollectAndCount(vec.gauge, name); count != 2 {
		t.Fatalf("series after first replace = %d, want 2", count)
	}
	if value := testutil.ToFloat64(vec.gauge.WithLabelValues("AU", "Australia")); value != 3 {
		t.Fatalf("AU = %f, want 3", value)
	}

	// JP loses its last provider and DE gains one: JP's series must go
	// away, AU must update in place, DE must appear
	vec.replace([]statsLabeledValue{
		{labelValues: []string{"AU", "Australia"}, value: 4},
		{labelValues: []string{"DE", "Germany"}, value: 2},
	})
	if count := testutil.CollectAndCount(vec.gauge, name); count != 2 {
		t.Fatalf("series after second replace = %d, want 2 (JP deleted, DE added)", count)
	}
	if value := testutil.ToFloat64(vec.gauge.WithLabelValues("AU", "Australia")); value != 4 {
		t.Fatalf("AU = %f, want 4", value)
	}
	if value := testutil.ToFloat64(vec.gauge.WithLabelValues("DE", "Germany")); value != 2 {
		t.Fatalf("DE = %f, want 2", value)
	}
	// reading JP through WithLabelValues would recreate it; check the
	// tracked set instead
	if _, ok := vec.current[statsLabelKey([]string{"JP", "Japan"})]; ok {
		t.Fatal("JP must be dropped from the tracked series")
	}

	// an empty replace deletes everything and keeps the vec registered
	vec.replace(nil)
	if count := testutil.CollectAndCount(vec.gauge, name); count != 0 {
		t.Fatalf("series after empty replace = %d, want 0", count)
	}
	if !vec.registered {
		t.Fatal("an empty replace must not unregister the vec")
	}

	// the vec is registered with the default registry, like every stats
	// gauge, so the pusher exports it
	prometheus.DefaultRegisterer.Unregister(vec.gauge)
}
