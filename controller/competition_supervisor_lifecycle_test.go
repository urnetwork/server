// A deterministic season runs the real reconciler with explicit durable-state
// boundaries. No sleep, noisy evaluator or discretionary review drives time.
package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type competitionSupervisorSeasonFixture struct {
	*competitionSupervisorFixture
	pending bool
	seals   map[int]*model.CompetitionEvaluationSeal
}

// A completed epoch has a different successor checkpoint, just as the durable
// driver queries by target epoch rather than replaying the last round's phase.
func (self *competitionSupervisorSeasonFixture) Snapshot(ctx context.Context, lease model.CompetitionSupervisorLease) (*CompetitionSupervisorSnapshot, error) {
	if self.snapshot.Transition != nil && self.snapshot.Transition.TargetEpoch != self.snapshot.Round.Epoch+1 {
		self.snapshot.Transition, self.snapshot.Intent = nil, nil
	}
	return self.competitionSupervisorFixture.Snapshot(ctx, lease)
}

// A close does not seal an epoch while even one already-admitted job remains.
func (self *competitionSupervisorSeasonFixture) Seal(_ context.Context, _ model.CompetitionSupervisorLease, roundId server.Id) error {
	if self.pending {
		return ErrReviewNotReady
	}
	epoch := self.snapshot.Round.Epoch
	self.snapshot.Seal = &model.CompetitionEvaluationSeal{RoundId: roundId, Epoch: epoch, SealedAt: self.now,
		SourceChoiceAt: self.now, PublicationAt: self.now, BaselineSha256: fmt.Sprintf("%064x", epoch)}
	self.seals[epoch] = self.snapshot.Seal
	return nil
}

// There is deliberately no review completion. Every successor retains the
// exact approved source and margin, not a provisional recipient's patch.
func (self *competitionSupervisorSeasonFixture) ChooseSource(_ context.Context, _ model.CompetitionSupervisorLease, _ *CompetitionSupervisorSnapshot, transition model.CompetitionSupervisorTransition) (*model.CompetitionSourceActivation, error) {
	self.choices++
	current := self.snapshot.CurrentSource
	self.snapshot.Intent = &model.CompetitionSourceActivation{Generation: current.Generation + 1, ExpectedGeneration: current.Generation,
		SourceGeneration: current.SourceGeneration, SourceLockSha256: current.SourceLockSha256, Commits: current.Commits,
		TargetEpoch: transition.TargetEpoch, Kind: "reviewed_source_carry_forward", Reason: "review_pending",
		Threshold: current.Threshold, PreparedAt: self.now, State: "prepared"}
	return self.snapshot.Intent, nil
}

// All six epochs close, drain, seal and advance without a synchronous review;
// readiness still consumes its interval and no seventh epoch can be created.
func TestCompetitionSupervisorCompleteSixEpochLifecycle(t *testing.T) {
	base := newCompetitionSupervisorFixture()
	base.snapshot.Round.Status, base.snapshot.Round.OpensAt = "open", base.now
	base.snapshot.Round.ClosesAt = base.now.Add(7 * 24 * time.Hour)
	base.snapshot.Seal = nil
	base.snapshot.CurrentSource.SourceLockSha256 = fmt.Sprintf("%064x", 77)
	base.snapshot.CurrentSource.Commits = map[string]string{"server": fmt.Sprintf("%040x", 88)}
	fixture := &competitionSupervisorSeasonFixture{competitionSupervisorFixture: base, seals: map[int]*model.CompetitionEvaluationSeal{}}
	step := func() (string, error) {
		// Reconstruct after every step: progress cannot live only in a process.
		supervisor := CompetitionSupervisor{Enabled: true, OwnerId: "synthetic-supervisor", Driver: fixture, Now: func() time.Time { return fixture.now }}
		return supervisor.Reconcile(context.Background())
	}
	baselineIds := map[string]bool{}
	for epoch := 1; epoch <= 6; epoch++ {
		round := fixture.snapshot.Round
		if round.Epoch != epoch || round.ClosesAt.Sub(round.OpensAt) != 7*24*time.Hour {
			t.Fatalf("epoch %d did not receive a full admission window: %+v", epoch, round)
		}
		fixture.now, fixture.pending = round.ClosesAt, true
		if state, err := step(); state != "draining" || err == nil || fixture.snapshot.Seal != nil {
			t.Fatalf("epoch %d advanced with outstanding work: %s, %v", epoch, state, err)
		}
		fixture.pending = false
		if state, err := step(); state != "sealed" || err != nil {
			t.Fatalf("epoch %d did not seal after drain: %s, %v", epoch, state, err)
		}
		baseline := fixture.snapshot.Seal.BaselineSha256
		if baselineIds[baseline] {
			t.Fatal("carried source reused a prior round's control identity")
		}
		baselineIds[baseline] = true
		if epoch == 6 {
			for range 2 {
				if state, err := step(); state != "season_complete" || err != nil {
					t.Fatalf("sixth epoch did not terminate progression: %s, %v", state, err)
				}
			}
			break
		}
		created := false
		for range 10 {
			state, err := step()
			if err != nil {
				t.Fatalf("epoch %d progression failed at %s: %v", epoch, state, err)
			}
			if state == "preparation_ready" {
				before := fixture.activations
				if again, err := step(); again != "preparation_ready" || err != nil || fixture.activations != before {
					t.Fatalf("preparation interval was bypassed: %s, %v", again, err)
				}
				fixture.now = fixture.snapshot.Intent.PreparedAt.Add(competitionPreparationInterval)
			}
			if state == "round_created" {
				created = true
				break
			}
		}
		if !created || fixture.snapshot.CurrentSource.SourceGeneration != 0 || fixture.snapshot.CurrentSource.Threshold != 0.161 ||
			fixture.snapshot.CurrentSource.SourceLockSha256 != fmt.Sprintf("%064x", 77) {
			t.Fatalf("epoch %d failed safe reviewed carry-forward: %+v", epoch, fixture.snapshot.CurrentSource)
		}
	}
	if fixture.creations != 5 || fixture.choices != 5 || fixture.activations != 5 || fixture.preparations != 5 || len(fixture.seals) != 6 {
		t.Fatalf("season effect counts: creates=%d choices=%d activations=%d preparations=%d seals=%d",
			fixture.creations, fixture.choices, fixture.activations, fixture.preparations, len(fixture.seals))
	}
}
