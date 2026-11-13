// Fake durable state and an explicit clock exercise full-window lifecycle
// ordering. These tests grant no runtime readiness or operational authority.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type competitionIntakeFirstSupervisorTestDriver struct {
	*competitionSupervisorFixture
	apiReady      bool
	runtimeReady  bool
	baselineReady bool
	humanApproved bool
	apiCalls      int
	gradeCalls    int
	controls      int
	queued        *queuedJob
}

func (self *competitionIntakeFirstSupervisorTestDriver) IntakeFirst(*model.CompetitionSourceActivation) bool {
	return true
}

// The fake independently represents API evidence, not the grading adapter.
func (self *competitionIntakeFirstSupervisorTestDriver) PrepareIntake(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) error {
	self.apiCalls++
	if !self.apiReady {
		return ErrReviewNotReady
	}
	return nil
}

// A committed round remains unchanged when the caller loses its checkpoint.
func (self *competitionIntakeFirstSupervisorTestDriver) OpenIntake(_ context.Context, _ model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation) (*model.CompetitionRoundResult, error) {
	if self.snapshot.Round != nil && self.snapshot.Round.RoundId == intent.TargetRoundId {
		return self.snapshot.Round, nil
	}
	if !self.apiReady || !self.humanApproved {
		return nil, ErrReviewNotReady
	}
	self.creations++
	round := &model.CompetitionRoundResult{RoundId: intent.TargetRoundId, Epoch: intent.TargetEpoch, Status: "open", DecisionMode: model.CompetitionDecisionModeV2,
		OpensAt: self.now, ClosesAt: self.now.Add(7 * 24 * time.Hour)}
	self.snapshot.Round, self.snapshot.Seal = round, nil
	self.queued = &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: round.RoundId, State: "queued"}}
	return round, nil
}

func (self *competitionIntakeFirstSupervisorTestDriver) PrepareRoundGrading(context.Context, model.CompetitionSupervisorLease, model.CompetitionRoundResult) error {
	self.gradeCalls++
	if !self.runtimeReady {
		return errCompetitionIntakeFirstPending
	}
	return nil
}

// Trusted control preparation is separate from the first real candidate claim.
func (self *competitionIntakeFirstSupervisorTestDriver) RunWorker(context.Context, model.CompetitionSupervisorLease, model.CompetitionRoundResult) error {
	if !self.baselineReady {
		self.controls++
		return nil
	}
	if self.queued.StartedAt == nil {
		now := self.now
		self.queued.StartedAt, self.queued.State = &now, "running"
		self.queued.AttemptCount++
	}
	return nil
}

// An explicit operator step supplements, and cannot be inferred from, a source
// choice or successful independent provider review in the immutable intent.
func (self *competitionIntakeFirstSupervisorTestDriver) PublishSource(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) error {
	if !self.humanApproved {
		return errCompetitionSourceOperatorHandoff
	}
	self.publications++
	return nil
}

func newCompetitionIntakeFirstSupervisorTest() (*competitionIntakeFirstSupervisorTestDriver, *CompetitionSupervisor) {
	base := newCompetitionSupervisorFixture()
	base.now = base.now.Add(72 * time.Hour)
	intent := &model.CompetitionSourceActivation{Generation: 2, ExpectedGeneration: 1, TargetEpoch: 2, TargetRoundId: server.NewId(), PreparedAt: base.now.Add(-48 * time.Hour),
		State: "prepared", Kind: "reviewed_source_carry_forward", Reason: "no_reward_winner"}
	base.snapshot.Intent = intent
	base.snapshot.Transition = &model.CompetitionSupervisorTransition{TargetEpoch: 2, Revision: 1, State: "preparation_pending", SourceActivationGeneration: 2, PreparationStartedAt: intent.PreparedAt}
	driver := &competitionIntakeFirstSupervisorTestDriver{competitionSupervisorFixture: base, apiReady: true, humanApproved: true}
	supervisor := &CompetitionSupervisor{Enabled: true, OwnerId: "synthetic-supervisor", Driver: driver, Now: func() time.Time { return driver.now }}
	return driver, supervisor
}

