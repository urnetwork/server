// Synthetic API projections and owned database transitions keep intake separate
// from held grading without supplying real calibration or readiness evidence.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Existing API fakes explicitly model the new optional durable-read capability.
func (self *fakeStore) AutomationHoldStatus(context.Context, *Settings) (competitionAutomationHoldStatus, error) {
	return self.holdStatus, self.holdStatusErr
}

// The normal API readiness path stays green only for actual intake checks;
// evaluation and publication remain visibly held without blocking admission.
func TestCompetitionIntakeHeldGradingPreservesAdmissionAndReportsStatus(t *testing.T) {
	_, store, service := newAutomationStagingApiFixture(t)
	store.fakeStore.holdStatus = competitionAutomationHoldStatus{Evaluation: true, RewardDelivery: true}
	ready, evalError := service.Ready(context.Background())
	if evalError != nil || !ready.Ready || ready.RuntimeStatus == nil || !ready.RuntimeStatus.IntakeReady || ready.RuntimeStatus.GradingReady || !ready.RuntimeStatus.PublicationHeld {
		t.Fatalf("intake/held grading readiness = %+v %v", ready, evalError)
	}
	info, evalError := service.Info(context.Background())
	if evalError != nil || info.RuntimeStatus == nil || !info.RuntimeStatus.IntakeReady || info.RuntimeStatus.GradingReady || !info.RuntimeStatus.PublicationHeld {
		t.Fatalf("intake discovery = %+v %v", info, evalError)
	}
	accepted, status, evalError := service.SubmitV2(context.Background(), ScoreArgs{RoundId: store.stagingRound.RoundId, Patch: testPatch("synthetic-held-intake")}, &Principal{Id: "synthetic-submitter", Role: "submitter"})
	if evalError != nil || status != http.StatusAccepted || accepted.State != "queued" || store.enqueueCalls != 1 {
		t.Fatalf("held grading blocked intake: %+v %d %v", accepted, status, evalError)
	}
}

// Hold-read failure is not interpreted as an unheld or intake-ready season.
func TestCompetitionIntakeStatusFailsClosedOnHoldReadFailure(t *testing.T) {
	_, store, service := newAutomationStagingApiFixture(t)
	store.fakeStore.holdStatusErr = errors.New("synthetic hold read failure")
	ready, evalError := service.Ready(context.Background())
	if evalError == nil || ready.Ready || ready.RuntimeStatus != nil || ready.Checks["automation_hold_status"] {
		t.Fatalf("failed hold read advertised readiness: %+v %v", ready, evalError)
	}
	if info, evalError := service.Info(context.Background()); evalError == nil || info != nil {
		t.Fatal("failed hold read returned intake discovery")
	}
}

// Neither intact source metadata nor a grading hold can waive real containment.
func TestCompetitionIntakeStatusPreservesInfrastructureFailure(t *testing.T) {
	_, store, service := newAutomationStagingApiFixture(t)
	store.fakeStore.holdStatus.Evaluation = true
	store.fakeStore.readiness = map[string]bool{"authoritative_evaluator_host": false}
	ready, evalError := service.Ready(context.Background())
	if evalError == nil || ready.Ready || ready.RuntimeStatus == nil || ready.RuntimeStatus.IntakeReady || ready.RuntimeStatus.GradingReady || !ready.RuntimeStatus.PublicationHeld {
		t.Fatalf("negative physical facts were promoted: %+v %v", ready, evalError)
	}
}

// Mutable operational state must never enter canonical normative approval bytes.
func TestCompetitionIntakeStatusDoesNotChangeCanonicalPolicy(t *testing.T) {
	settings := automationApiSettings()
	content, err := json.Marshal(settings.PublicInfo())
	if err != nil || strings.Contains(string(content), "runtime_status") {
		t.Fatalf("mutable hold status entered normative policy: %s %v", content, err)
	}
}

// Even a retained previously completed score remains hidden while grading is held.
func TestCompetitionIntakeHeldGradingDoesNotPublishScores(t *testing.T) {
	_, store, service := newAutomationStagingApiFixture(t)
	store.fakeStore.holdStatus.Evaluation = true
	job := *store.enqueueJob
	rawScore := float64(123)
	job.State, job.Score = "succeeded", &ScoreResult{RawScore: &rawScore}
	publicationAt := server.NowUtc()
	job.Round.AutomationPublicationAt = &publicationAt
	store.fakeStore.getJob = &job
	result, status, evalError := service.GetScoreV2(context.Background(), job.JobId, &Principal{Id: "synthetic-submitter", Role: "submitter"})
	if evalError != nil || status != http.StatusOK || result.Score != nil || result.State != "completed" {
		t.Fatalf("held grading exposed a score: %+v %d %v", result, status, evalError)
	}
}

// Current-season holds cannot reinterpret an already published historical era.
type competitionIntakeScopedHoldStore struct {
	*automationStagingApiStore
	currentSeason, observedSeason string
}

