// Preparation-only reconciliation preserves genuine quorum checks while an
// explicit operator boundary prevents activation, round creation and evaluation.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Existing checkpoint behavior is reused; independent receipt validation and
// forbidden downstream calls remain observable without a database or clock wait.
type competitionPreparationOnlyFixture struct {
	*competitionSupervisorFixture
	receipts     []model.CompetitionSourceReadiness
	acquisitions int
	workerCalls  int
	sealCalls    int
}

// Deterministic synthetic identities put the elected policy's zero preparation
// interval directly at the old same-reconciliation activation boundary.
func newCompetitionPreparationOnlyFixture() (*competitionPreparationOnlyFixture, *CompetitionSupervisor) {
	now := time.Date(2035, 4, 1, 0, 0, 0, 0, time.UTC)
	intent := &model.CompetitionSourceActivation{Environment: "synthetic", CompetitionId: "synthetic-competition", SeasonId: "synthetic-season",
		Generation: 1, ExpectedGeneration: 0, SourceGeneration: 0, TargetEpoch: 1, TargetRoundId: server.Id{1}, Kind: "baseline", State: "prepared",
		WinnerReviewPolicy: model.CompetitionSnowWinnerReviewPolicy, PreparedAt: now, OpensAt: now.Add(time.Hour),
		ManifestCommit: strings.Repeat("a", 40), ManifestSha256: strings.Repeat("b", 64), SourceLockSha256: strings.Repeat("c", 64), HostQualificationSha256: strings.Repeat("d", 64)}
	fixture := &competitionPreparationOnlyFixture{competitionSupervisorFixture: &competitionSupervisorFixture{now: now,
		snapshot: CompetitionSupervisorSnapshot{Intent: intent, Transition: &model.CompetitionSupervisorTransition{TargetEpoch: 1, Revision: 1,
			State: "preparation_pending", SourceActivationGeneration: 1, PreparationStartedAt: now}}}}
	for _, role := range []string{"api", "worker"} {
		fixture.receipts = append(fixture.receipts, model.CompetitionSourceReadiness{Environment: intent.Environment, CompetitionId: intent.CompetitionId,
			SeasonId: intent.SeasonId, Generation: intent.Generation, TargetRoundId: intent.TargetRoundId, Role: role, ActorId: "synthetic-" + role,
			ManifestCommit: intent.ManifestCommit, ManifestSha256: intent.ManifestSha256, SourceLockSha256: intent.SourceLockSha256,
			QualificationSha256: intent.HostQualificationSha256, RuntimeImageDigest: "sha256:" + strings.Repeat("e", 64), Ready: true})
	}
	supervisor := &CompetitionSupervisor{Enabled: true, PreparationOnly: true, OwnerId: "synthetic-owner", Driver: fixture, Now: func() time.Time { return fixture.now }}
	return fixture, supervisor
}

// Lease acquisition is still native state-machine work, never bypassed by the
// preparation-only flag or a previously successful readiness checkpoint.
func (self *competitionPreparationOnlyFixture) Acquire(ctx context.Context, owner string, ttl time.Duration) (*model.CompetitionSupervisorLease, error) {
	self.acquisitions++
	return self.competitionSupervisorFixture.Acquire(ctx, owner, ttl)
}

// Quorum is checked by the same strict helper as the concrete store driver.
func (self *competitionPreparationOnlyFixture) PrepareSource(_ context.Context, _ model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation) error {
	self.preparations++
	if self.failPrepare {
		return errors.New("synthetic readiness failure")
	}
	return sourcePreparationReceiptsMatch(intent, self.receipts)
}

// Ordinary evaluation must never be mistaken for the one-replicate qualifier.
func (self *competitionPreparationOnlyFixture) RunWorker(context.Context, model.CompetitionSupervisorLease, model.CompetitionRoundResult) error {
	self.workerCalls++
	return nil
}

// A preparation-only call must not advance round closure either.
func (self *competitionPreparationOnlyFixture) Seal(context.Context, model.CompetitionSupervisorLease, server.Id) error {
	self.sealCalls++
	return nil
}

// The default control verifies unchanged activation-to-ordinary-worker behavior
// with the already reserved synthetic target, not a fresh random identifier.
func (self *competitionPreparationOnlyFixture) CreateRound(_ context.Context, _ model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation, opensAt time.Time) (*model.CompetitionRoundResult, error) {
	self.creations++
	round := &model.CompetitionRoundResult{RoundId: intent.TargetRoundId, Epoch: intent.TargetEpoch, Status: "open", DecisionMode: model.CompetitionDecisionModeV2,
		OpensAt: opensAt, ClosesAt: opensAt.Add(7 * 24 * time.Hour)}
	self.snapshot.Round, self.snapshot.Seal = round, nil
	return round, nil
}

