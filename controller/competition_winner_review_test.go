// Fake clocks and durable snapshots exercise elected-winner closing without
// a live database, model call, Git publication, or evaluator process.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type winnerClosingFixture struct {
	now                                                 time.Time
	snapshot                                            CompetitionSupervisorSnapshot
	reviews, choices, builds, opens, fallbacks          int
	rejectFirst, rejectAll, failReview, failPreparation bool
	failBuild                                           bool
}

// Only the production archive methods used by review evidence are supplied.
// Any unexpected storage capability stays unavailable, not a silent success.
type winnerReviewEvidenceStore struct {
	server.ProtectedBlobStore
	content []byte
	key     string
	corrupt bool
}

func (self *winnerReviewEvidenceStore) Prefix() string    { return "test-evidence" }
func (self *winnerReviewEvidenceStore) Authority() string { return "blob.example" }
func (self *winnerReviewEvidenceStore) Bucket() string    { return "test-competition" }
func (self *winnerReviewEvidenceStore) CheckProtection(context.Context) (*server.BlobProtection, error) {
	return &server.BlobProtection{Authority: self.Authority(), Bucket: self.Bucket(), ObjectLock: "COMPLIANCE", Versioning: true, ReplicationTargets: []string{"test-backup"}}, nil
}
func (self *winnerReviewEvidenceStore) PutRetained(_ context.Context, key, path, _ string, until time.Time) (*server.BlobRetention, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	self.content, self.key = content, key
	return &server.BlobRetention{Key: key, Size: int64(len(content)), VersionId: "test-version", Mode: "COMPLIANCE", RetainUntil: until}, nil
}
func (self *winnerReviewEvidenceStore) GetVersion(_ context.Context, key, version string) (io.ReadCloser, error) {
	if key != self.key || version != "test-version" {
		return nil, errors.New("wrong immutable version")
	}
	if self.corrupt {
		return io.NopCloser(strings.NewReader("corrupt evidence")), nil
	}
	return io.NopCloser(bytes.NewReader(self.content)), nil
}

// Approval dispatch cannot proceed with missing or unauthenticated private
// report evidence. The real retained version, not a mutable local file, binds it.
func TestWinnerClosingReviewEvidenceRequiresProtectedExactReadback(t *testing.T) {
	store := &winnerReviewEvidenceStore{}
	settings := &Settings{CompetitionId: "test-competition", AutomationPolicy: model.CompetitionAutomationPolicy{Environment: "test", SeasonId: "test-season"},
		RetainUntil: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), artifactArchive: &blobArtifactArchive{store: store}}
	report := []byte("Synthetic private honesty report.")
	digest := sha256.Sum256(report)
	sha := hex.EncodeToString(digest[:])
	round, job := server.NewId(), server.NewId()
	receipt, err := RetainCompetitionWinnerReviewEvidence(context.Background(), settings, round, job, report, sha)
	if err != nil || receipt.VersionId != "test-version" || receipt.Sha256 != sha {
		t.Fatal("genuine retained report was rejected", err)
	}
	store.corrupt = true
	if _, err := RetainCompetitionWinnerReviewEvidence(context.Background(), settings, round, job, report, sha); err == nil {
		t.Fatal("corrupt immutable readback authorized evidence")
	}
	if _, err := RetainCompetitionWinnerReviewEvidence(context.Background(), settings, round, job, report, strings.Repeat("0", 64)); err == nil {
		t.Fatal("unbound report digest accepted")
	}
}

// Policy identities are synthetic and timing is entirely test-owned.
func newWinnerClosingFixture() *winnerClosingFixture {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	job := server.NewId()
	policy := model.CompetitionAutomationPolicy{Enabled: true, Version: 2, Environment: "test", SeasonId: "test-season", ElectionRule: model.CompetitionElectionRule,
		ReviewCriteriaVersion: "review/v2", SettlementPolicyVersion: "test-settlement", BurnPolicyVersion: "test-burn", RepeatRewardPolicy: "allow", SettlementMode: "shadow",
		RequiredApiCapability: model.CompetitionRuntimeBlobCapability, PolicyApprovalSha256: strings.Repeat("a", 64), SourceLockSha256: strings.Repeat("b", 64), SourceActivationGeneration: 1,
		WinnerReviewPolicy: model.CompetitionWinnerReviewPolicy, WinnerReviewTimeoutSeconds: 600}
	return &winnerClosingFixture{now: now, snapshot: CompetitionSupervisorSnapshot{
		Round:         &model.CompetitionRoundResult{RoundId: server.NewId(), Epoch: 1, Status: "sealed", DecisionMode: model.CompetitionDecisionModeV2},
		Seal:          &model.CompetitionEvaluationSeal{PublicationAt: now, SourceChoiceAt: now, SealSha256: strings.Repeat("c", 64)},
		CurrentSource: &model.CompetitionSourceActivation{Generation: 1, SourceGeneration: 0},
		WinnerReview:  &CompetitionWinnerReviewState{Policy: policy, CurrentDecision: model.CompetitionRewardDecision{Revision: 1, SelectedJobId: &job, DecisionSha256: strings.Repeat("d", 64)}},
	}}
}

