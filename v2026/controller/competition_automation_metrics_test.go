// Verifies metric snapshots with an explicit clock, including restart and
// clock-skew behavior; there is no scheduler-dependent waiting.
package controller

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A missing heartbeat must not preserve the prior supervisor's healthy signal.
func TestCompetitionAutomationMetricsClearAbsentState(t *testing.T) {
	now := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	past := now.Add(-time.Minute)
	competitionAutomationMetricSnapshot{
		unpublishedSeals: 2, pendingEvents: 3, oldestPublicationAt: &past,
		pendingSources: 4, supervisorHeartbeatAt: &past,
		supervisorExpected: true,
		activeHoldKinds:    []string{"source_activation"}, incidentCount: 1,
	}.publish(now)
	if got := testutil.ToFloat64(competitionAutomationOldestEventAge); got != 60 {
		t.Fatalf("event age = %v, want 60", got)
	}
	competitionAutomationMetricSnapshot{}.publish(now)
	for _, gauge := range []struct {
		name  string
		value float64
	}{
		{name: "seals", value: testutil.ToFloat64(competitionAutomationUnpublishedSeals)},
		{name: "events", value: testutil.ToFloat64(competitionAutomationPendingEvents)},
		{name: "event age", value: testutil.ToFloat64(competitionAutomationOldestEventAge)},
		{name: "sources", value: testutil.ToFloat64(competitionAutomationPendingSources)},
		{name: "supervisor expected", value: testutil.ToFloat64(competitionAutomationSupervisorExpected)},
		{name: "incident count", value: testutil.ToFloat64(competitionAutomationIncidentCount)},
		{name: "source hold", value: testutil.ToFloat64(competitionAutomationHoldActive.WithLabelValues("source_activation"))},
	} {
		if gauge.value != 0 {
			t.Errorf("%s = %v after empty snapshot", gauge.name, gauge.value)
		}
	}
	if got := testutil.ToFloat64(competitionAutomationSupervisorHeartbeatAge); got != -1 {
		t.Fatalf("missing heartbeat age = %v, want -1", got)
	}
}

// Future timestamps caused by clock skew cannot become negative health ages.
func TestCompetitionAutomationMetricsClampFutureTimes(t *testing.T) {
	now := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	future := now.Add(time.Minute)
	competitionAutomationMetricSnapshot{
		oldestPublicationAt: &future, supervisorHeartbeatAt: &future,
	}.publish(now)
	if got := testutil.ToFloat64(competitionAutomationOldestEventAge); got != 0 {
		t.Errorf("event age = %v, want 0", got)
	}
	if got := testutil.ToFloat64(competitionAutomationSupervisorHeartbeatAge); got != 0 {
		t.Errorf("heartbeat age = %v, want 0", got)
	}
}