// Both independently scoped receipts can become ready without activating source
// in the same reconciliation, even with the elected policy's zero interval.
func TestCompetitionSupervisorPreparationOnlyStopsAfterReadiness(t *testing.T) {
	fixture, supervisor := newCompetitionPreparationOnlyFixture()
	if state, err := supervisor.Reconcile(context.Background()); err != nil || state != "preparation_ready" {
		t.Fatalf("preparation failed: state=%s err=%v", state, err)
	}
	if fixture.preparations != 1 || fixture.snapshot.Transition.State != "preparation_ready" || fixture.snapshot.Transition.Revision != 2 ||
		fixture.activations != 0 || fixture.creations != 0 || fixture.workerCalls != 0 || fixture.snapshot.CurrentSource != nil || fixture.snapshot.Intent.State != "prepared" {
		t.Fatal("ready preparation crossed the activation or ordinary-worker boundary")
	}
}

// Restarting on a genuine ready checkpoint cannot activate after the interval;
// only a separate normal-mode invocation can cross the operator review boundary.
func TestCompetitionSupervisorPreparationOnlyReadyRestartCannotActivate(t *testing.T) {
	fixture, supervisor := newCompetitionPreparationOnlyFixture()
	fixture.snapshot.Transition.State = "preparation_ready"
	fixture.now = fixture.now.Add(48 * time.Hour)
	for range 2 {
		if state, err := supervisor.Reconcile(context.Background()); err != nil || state != "preparation_ready" {
			t.Fatalf("ready restart failed: state=%s err=%v", state, err)
		}
	}
	if fixture.preparations != 0 || fixture.activations != 0 || fixture.creations != 0 || fixture.workerCalls != 0 || fixture.snapshot.Intent.State != "prepared" {
		t.Fatal("restarted preparation-only control activated ready source")
	}
}

// Every unsealed ordinary round is left alone; neither worker nor sealing runs.
func TestCompetitionSupervisorPreparationOnlyNeverRunsOrdinaryWorker(t *testing.T) {
	for _, status := range []string{"scheduled", "open", "draining", "closed"} {
		fixture, supervisor := newCompetitionPreparationOnlyFixture()
		fixture.snapshot.Round = &model.CompetitionRoundResult{RoundId: server.Id{2}, Epoch: 1, Status: status,
			DecisionMode: model.CompetitionDecisionModeV2, ClosesAt: fixture.now.Add(-time.Hour)}
		if state, err := supervisor.Reconcile(context.Background()); err != nil || state != "preparation_only" {
			t.Fatalf("%s: ordinary round refusal failed: state=%s err=%v", status, state, err)
		}
		if fixture.workerCalls != 0 || fixture.sealCalls != 0 || fixture.preparations != 0 || fixture.activations != 0 || fixture.creations != 0 {
			t.Fatalf("%s: preparation-only control advanced ordinary work", status)
		}
	}
}

// Existing active source and round-creation checkpoints are not permission to
// create a public round while only host preparation was requested.
func TestCompetitionSupervisorPreparationOnlyCurrentSourceCannotCreateRound(t *testing.T) {
	for _, phase := range []string{"source_active", "round_created"} {
		fixture, supervisor := newCompetitionPreparationOnlyFixture()
		fixture.snapshot.Intent.State = "active"
		fixture.snapshot.CurrentSource = fixture.snapshot.Intent
		fixture.snapshot.Transition.State = phase
		if state, err := supervisor.Reconcile(context.Background()); err != nil || state != "preparation_only" {
			t.Fatalf("%s: active source refusal failed: state=%s err=%v", phase, state, err)
		}
		if fixture.preparations != 0 || fixture.activations != 0 || fixture.creations != 0 || fixture.workerCalls != 0 || fixture.snapshot.Round != nil {
			t.Fatalf("%s: active source escaped preparation-only mode", phase)
		}
	}
}

// Source election/review remains outside the narrow preparation interface;
// an absent intent cannot cause a hidden choice or new durable checkpoint.
func TestCompetitionSupervisorPreparationOnlyRefusesNewSourceDecision(t *testing.T) {
	for _, phase := range []string{"", "source_choice_pending", "winner_review_pending", "preparation_pending"} {
		fixture, supervisor := newCompetitionPreparationOnlyFixture()
		if phase == "" {
			fixture.snapshot.Transition = nil
		} else {
			fixture.snapshot.Transition.State = phase
		}
		if phase == "preparation_pending" {
			fixture.snapshot.Intent = nil
		}
		if state, err := supervisor.Reconcile(context.Background()); err == nil || state != "preparation_blocked" {
			t.Fatalf("%q: unreserved source accepted: state=%s err=%v", phase, state, err)
		}
		if fixture.choices != 0 || fixture.publications != 0 || fixture.preparations != 0 || fixture.activations != 0 || fixture.creations != 0 || fixture.workerCalls != 0 {
			t.Fatalf("%q: refused source choice produced side effects", phase)
		}
	}
}