func (self *winnerClosingFixture) Acquire(context.Context, string, time.Duration) (*model.CompetitionSupervisorLease, error) {
	return &model.CompetitionSupervisorLease{}, nil
}
func (self *winnerClosingFixture) Renew(context.Context, model.CompetitionSupervisorLease, time.Duration) (*model.CompetitionSupervisorLease, error) {
	return &model.CompetitionSupervisorLease{}, nil
}
func (self *winnerClosingFixture) Snapshot(context.Context, model.CompetitionSupervisorLease) (*CompetitionSupervisorSnapshot, error) {
	snapshot := self.snapshot
	return &snapshot, nil
}
func (self *winnerClosingFixture) SaveTransition(_ context.Context, _ model.CompetitionSupervisorLease, next model.CompetitionSupervisorTransition) (*model.CompetitionSupervisorTransition, error) {
	next.Revision = next.ExpectedRevision + 1
	next.UpdatedAt = self.now
	self.snapshot.Transition = &next
	return &next, nil
}
func (self *winnerClosingFixture) RunWorker(context.Context, model.CompetitionSupervisorLease, model.CompetitionRoundResult) error {
	return nil
}
func (self *winnerClosingFixture) Seal(context.Context, model.CompetitionSupervisorLease, server.Id) error {
	return nil
}
func (self *winnerClosingFixture) ReviewWinner(_ context.Context, _ model.CompetitionSupervisorLease, request CompetitionWinnerReviewRequest) (*CompetitionWinnerReviewState, error) {
	self.reviews++
	if self.failReview {
		return nil, errors.New("synthetic unavailable reviewer")
	}
	state := self.snapshot.WinnerReview
	if self.rejectAll || self.rejectFirst {
		self.rejectFirst = false
		state.CurrentDecision.Revision++
		state.CurrentDecision.DecisionSha256 = strings.Repeat("e", 64)
		if self.rejectAll {
			state.CurrentDecision.SelectedJobId = nil
		} else {
			job := server.NewId()
			state.CurrentDecision.SelectedJobId = &job
		}
		return state, nil
	}
	id := server.NewId()
	state.ApprovedReviewId = &id
	state.ApprovedReview = &model.CompetitionIntegrityReviewResult{ReviewId: id, RoundId: request.RoundId, CreatedAt: self.now,
		Request: model.CompetitionIntegrityReviewRequest{JobId: request.JobId, ExpectedDecisionSha256: request.RewardDecisionSha256, ExpectedRevision: request.RewardRevision,
			RewardDisposition: "approved", SourceDisposition: "approved", ReviewerModel: model.CompetitionWinnerReviewModel, ReviewerReasoning: model.CompetitionWinnerReviewReasoning}}
	return state, nil
}
func (self *winnerClosingFixture) ChooseSource(_ context.Context, _ model.CompetitionSupervisorLease, snapshot *CompetitionSupervisorSnapshot, next model.CompetitionSupervisorTransition) (*model.CompetitionSourceActivation, error) {
	self.choices++
	if self.failBuild {
		return nil, errors.New("synthetic failed source build")
	}
	intent := &model.CompetitionSourceActivation{Generation: snapshot.CurrentSource.Generation + 1, ExpectedGeneration: snapshot.CurrentSource.Generation, SourceGeneration: snapshot.CurrentSource.SourceGeneration, TargetEpoch: next.TargetEpoch, PreparedAt: self.now, WinnerReviewPolicy: model.CompetitionWinnerReviewPolicy, WinnerReview: next.WinnerReview, Kind: "reviewed_source_carry_forward", Reason: "no_reward_winner"}
	if snapshot.WinnerReview.CurrentDecision.SelectedJobId != nil {
		self.builds++
		intent.Kind = "reviewed_source_promotion"
		intent.SourceGeneration = snapshot.CurrentSource.SourceGeneration + 1
		intent.ReviewId = snapshot.WinnerReview.ApprovedReviewId
	}
	self.snapshot.Intent = intent
	return intent, nil
}
func (self *winnerClosingFixture) PublishSource(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) error {
	return nil
}
func (self *winnerClosingFixture) PrepareSource(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) error {
	if self.failPreparation {
		return errors.New("synthetic failed qualification")
	}
	return nil
}
func (self *winnerClosingFixture) ActivateSource(_ context.Context, _ model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation) error {
	intent.State = "active"
	self.snapshot.CurrentSource = intent
	return nil
}