// Reads record the actual scope used by the public historical-result consumer.
func (self *competitionIntakeScopedHoldStore) AutomationHoldStatus(_ context.Context, settings *Settings) (competitionAutomationHoldStatus, error) {
	self.observedSeason = settings.AutomationPolicy.SeasonId
	return competitionAutomationHoldStatus{Evaluation: self.observedSeason == self.currentSeason}, nil
}

// Historical contracts authenticate their original frozen scope rather than
// inheriting the live season's hold. Existing source/auth guards run unchanged.
func TestCompetitionIntakeHistoricalScoreUsesFrozenHoldScope(t *testing.T) {
	settings, source, _ := newAutomationStagingApiFixture(t)
	store := &competitionIntakeScopedHoldStore{automationStagingApiStore: source, currentSeason: settings.AutomationPolicy.SeasonId}
	job := *source.enqueueJob
	rawScore := float64(123)
	job.State, job.Score = "succeeded", &ScoreResult{RawScore: &rawScore}
	job.Round.CompetitionId = settings.CompetitionId
	policy, err := decodeRoundPolicySnapshot(job.Round.PolicyJson)
	if err != nil {
		t.Fatal(err)
	}
	policy.AutomationPolicy.SeasonId = "synthetic-prior-season"
	job.Round.PolicyJson, err = json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	publicationAt := server.NowUtc().Add(-time.Second)
	job.Round.AutomationPublicationAt = &publicationAt
	store.fakeStore.getJob = &job
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	result, status, evalError := service.GetScoreV2(context.Background(), job.JobId, &Principal{Id: "synthetic-submitter", Role: "submitter"})
	if evalError != nil || status != http.StatusOK || result.Score == nil || store.observedSeason != policy.AutomationPolicy.SeasonId {
		t.Fatalf("historical score inherited the current season hold: %+v %d %v scope=%s", result, status, evalError, store.observedSeason)
	}
}

// An actual durable evaluation hold blocks all new claim/seal work while
// retaining the original queued job and admission clock. No score is created.
func TestCompetitionIntakeDurableHoldPreservesFifoAndPreventsClaimAndSeal(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		args := model.CompetitionAutomationHoldArgs{Kind: "evaluation", Active: true, IdempotencyKey: "synthetic-intake-hold", ReasonCode: "OPERATOR_HOLD", PublicExplanation: "Synthetic grading gate remains pending.", EvidenceSha256: strings.Repeat("a", 64)}
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args); err != nil {
			t.Fatal(err)
		}
		patch, patchError := ValidateAndCanonicalizePatch(testPatch("synthetic-fifo-held"), fixture.settings.PatchPolicy)
		if patchError != nil {
			t.Fatal(patchError)
		}
		job, hit, err := fixture.store.Enqueue(ctx, fixture.settings, fixture.round.RoundId, patch, "synthetic-submitter", testApiImageDigest())
		if err != nil || hit || job.State != "queued" || job.StartedAt != nil || job.AttemptCount != 0 {
			t.Fatalf("held grading changed intake: %+v %t %v", job, hit, err)
		}
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-held-supervisor", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		fixture.settings.automationLease, fixture.settings.automationRoundId = lease, fixture.round.RoundId
		if claimed, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-worker", testWorkerImageDigest()); !errors.Is(err, ErrAutomationHeld) || claimed != nil {
			t.Fatalf("held job was claimed: %+v %v", claimed, err)
		}
		fixture.settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
		if reserved, err := fixture.store.ReserveRoundBaseline(ctx, fixture.settings, "synthetic-worker", testWorkerImageDigest()); !errors.Is(err, ErrAutomationHeld) || reserved != nil {
			t.Fatalf("held grading started baseline work: %+v %v", reserved, err)
		}
		fixture.now = fixture.round.RevealAt
		if sealed, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId); !errors.Is(err, ErrAutomationHeld) || sealed != nil {
			t.Fatalf("held evaluation was sealed: %+v %v", sealed, err)
		}
		saved, err := fixture.store.GetJob(ctx, fixture.settings, job.JobId, &Principal{Id: "synthetic-submitter", Role: "submitter"})
		if err != nil || saved.State != "queued" || saved.StartedAt != nil || saved.Score != nil || !saved.SubmittedAt.Equal(job.SubmittedAt) {
			t.Fatalf("hold rewrote queued identity or manufactured a score: %+v %v", saved, err)
		}
	})
}

// A newly applied grading hold also blocks an existing seal's public result
// and replay path; it cannot be bypassed through idempotent finalization.
func TestCompetitionIntakeHoldBlocksExistingWinnerPublication(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		addCompetitionAutomationJob(t, fixture, 70)
		fixture.now = fixture.round.RevealAt
		if _, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId); err != nil {
			t.Fatal(err)
		}
		args := model.CompetitionAutomationHoldArgs{Kind: "evaluation", Active: true, IdempotencyKey: "synthetic-publication-hold", ReasonCode: "OPERATOR_HOLD", PublicExplanation: "Synthetic publication gate.", EvidenceSha256: strings.Repeat("b", 64)}
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args); err != nil {
			t.Fatal(err)
		}
		if result, err := fixture.store.AutomationResult(ctx, fixture.settings, fixture.round.RoundId); !errors.Is(err, ErrAutomationHeld) || result != nil {
			t.Fatalf("hold exposed a previous winner: %+v %v", result, err)
		}
		if result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId); !errors.Is(err, ErrAutomationHeld) || result != nil {
			t.Fatalf("seal replay bypassed grading hold: %+v %v", result, err)
		}
	})
}