// Full intake starts after delayed drain/build even though grading is not
// ready. The synthetic queued job remains clockless until the actual claim.
func TestCompetitionIntakeFirstSupervisorOpensBeforeBaselineWithoutClock(t *testing.T) {
	driver, supervisor := newCompetitionIntakeFirstSupervisorTest()
	opening := driver.now
	state, err := supervisor.Reconcile(context.Background())
	if err != nil || state != "round_created" || driver.creations != 1 || driver.activations != 0 || driver.preparations != 0 || driver.gradeCalls != 0 || driver.apiCalls != 1 {
		t.Fatal("intake waited for or fabricated grading activation", state, err, driver)
	}
	round := *driver.snapshot.Round
	if !round.OpensAt.Equal(opening) || round.ClosesAt.Sub(round.OpensAt) != 7*24*time.Hour || driver.queued.StartedAt != nil || driver.queued.AttemptCount != 0 {
		t.Fatal("intake lost its actual full week or started a queued execution clock")
	}
	driver.now = driver.now.Add(time.Hour)
	state, err = supervisor.Reconcile(context.Background())
	if state != "grading_preparation_pending" || !errors.Is(err, errCompetitionIntakeFirstPending) || driver.queued.StartedAt != nil || driver.controls != 0 {
		t.Fatal("absent genuine runtime readiness started evaluation", state, err)
	}
	driver.runtimeReady = true
	if _, err := supervisor.Reconcile(context.Background()); err != nil || driver.controls != 1 || driver.queued.StartedAt != nil || driver.queued.AttemptCount != 0 {
		t.Fatal("trusted baseline preparation started a candidate clock", err)
	}
	driver.baselineReady = true
	claimedAt := driver.now
	if _, err := supervisor.Reconcile(context.Background()); err != nil || driver.queued.StartedAt == nil || !driver.queued.StartedAt.Equal(claimedAt) || driver.queued.AttemptCount != 1 {
		t.Fatal("first real claim did not own the execution clock", err)
	}
	driver.now = driver.now.Add(2 * time.Hour)
	if _, err := supervisor.Reconcile(context.Background()); err != nil || !driver.queued.StartedAt.Equal(claimedAt) || driver.queued.AttemptCount != 1 || !driver.snapshot.Round.ClosesAt.Equal(round.ClosesAt) {
		t.Fatal("resume extended the public window or candidate clock", err)
	}
}

// Lack of API evidence blocks opening without falling through to the grading
// adapter. Preparation-only can collect evidence but cannot activate/open.
func TestCompetitionIntakeFirstSupervisorRequiresActualApiAndExplicitOpening(t *testing.T) {
	driver, supervisor := newCompetitionIntakeFirstSupervisorTest()
	driver.apiReady = false
	state, err := supervisor.Reconcile(context.Background())
	if state != "preparation_blocked" || !errors.Is(err, ErrReviewNotReady) || driver.creations != 0 || driver.activations != 0 || driver.preparations != 0 {
		t.Fatal("missing API readiness opened or graded a source", state, err)
	}
	driver.apiReady, supervisor.PreparationOnly = true, true
	state, err = supervisor.Reconcile(context.Background())
	if err != nil || state != "preparation_ready" || driver.creations != 0 || driver.activations != 0 {
		t.Fatal("preparation-only crossed the opening barrier", state, err)
	}
	supervisor.PreparationOnly = false
	state, err = supervisor.Reconcile(context.Background())
	if err != nil || state != "round_created" || driver.creations != 1 || driver.activations != 0 {
		t.Fatal("explicit API-ready intake could not open independently", state, err)
	}
}

// Human disposition is durable and precedes publication; a provider/build
// success alone cannot run the external source publication hook.
func TestCompetitionIntakeFirstSupervisorStopsBeforeHumanHandoff(t *testing.T) {
	driver, supervisor := newCompetitionIntakeFirstSupervisorTest()
	driver.humanApproved = false
	driver.snapshot.Transition.State = "source_publication_pending"
	state, err := supervisor.Reconcile(context.Background())
	if state != "publication_blocked" || !errors.Is(err, errCompetitionSourceOperatorHandoff) || driver.publications != 0 || driver.creations != 0 || driver.apiCalls != 0 {
		t.Fatal("provider completion was mistaken for human publication authority", state, err)
	}
	driver.humanApproved = true
	state, err = supervisor.Reconcile(context.Background())
	if err != nil || state != "preparation_pending" || driver.publications != 1 || driver.creations != 0 {
		t.Fatal("exact explicit human handoff did not release publication", state, err)
	}
}

// Lost checkpoint acknowledgment and a restarted reconciler cannot choose new
// opening dates or regenerate the already committed target/workload identity.
func TestCompetitionIntakeFirstSupervisorReplayRetainsOpeningAfterCheckpointLoss(t *testing.T) {
	driver, supervisor := newCompetitionIntakeFirstSupervisorTest()
	driver.failCreatedCheckpoint = true
	state, err := supervisor.Reconcile(context.Background())
	if err == nil || state != "checkpoint_blocked" || driver.creations != 1 {
		t.Fatal("synthetic checkpoint failure did not follow actual opening", state, err)
	}
	before := *driver.snapshot.Round
	driver.failCreatedCheckpoint = false
	driver.now = driver.now.Add(24 * time.Hour)
	supervisor = &CompetitionSupervisor{Enabled: true, OwnerId: supervisor.OwnerId, Driver: driver, Now: func() time.Time { return driver.now }}
	_, _ = supervisor.Reconcile(context.Background())
	if driver.creations != 1 || driver.snapshot.Round.RoundId != before.RoundId || !driver.snapshot.Round.OpensAt.Equal(before.OpensAt) || !driver.snapshot.Round.ClosesAt.Equal(before.ClosesAt) {
		t.Fatal("restart regenerated or extended committed intake")
	}
}