// Six independent clocks/rounds share one incumbent ledger. Rejection advances
// the elected identity; no-winner carries; the final review opens no successor.
func TestWinnerClosingCompleteSixEpochLifecycle(t *testing.T) {
	f := newWinnerClosingFixture()
	policy := f.snapshot.WinnerReview.Policy
	for epoch := 1; epoch <= 6; epoch++ {
		job := server.NewId()
		f.snapshot.Round = &model.CompetitionRoundResult{RoundId: server.NewId(), Epoch: epoch, Status: "sealed", DecisionMode: model.CompetitionDecisionModeV2}
		f.snapshot.Seal = &model.CompetitionEvaluationSeal{PublicationAt: f.now, SourceChoiceAt: f.now, SealSha256: strings.Repeat("a", 64)}
		f.snapshot.Transition, f.snapshot.Intent = nil, nil
		f.snapshot.WinnerReview = &CompetitionWinnerReviewState{Policy: policy, CurrentDecision: model.CompetitionRewardDecision{Revision: 1, SelectedJobId: &job, DecisionSha256: strings.Repeat("b", 64)}}
		if epoch == 2 {
			f.rejectFirst = true
		}
		if epoch == 3 {
			f.snapshot.WinnerReview.CurrentDecision.SelectedJobId = nil
		}
		completed := false
		for step := 0; step < 20; step++ {
			state, err := (&CompetitionSupervisor{Enabled: true, OwnerId: "test-owner", Driver: f, Now: func() time.Time { return f.now }}).Reconcile(context.Background())
			if err != nil {
				t.Fatalf("epoch %d state %s: %v", epoch, state, err)
			}
			if state == "round_created" || state == "season_complete" {
				completed = true
				break
			}
		}
		if !completed {
			t.Fatalf("epoch %d did not finish its deterministic state transitions", epoch)
		}
		if epoch == 3 && f.snapshot.CurrentSource.SourceGeneration != 2 {
			t.Fatal("no-winner epoch changed source")
		}
		f.now = f.now.Add(7 * 24 * time.Hour)
	}
	if f.reviews != 6 || f.builds != 4 || f.opens != 5 || f.snapshot.CurrentSource.SourceGeneration != 4 {
		t.Fatalf("six-epoch closing violated counts: reviews=%d builds=%d opens=%d source=%d", f.reviews, f.builds, f.opens, f.snapshot.CurrentSource.SourceGeneration)
	}
}
func (self *winnerClosingFixture) CreateRound(_ context.Context, _ model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation, opens time.Time) (*model.CompetitionRoundResult, error) {
	self.opens++
	return &model.CompetitionRoundResult{RoundId: server.NewId(), Epoch: intent.TargetEpoch, OpensAt: opens}, nil
}
func (self *winnerClosingFixture) FallbackSource(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation, string) (bool, error) {
	self.fallbacks++
	return true, nil
}

// A new supervisor is constructed at every step, so no in-memory continuation
// can substitute for the durable review identity/deadline.
func (self *winnerClosingFixture) step(t *testing.T, want string, wantError bool) {
	t.Helper()
	state, err := (&CompetitionSupervisor{Enabled: true, OwnerId: "test-owner", Driver: self, Now: func() time.Time { return self.now }}).Reconcile(context.Background())
	if state != want || (err != nil) != wantError {
		t.Fatalf("state %q error %v, want %q error=%t", state, err, want, wantError)
	}
}

func TestWinnerClosingApprovesBuildsQualifiesAndOpens(t *testing.T) {
	f := newWinnerClosingFixture()
	for _, state := range []string{"winner_review_pending", "winner_review_pending", "winner_review_recorded", "source_choice_pending", "source_publication_pending", "preparation_pending", "source_active", "round_created"} {
		f.step(t, state, false)
	}
	if f.reviews != 1 || f.builds != 1 || f.opens != 1 || f.fallbacks != 0 {
		t.Fatalf("unexpected closing effects: %+v", f)
	}
}