// Missing, forged and unavailable role evidence stays blocked by the original
// quorum predicate; preparation-only is not a readiness bypass.
func TestCompetitionSupervisorPreparationOnlyPreservesReadinessFailure(t *testing.T) {
	for _, failure := range []string{"missing-api", "worker-not-ready", "wrong-target", "unavailable"} {
		fixture, supervisor := newCompetitionPreparationOnlyFixture()
		switch failure {
		case "missing-api":
			fixture.receipts = fixture.receipts[1:]
		case "worker-not-ready":
			fixture.receipts[1].Ready = false
		case "wrong-target":
			fixture.receipts[1].TargetRoundId = server.Id{3}
		case "unavailable":
			fixture.failPrepare = true
		}
		if state, err := supervisor.Reconcile(context.Background()); err == nil || state != "preparation_blocked" {
			t.Fatalf("%s: incomplete readiness accepted: state=%s err=%v", failure, state, err)
		}
		if fixture.preparations != 1 || fixture.activations != 0 || fixture.creations != 0 || fixture.workerCalls != 0 || fixture.snapshot.Intent.State != "prepared" {
			t.Fatalf("%s: readiness failure activated or abandoned source", failure)
		}
	}
}

// Omitting the additive flag keeps normal elected-source activation, scheduled
// round creation and the ordinary singleton worker path unchanged.
func TestCompetitionSupervisorPreparationOnlyDefaultRetainsNormalAutomation(t *testing.T) {
	fixture, preparation := newCompetitionPreparationOnlyFixture()
	supervisor := &CompetitionSupervisor{Enabled: true, OwnerId: preparation.OwnerId, Driver: fixture, Now: preparation.Now}
	for _, expected := range []string{"source_active", "round_created", "evaluating"} {
		if state, err := supervisor.Reconcile(context.Background()); err != nil || state != expected {
			t.Fatalf("default automation changed: state=%s expected=%s err=%v", state, expected, err)
		}
	}
	if fixture.preparations != 1 || fixture.activations != 1 || fixture.creations != 1 || fixture.workerCalls != 1 || fixture.snapshot.CurrentSource != fixture.snapshot.Intent {
		t.Fatal("default automation lost activation or ordinary-worker behavior")
	}
}

// The library also refuses a repeating preparation loop, independent of CLI
// parsing, before acquiring a lease or touching any durable state.
func TestCompetitionSupervisorPreparationOnlyRequiresSingleReconciliation(t *testing.T) {
	fixture, supervisor := newCompetitionPreparationOnlyFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := supervisor.Run(ctx, nil); err == nil || !strings.Contains(err.Error(), "one explicit reconciliation") {
		t.Fatalf("repeating preparation-only loop accepted: %v", err)
	}
	if fixture.acquisitions != 0 || fixture.preparations != 0 || fixture.activations != 0 || fixture.workerCalls != 0 {
		t.Fatal("rejected preparation-only loop acquired or executed work")
	}
}

// A new flag cannot bypass original canceled-round or legacy-mode refusals.
func TestCompetitionSupervisorPreparationOnlyPreservesIncidentAndLegacyGuards(t *testing.T) {
	for _, failure := range []string{"canceled", "legacy", "lease"} {
		fixture, supervisor := newCompetitionPreparationOnlyFixture()
		fixture.snapshot.Round = &model.CompetitionRoundResult{RoundId: server.Id{2}, Epoch: 1, Status: "open", DecisionMode: model.CompetitionDecisionModeV2}
		expected := "incident_hold"
		switch failure {
		case "canceled":
			fixture.snapshot.Round.Status = "canceled"
		case "legacy":
			fixture.snapshot.Round.DecisionMode = "legacy"
			expected = "legacy_mode"
		case "lease":
			fixture.lease = model.CompetitionSupervisorLease{OwnerId: "another-synthetic-owner", FencingToken: 1, LeaseExpiresAt: fixture.now.Add(time.Hour)}
			expected = "lease_unavailable"
		}
		if state, err := supervisor.Reconcile(context.Background()); err == nil || state != expected {
			t.Fatalf("%s: existing guard changed: state=%s err=%v", failure, state, err)
		}
		if fixture.preparations != 0 || fixture.activations != 0 || fixture.creations != 0 || fixture.workerCalls != 0 {
			t.Fatalf("%s: rejected control performed downstream work", failure)
		}
	}
}

// Publication of the exact pre-existing intent is allowed, but subsequent
// preparation still stops before source activation and public round creation.
func TestCompetitionSupervisorPreparationOnlyPublishesExistingIntent(t *testing.T) {
	fixture, supervisor := newCompetitionPreparationOnlyFixture()
	fixture.snapshot.Transition.State = "source_publication_pending"
	original := fixture.snapshot.Intent
	for _, expected := range []string{"preparation_pending", "preparation_ready"} {
		if state, err := supervisor.Reconcile(context.Background()); err != nil || state != expected {
			t.Fatalf("existing intent preparation failed: state=%s err=%v", state, err)
		}
	}
	if fixture.publications != 1 || fixture.preparations != 1 || fixture.choices != 0 || fixture.activations != 0 || fixture.creations != 0 || fixture.workerCalls != 0 || fixture.snapshot.Intent != original {
		t.Fatal("preparation-only publication changed source or activated it")
	}
}