// Expected-revision CAS prevents an old authorized release from clearing a newer
// hold. Replayed POST responses are historical receipts, not latest availability.
func TestCompetitionIntakeReleaseCannotClearNewerHold(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		args := model.CompetitionAutomationHoldArgs{Kind: "evaluation", Active: true, IdempotencyKey: "synthetic-first-hold", ReasonCode: "OPERATOR_HOLD", PublicExplanation: "Synthetic original gate.", EvidenceSha256: strings.Repeat("a", 64)}
		first, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args)
		if err != nil {
			t.Fatal(err)
		}
		args.ExpectedRevision, args.IdempotencyKey = first.Revision, "synthetic-newer-hold"
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args); err != nil {
			t.Fatal(err)
		}
		args.Active, args.ReasonCode, args.IdempotencyKey = false, "RECOVERY_APPROVED", "synthetic-stale-release"
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale release cleared newer gate: %v", err)
		}
		holds, err := fixture.store.AutomationHoldStatus(ctx, fixture.settings)
		if err != nil || !holds.Evaluation {
			t.Fatal("latest hold was not retained", holds, err)
		}
		args.ExpectedRevision = first.Revision + 1
		args.IdempotencyKey = "synthetic-current-release"
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args); err != nil {
			t.Fatal(err)
		}
		holds, err = fixture.store.AutomationHoldStatus(ctx, fixture.settings)
		if err != nil || holds.Evaluation {
			t.Fatal("explicit current authorized release failed", holds, err)
		}
	})
}

// Each idle boundary reports through channels; no scheduler timing or live
// service is used to prove that a paused worker remains a truthful host owner.
type competitionIntakeHeldWorkerStore struct {
	*automationStagingApiStore
	sealHeld   bool
	observed   chan struct{}
	registered chan struct{}
	claims     int
}

// Both the seal pause and the claim pause remain exact native sentinel errors.
func (self *competitionIntakeHeldWorkerStore) SealAndElect(context.Context, *Settings, server.Id) (*model.CompetitionV2RoundResult, error) {
	if self.sealHeld {
		self.observed <- struct{}{}
		return nil, ErrAutomationHeld
	}
	return nil, ErrReviewNotReady
}

// A held claim never hands an evaluation job or execution clock to the worker.
func (self *competitionIntakeHeldWorkerStore) Claim(context.Context, *Settings, string, string) (*queuedJob, error) {
	self.claims++
	self.observed <- struct{}{}
	return nil, ErrAutomationHeld
}

// A positive registration barrier proves preflight and later idle heartbeats.
func (self *competitionIntakeHeldWorkerStore) RegisterHost(context.Context, *Settings, HostSelfCheck) error {
	self.registered <- struct{}{}
	return nil
}

// Injected ticker edges prove real self-check/registration continues during
// either held boundary and that cancellation joins without evaluating a job.
func TestCompetitionIntakeHeldWorkerKeepsHostHeartbeatAndCancels(t *testing.T) {
	for _, sealHeld := range []bool{false, true} {
		settings, source, _ := newAutomationStagingApiFixture(t)
		store := &competitionIntakeHeldWorkerStore{automationStagingApiStore: source, sealHeld: sealHeld,
			observed: make(chan struct{}, 4), registered: make(chan struct{})}
		evaluator := &fakeEvaluator{check: passingHostCheck(settings)}
		worker, err := newWorkerWithImageDigest(settings, store, evaluator, "synthetic-held-worker", testWorkerImageDigest())
		if err != nil {
			t.Fatal(err)
		}
		poll, heartbeat := make(chan time.Time), make(chan time.Time)
		worker.pollTickForTest, worker.hostTickForTest = poll, heartbeat
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- worker.Run(ctx) }()
		select {
		case <-store.registered:
		case err := <-done:
			cancel()
			t.Fatal("held worker failed preflight", err)
		}
		select {
		case <-store.observed:
		case err := <-done:
			cancel()
			t.Fatal("held worker exited before its idle boundary", err)
		}
		select {
		case heartbeat <- time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC):
		case err := <-done:
			cancel()
			t.Fatal("grading hold killed the authoritative host heartbeat", err)
		}
		select {
		case <-store.registered:
		case err := <-done:
			cancel()
			t.Fatal("held worker did not refresh host registration", err)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("held worker failed cancellation join", err)
		}
		if evaluator.calls != 0 || (sealHeld && store.claims != 0) || (!sealHeld && store.claims == 0) {
			t.Fatal("held idle changed claim/evaluation ownership", sealHeld, store.claims, evaluator.calls)
		}
	}
}