func TestWinnerClosingRejectsAndReviewsNextElectedCandidate(t *testing.T) {
	f := newWinnerClosingFixture()
	f.rejectFirst = true
	first := *f.snapshot.WinnerReview.CurrentDecision.SelectedJobId
	for _, state := range []string{"winner_review_pending", "winner_review_pending", "winner_review_recorded", "winner_review_pending", "winner_review_recorded", "source_choice_pending", "source_publication_pending"} {
		f.step(t, state, false)
	}
	if f.reviews != 2 || f.builds != 1 || f.snapshot.Transition.WinnerReview.JobId == first {
		t.Fatal("rejected winner was reused or alternate candidate skipped review")
	}
}

func TestWinnerClosingExhaustedRankingCarriesIncumbent(t *testing.T) {
	f := newWinnerClosingFixture()
	f.rejectAll = true
	for _, state := range []string{"winner_review_pending", "winner_review_pending", "winner_review_recorded", "source_choice_pending", "source_publication_pending"} {
		f.step(t, state, false)
	}
	if f.reviews != 1 || f.builds != 0 || f.snapshot.Intent.Kind != "reviewed_source_carry_forward" || f.snapshot.Intent.WinnerReview != nil {
		t.Fatal("exhausted ranking manufactured promotion")
	}
}

func TestWinnerClosingDeadlineSurvivesRestartAndCannotBecomeCarryForward(t *testing.T) {
	f := newWinnerClosingFixture()
	f.step(t, "winner_review_pending", false)
	f.step(t, "winner_review_pending", false)
	checkpoint := *f.snapshot.Transition.WinnerReview
	f.now = checkpoint.Deadline
	f.step(t, "winner_review_blocked", true)
	f.step(t, "winner_review_blocked", true)
	if *f.snapshot.Transition.WinnerReview != checkpoint || f.choices != 0 || f.reviews != 0 {
		t.Fatal("timeout extended review or advanced source")
	}
}

func TestWinnerClosingInfrastructureFailureDoesNotRejectOrPromote(t *testing.T) {
	f := newWinnerClosingFixture()
	f.failReview = true
	f.step(t, "winner_review_pending", false)
	f.step(t, "winner_review_pending", false)
	f.step(t, "winner_review_blocked", true)
	if f.snapshot.WinnerReview.CurrentDecision.Revision != 1 || f.choices != 0 || f.fallbacks != 0 {
		t.Fatal("review infrastructure failure changed the winner or incumbent")
	}
}

func TestWinnerClosingQualificationFailureCannotFallBack(t *testing.T) {
	f := newWinnerClosingFixture()
	f.failPreparation = true
	for _, state := range []string{"winner_review_pending", "winner_review_pending", "winner_review_recorded", "source_choice_pending", "source_publication_pending", "preparation_pending"} {
		f.step(t, state, false)
	}
	f.step(t, "preparation_blocked", true)
	if f.fallbacks != 0 || f.opens != 0 || f.snapshot.Intent.Kind != "reviewed_source_promotion" {
		t.Fatal("failed winner qualification silently carried forward or opened")
	}
}

// A failed source build stays on the approved winner and its durable retry
// cooldown; it cannot trigger an alternate candidate or repeated tight clones.
func TestWinnerClosingBuildFailureBlocksWithBoundedRetry(t *testing.T) {
	f := newWinnerClosingFixture()
	f.failBuild = true
	for _, state := range []string{"winner_review_pending", "winner_review_pending", "winner_review_recorded", "source_choice_pending"} {
		f.step(t, state, false)
	}
	f.step(t, "winner_source_blocked", true)
	f.failBuild = false
	f.step(t, "winner_source_blocked", true)
	if f.choices != 1 || f.fallbacks != 0 || f.opens != 0 {
		t.Fatal("failed build retried without cooldown or silently advanced")
	}
	f.now = f.now.Add(10 * time.Minute)
	f.step(t, "source_publication_pending", false)
	if f.reviews != 1 || f.choices != 2 {
		t.Fatal("build retry lost approved winner or re-ran honesty review")
	}
}

