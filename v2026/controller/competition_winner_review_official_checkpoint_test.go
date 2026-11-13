// Imported official approvals can precede supervisor startup. Adoption records
// the present reconciliation boundary without inventing native model custody.
package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Build a complete synthetic amendment through the same public validator used
// for authentic stored approvals, rather than an incomplete approval-shaped stub.
func newOfficialWinnerClosingFixture(t *testing.T) *winnerClosingFixture {
	t.Helper()
	fixture := newWinnerClosingFixture()
	state := fixture.snapshot.WinnerReview
	state.Policy.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicy
	fixture.snapshot.Seal.RoundId = fixture.snapshot.Round.RoundId
	fixture.snapshot.Seal.BaselineSha256 = strings.Repeat("6", 64)
	fixture.snapshot.Seal.PublicationAt = fixture.now.Add(-2 * time.Hour)
	result := &model.CompetitionV2RoundResult{Seal: *fixture.snapshot.Seal, RewardRevision: state.CurrentDecision.Revision, Decisions: []model.CompetitionRewardDecision{state.CurrentDecision}}
	entry := model.CompetitionSealedEntry{JobId: *state.CurrentDecision.SelectedJobId, PatchSha256: strings.Repeat("1", 64), BaseSourceLockSha256: strings.Repeat("2", 64), ScoreSha256: strings.Repeat("3", 64), ArtifactManifestSha256: strings.Repeat("4", 64)}
	request := competitionAutomationReviewRequest(result, entry, false)
	request.CriteriaVersion = state.Policy.ReviewCriteriaVersion
	request.ExpectedDecisionSha256 = state.CurrentDecision.DecisionSha256
	request.SourceDisposition = "approved"
	request.ReviewerModel, request.ReviewerReasoning = "gpt-6-astra", "max"
	request.OfficialReviewOverride = &model.CompetitionOfficialReviewOverride{AuthorizationSha256: strings.Repeat("5", 64), CompletedAt: fixture.now.Add(-time.Minute), NativeQualificationWaived: true, UnacceptedCalibrationAcknowledged: true}
	if err := model.ValidateCompetitionOfficialReviewImport(model.CompetitionOfficialReviewImport{RoundId: fixture.snapshot.Round.RoundId, Review: request}, state.Policy); err != nil {
		t.Fatal(err)
	}
	review := model.CompetitionIntegrityReviewResult{ReviewId: server.NewId(), RoundId: fixture.snapshot.Round.RoundId, CreatedAt: fixture.now.Add(-30 * time.Second), Request: request}
	state.ApprovedReview, state.ApprovedReviewId = &review, &review.ReviewId
	return fixture
}

func TestWinnerClosingOfficialApprovalAdoptsMissingCheckpointWithoutReview(t *testing.T) {
	fixture := newOfficialWinnerClosingFixture(t)
	fixture.step(t, "winner_review_pending", false)
	fixture.step(t, "winner_review_pending", false)
	checkpoint := fixture.snapshot.Transition.WinnerReview
	if checkpoint == nil || !checkpoint.StartedAt.Equal(fixture.now) || checkpoint.HandoffId != "" || fixture.snapshot.WinnerReview.ApprovedReview.ModelCompletedAt != nil {
		t.Fatal("adoption lost current time or fabricated native provenance", checkpoint)
	}
	fixture.step(t, "source_choice_pending", false)
	fixture.step(t, "source_publication_pending", false)
	if fixture.reviews != 0 || fixture.builds != 1 {
		t.Fatal("approved import reran review or failed source progression", fixture.reviews, fixture.builds)
	}
}

func TestWinnerClosingOfficialApprovalPreservesExistingExpiredCheckpoint(t *testing.T) {
	fixture := newOfficialWinnerClosingFixture(t)
	fixture.step(t, "winner_review_pending", false)
	decision := fixture.snapshot.WinnerReview.CurrentDecision
	checkpoint := &model.CompetitionWinnerReviewCheckpoint{RoundId: fixture.snapshot.Round.RoundId, JobId: *decision.SelectedJobId, RewardRevision: decision.Revision, RewardDecisionSha256: decision.DecisionSha256, StartedAt: fixture.now.Add(-4 * time.Hour), Deadline: fixture.now.Add(-3 * time.Hour)}
	fixture.snapshot.Transition.WinnerReview = checkpoint
	before := *checkpoint
	fixture.step(t, "source_choice_pending", false)
	if *fixture.snapshot.Transition.WinnerReview != before || fixture.reviews != 0 {
		t.Fatal("official approval rewrote an existing review clock")
	}
}

func TestWinnerClosingOfficialApprovalCannotReplaceConflictingCheckpoint(t *testing.T) {
	fixture := newOfficialWinnerClosingFixture(t)
	fixture.step(t, "winner_review_pending", false)
	checkpoint := &model.CompetitionWinnerReviewCheckpoint{RoundId: fixture.snapshot.Round.RoundId, JobId: server.NewId(), StartedAt: fixture.now.Add(-time.Hour), Deadline: fixture.now}
	fixture.snapshot.Transition.WinnerReview = checkpoint
	before := *checkpoint
	fixture.step(t, "winner_review_blocked", true)
	if *fixture.snapshot.Transition.WinnerReview != before || fixture.reviews != 0 {
		t.Fatal("conflicting checkpoint was replaced or reviewed")
	}
}

func TestWinnerClosingNativeApprovalCannotCreateReplacementClock(t *testing.T) {
	fixture := newOfficialWinnerClosingFixture(t)
	fixture.snapshot.WinnerReview.ApprovedReview.Request.OfficialReviewOverride = nil
	fixture.step(t, "winner_review_pending", false)
	fixture.step(t, "winner_review_blocked", true)
	if fixture.snapshot.Transition.WinnerReview != nil || fixture.reviews != 0 {
		t.Fatal("native approval acquired an unowned replacement clock")
	}
}

func TestWinnerClosingOfficialApprovalRequiresCompleteAmendmentForAdoption(t *testing.T) {
	fixture := newOfficialWinnerClosingFixture(t)
	fixture.snapshot.WinnerReview.ApprovedReview.Request.OfficialReviewOverride.NativeQualificationWaived = false
	fixture.step(t, "winner_review_pending", false)
	fixture.step(t, "winner_review_blocked", true)
	if fixture.snapshot.Transition.WinnerReview != nil {
		t.Fatal("incomplete amendment acquired a checkpoint")
	}
}

func TestWinnerClosingOfficialCheckpointAdoptionCannotCrossEpoch(t *testing.T) {
	fixture := newOfficialWinnerClosingFixture(t)
	fixture.snapshot.Round.Epoch = 2
	fixture.step(t, "winner_review_pending", false)
	fixture.step(t, "winner_review_blocked", true)
	if fixture.snapshot.Transition.WinnerReview != nil {
		t.Fatal("epoch-one amendment acquired a later-epoch checkpoint")
	}
}

func TestWinnerClosingOfficialCheckpointAdoptionRejectsFutureImport(t *testing.T) {
	fixture := newOfficialWinnerClosingFixture(t)
	fixture.snapshot.WinnerReview.ApprovedReview.CreatedAt = fixture.now.Add(time.Second)
	fixture.step(t, "winner_review_pending", false)
	fixture.step(t, "winner_review_blocked", true)
	if fixture.snapshot.Transition.WinnerReview != nil {
		t.Fatal("unobserved future import acquired a checkpoint")
	}
}
