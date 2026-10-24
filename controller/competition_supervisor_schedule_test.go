// The announced admission window survives process restarts and late reconciliation.
package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// Before or after the scheduled instant, source_active must use the durable pin
// instead of the supervisor's clock. A lost checkpoint cannot create a new date.
func TestCompetitionSupervisorPreservesPinnedOpeningAcrossRestart(t *testing.T) {
	for _, delay := range []time.Duration{-time.Hour, time.Hour} {
		fixture := newCompetitionSupervisorFixture()
		opensAt := fixture.now.Add(48 * time.Hour)
		fixture.now = opensAt.Add(delay)
		fixture.snapshot.Round, fixture.snapshot.Seal = nil, nil
		fixture.snapshot.Intent = &model.CompetitionSourceActivation{Generation: 1, TargetEpoch: 1, OpensAt: opensAt}
		fixture.snapshot.Transition = &model.CompetitionSupervisorTransition{TargetEpoch: 1, State: "source_active", Revision: 1}
		fixture.failCreatedCheckpoint = true
		supervisor := &CompetitionSupervisor{Enabled: true, OwnerId: "synthetic-schedule-owner", Driver: fixture, Now: func() time.Time { return fixture.now }}
		state, err := supervisor.Reconcile(context.Background())
		if err == nil || state != "checkpoint_blocked" || fixture.creations != 1 || fixture.snapshot.Round == nil {
			t.Fatalf("lost checkpoint fixture: %s %v", state, err)
		}
		if !fixture.snapshot.Round.OpensAt.Equal(opensAt) || !fixture.snapshot.Round.ClosesAt.Equal(opensAt.Add(7*24*time.Hour)) {
			t.Fatal("supervisor replaced the durable opening with its clock")
		}
		supervisor = &CompetitionSupervisor{Enabled: true, OwnerId: "synthetic-schedule-owner", Driver: fixture, Now: func() time.Time { return fixture.now }}
		if _, err := supervisor.Reconcile(context.Background()); err != nil || fixture.creations != 1 || !fixture.snapshot.Round.OpensAt.Equal(opensAt) {
			t.Fatalf("restart replaced an already committed schedule: %v", err)
		}
	}
}