// Revocation after staging never creates a fresh unrelated source intent.
func TestWinnerClosingRevocationDuringPreparationIsDurablyBlocked(t *testing.T) {
	f := newWinnerClosingFixture()
	for _, state := range []string{"winner_review_pending", "winner_review_pending", "winner_review_recorded", "source_choice_pending", "source_publication_pending"} {
		f.step(t, state, false)
	}
	f.snapshot.WinnerReview.ApprovedReview, f.snapshot.WinnerReview.ApprovedReviewId = nil, nil
	f.step(t, "winner_review_blocked", true)
	if f.snapshot.Transition.State != "winner_review_blocked" || f.opens != 0 || f.fallbacks != 0 {
		t.Fatal("revoked source approval was neither blocked nor observable")
	}
}

func TestWinnerClosingSixthEpochStillRequiresReview(t *testing.T) {
	f := newWinnerClosingFixture()
	f.snapshot.Round.Epoch = 6
	for _, state := range []string{"winner_review_pending", "winner_review_pending", "winner_review_recorded", "complete", "season_complete"} {
		f.step(t, state, false)
	}
	if f.reviews != 1 || f.opens != 0 || f.snapshot.Transition.TargetEpoch != 7 {
		t.Fatal("last epoch skipped its review or opened a seventh round")
	}
}

func TestWinnerClosingCannotReviewBeforePublicReveal(t *testing.T) {
	f := newWinnerClosingFixture()
	f.snapshot.Seal.PublicationAt = f.now.Add(time.Hour)
	f.step(t, "winner_review_pending", false)
	f.step(t, "awaiting_publication", false)
	if f.reviews != 0 || f.snapshot.Transition.WinnerReview != nil {
		t.Fatal("unpublished candidate reached review")
	}
}

func TestWinnerClosingProjectionIgnoresOtherCandidatesAndRevocation(t *testing.T) {
	job, other, reviewId := server.NewId(), server.NewId(), server.NewId()
	result := &model.CompetitionV2RoundResult{Decisions: []model.CompetitionRewardDecision{{SelectedJobId: &job}}}
	review := model.CompetitionIntegrityReviewResult{ReviewId: reviewId, Request: model.CompetitionIntegrityReviewRequest{JobId: other, RewardDisposition: "approved", SourceDisposition: "approved", ReviewerModel: model.CompetitionWinnerReviewModel, ReviewerReasoning: model.CompetitionWinnerReviewReasoning}}
	result.IntegrityReviews = []model.CompetitionIntegrityReviewResult{review}
	state, err := winnerReviewState(result, model.CompetitionAutomationPolicy{})
	if err != nil || state.ApprovedReviewId != nil {
		t.Fatal("different candidate's approval was accepted", err)
	}
	result.IntegrityReviews[0].Request.JobId = job
	state, err = winnerReviewState(result, model.CompetitionAutomationPolicy{})
	if err != nil || state.ApprovedReviewId == nil {
		t.Fatal("same elected candidate approval was lost", err)
	}
	result.IntegrityReviews = append(result.IntegrityReviews, model.CompetitionIntegrityReviewResult{Request: model.CompetitionIntegrityReviewRequest{JobId: job, RewardDisposition: "disqualified", SourceDisposition: "rejected"}})
	state, err = winnerReviewState(result, model.CompetitionAutomationPolicy{})
	if err != nil || state.ApprovedReviewId != nil {
		t.Fatal("revoked approval still authorized source", err)
	}
}

// Discovery must describe the selected immutable round, not a prospective
// main configuration which would change the entrant's closing contract.
func TestWinnerClosingDiscoveryUsesFrozenPolicy(t *testing.T) {
	f := newWinnerClosingFixture()
	settings := &Settings{CompetitionId: "test-competition", BaseSha: strings.Repeat("a", 40), EvaluatorImageDigest: "sha256:" + strings.Repeat("b", 64),
		EvaluatorCommand: "/test/evaluator", EvaluatorCommandSha256: strings.Repeat("c", 64), AutomationPolicy: f.snapshot.WinnerReview.Policy}
	info := settings.PublicInfo()
	if info.AutomationPolicy == nil || info.AutomationPolicy.WinnerReviewPolicy != model.CompetitionWinnerReviewPolicy {
		t.Fatal("new closing policy was not discoverable")
	}
	settings.AutomationPolicy.WinnerReviewPolicy, settings.AutomationPolicy.WinnerReviewTimeoutSeconds = "", 0
	stored, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyFrozenRoundPublicPolicy(&info, &roundRecord{PolicyJson: stored}); err != nil {
		t.Fatal(err)
	}
	if info.AutomationPolicy == nil || info.AutomationPolicy.WinnerReviewPolicy != "" {
		t.Fatal("prospective config reinterpreted the old round")
	}
}
