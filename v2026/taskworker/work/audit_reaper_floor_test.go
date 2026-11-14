package work

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026/model"
)

// The value of the unlabeled counter the process exports as name, as a scrape
// reads it; zero when there is none.
func auditRollupTestCounter(name string) float64 {
	// a collector that fails elsewhere still leaves this family in the result
	families, _ := prometheus.DefaultGatherer.Gather()
	for _, family := range families {
		if family.GetName() == name && len(family.GetMetric()) == 1 && family.GetMetric()[0].GetCounter() != nil {
			return family.GetMetric()[0].GetCounter().GetValue()
		}
	}
	return 0
}

// Pure: a rollup chain left behind for weeks resumes no earlier than the
// contract horizon reaper's 35-day floor allows, so it never replaces the row
// of a day whose contracts the reaper can have removed. The normal window,
// three days back, is unchanged. Each clamp counts once.
func TestTransferAuditRollupResumesWhereTheReaperCannotReach(t *testing.T) {
	const day = 24 * time.Hour
	now := time.Date(2026, time.November, 8, 10, 0, 0, 0, time.UTC)
	idle := func(context.Context) bool { return false }
	var rolled []time.Time
	advance := func(_ context.Context, sum *model.TransferCloseByteSum, _ int) bool {
		rolled = append(rolled, sum.Start)
		return true
	}

	clamps := auditRollupTestCounter("urnetwork_transfer_audit_rollup_clamped_total")
	stale := now.Add(-50 * day)
	result := rollupTransferAuditEvents(context.Background(), &RollupTransferAuditEventsArgs{MinTime: &stale}, now, idle, advance)
	floor := now.Add(-35 * day)
	if len(rolled) != 1 || rolled[0].Before(floor) || floor.Add(3*day).Before(rolled[0]) || !rolled[0].Equal(rolled[0].Truncate(day)) || result.DayCount != 1 {
		t.Fatalf("a chain from %s rolled %v (%+v); want one whole day from %s, within three days of it", stale, rolled, result, floor)
	}
	if counted := auditRollupTestCounter("urnetwork_transfer_audit_rollup_clamped_total") - clamps; counted != 1 {
		t.Fatalf("the clamp counted %v times, want once", counted)
	}

	rolled = nil
	rollupTransferAuditEvents(context.Background(), nil, now, idle, advance)
	if len(rolled) != 1 || !rolled[0].Equal(time.Date(2026, time.November, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("the normal window rolled %v, want 11-05", rolled)
	}
	if counted := auditRollupTestCounter("urnetwork_transfer_audit_rollup_clamped_total") - clamps; counted != 1 {
		t.Fatalf("the normal window counted a clamp: %v in all, want 1", counted)
	}
}
