// Scheduling regressions use explicit clocks, never main state or wall-clock waits.
package main

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// New pins require a real future instant and database-safe timestamp precision.
func TestBootstrapSourceOpeningValidatesNewSchedule(t *testing.T) {
	now := time.Date(2035, 4, 1, 0, 0, 0, 0, time.UTC)
	opensAt := now.Add(48 * time.Hour)
	actual, err := bootstrapSourceOpening(opensAt.Format(time.RFC3339), nil, now)
	if err != nil || !actual.Equal(opensAt) || actual.Location() != time.UTC {
		t.Fatalf("future opening rejected: %v %v", actual, err)
	}
	for _, invalid := range []string{"not-a-time", now.Format(time.RFC3339), now.Add(-time.Second).Format(time.RFC3339), "2035-04-03T00:00:00.000000001Z", "0001-01-01T00:00:00Z"} {
		if _, err := bootstrapSourceOpening(invalid, nil, now); err == nil {
			t.Fatalf("invalid new schedule accepted: %q", invalid)
		}
	}
	actual, err = bootstrapSourceOpening("", nil, now)
	if err != nil || !actual.IsZero() {
		t.Fatal("historical immediate bootstrap changed")
	}
}

// Recovery may occur after the date, but must never reset, move or add a pin.
func TestBootstrapSourceOpeningRecoveryPreservesExactSchedule(t *testing.T) {
	opensAt := time.Date(2035, 4, 3, 0, 0, 0, 0, time.UTC)
	intent := &model.CompetitionSourceActivation{OpensAt: opensAt}
	for _, value := range []string{"", opensAt.Format(time.RFC3339), "2035-04-03T02:00:00+02:00"} {
		actual, err := bootstrapSourceOpening(value, intent, opensAt.Add(time.Hour))
		if err != nil || !actual.Equal(opensAt) {
			t.Fatalf("recovery changed durable opening: %q %v %v", value, actual, err)
		}
	}
	if _, err := bootstrapSourceOpening(opensAt.Add(time.Second).Format(time.RFC3339), intent, opensAt.Add(time.Hour)); err == nil {
		t.Fatal("recovery moved the announced opening")
	}
	if _, err := bootstrapSourceOpening(opensAt.Format(time.RFC3339), &model.CompetitionSourceActivation{}, opensAt.Add(-time.Hour)); err == nil {
		t.Fatal("recovery retroactively pinned a historical immediate bootstrap")
	}
}
