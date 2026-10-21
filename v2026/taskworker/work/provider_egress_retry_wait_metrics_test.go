package work

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestBlackholeRetryWaitMetricsDistinguishWaitingLoadsFromWorkers(t *testing.T) {
	metrics := newBlackholeRetryWaitMetrics()
	metrics.observe(true)
	metrics.observe(true)
	if got := testutil.ToFloat64(metrics.active); got != 2 {
		t.Errorf("active waiting loads = %v, want 2", got)
	}
	metrics.observe(false)
	if got := testutil.ToFloat64(metrics.active); got != 1 {
		t.Errorf("active after one wait ends = %v, want 1", got)
	}
	metrics.observe(false)
	if got := testutil.ToFloat64(metrics.active); got != 0 {
		t.Errorf("active after all waits end = %v, want 0", got)
	}
	if got := testutil.ToFloat64(metrics.started); got != 2 {
		t.Errorf("started waits = %v, want 2", got)
	}
	if got := testutil.ToFloat64(metrics.completed); got != 2 {
		t.Errorf("completed waits = %v, want 2", got)
	}
}
