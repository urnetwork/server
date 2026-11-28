// Synthetic supervisor snapshots prove that prereview substitution changes
// neither the elected identity nor production build/qualification authority.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// This retains complete common findings and creates only a real current clock,
// not the legacy epoch-one import's missing-checkpoint adoption.
func newEpochTwoOfficialWinnerFixture(t *testing.T) *winnerClosingFixture {
	t.Helper()
	fixture := newOfficialWinnerClosingFixture(t)
	fixture.snapshot.Round.Epoch = 2
	fixture.snapshot.Seal.Epoch = 2
	fixture.snapshot.Seal.PublicationAt = fixture.now.Add(-40 * time.Second)
	review := fixture.snapshot.WinnerReview.ApprovedReview
	review.Request.OfficialReviewOverride.NativeQualificationWaived = false
	review.Request.OfficialReviewOverride.UnacceptedCalibrationAcknowledged = false
	fixture.snapshot.Seal.Entries = []model.CompetitionSealedEntry{{JobId: review.Request.JobId, State: "succeeded", Eligible: true,
		CompletedAt: review.Request.OfficialReviewOverride.CompletedAt.Add(-time.Minute), PatchSha256: review.Request.PatchSha256,
		BaseSourceLockSha256: review.Request.BaseSourceLockSha256, ScoreSha256: review.Request.ScoreSha256, ArtifactManifestSha256: review.Request.ArtifactManifestSha256}}
	if err := model.ValidateCompetitionOfficialReviewImportForEpoch(model.CompetitionOfficialReviewImport{RoundId: review.RoundId, Review: review.Request}, fixture.snapshot.WinnerReview.Policy, 2); err != nil {
		t.Fatal(err)
	}
	fixture.step(t, "winner_review_pending", false)
	decision := fixture.snapshot.WinnerReview.CurrentDecision
	fixture.snapshot.Transition.WinnerReview = &model.CompetitionWinnerReviewCheckpoint{RoundId: review.RoundId, JobId: review.Request.JobId,
		RewardRevision: decision.Revision, RewardDecisionSha256: decision.DecisionSha256, StartedAt: fixture.now.Add(-35 * time.Second), Deadline: fixture.now.Add(time.Minute)}
	return fixture
}

// The real reconciliation entry point must retain the genuine prereview time
// and refuse to interpret it as a completed native Snow call.
func TestWinnerClosingEpochTwoOfficialPrereviewUsesCurrentCheckpoint(t *testing.T) {
	for _, modelName := range []string{"gpt-6-astra", "gpt-6.2-astra"} {
		t.Run(modelName, func(t *testing.T) {
			fixture := newEpochTwoOfficialWinnerFixture(t)
			prior := fixture.snapshot.WinnerReview
			review := prior.ApprovedReview
			review.Request.ReviewerModel = modelName
			state, err := winnerReviewState(&model.CompetitionV2RoundResult{Seal: *fixture.snapshot.Seal,
				Decisions: []model.CompetitionRewardDecision{prior.CurrentDecision}, IntegrityReviews: []model.CompetitionIntegrityReviewResult{*review}}, prior.Policy)
			if err != nil || state.ApprovedReview == nil || state.ApprovedReview.ReviewId != review.ReviewId {
				t.Fatal("persisted official reviewer could not be projected", err)
			}
			fixture.snapshot.WinnerReview = state
			before := *fixture.snapshot.Transition.WinnerReview
			completed := review.Request.OfficialReviewOverride.CompletedAt
			fixture.step(t, "source_choice_pending", false)
			fixture.step(t, "source_publication_pending", false)
			if *fixture.snapshot.Transition.WinnerReview != before || fixture.reviews != 0 || fixture.builds != 1 ||
				state.ApprovedReview.ModelCompletedAt != nil || state.ApprovedReview.Request.ReviewerModel != modelName ||
				!state.ApprovedReview.Request.OfficialReviewOverride.CompletedAt.Equal(completed) {
				t.Fatal("prereview was retimed, relabeled or renewed its checkpoint")
			}
		})
	}
}

// An epoch-two import cannot inherit epoch one's clock exemption or replace an
// already owned native checkpoint, even with otherwise complete review content.
func TestWinnerClosingEpochTwoOfficialPrereviewRejectsMissingExpiredAndNativeClock(t *testing.T) {
	for i, mutate := range []func(*winnerClosingFixture){
		func(f *winnerClosingFixture) { f.snapshot.Transition.WinnerReview = nil },
		func(f *winnerClosingFixture) {
			f.snapshot.WinnerReview.ApprovedReview.CreatedAt = f.snapshot.Transition.WinnerReview.Deadline
		},
		func(f *winnerClosingFixture) { f.snapshot.Transition.WinnerReview.HandoffId = server.NewId().String() },
		func(f *winnerClosingFixture) { f.snapshot.Transition.WinnerReview.JobId = server.NewId() },
		func(f *winnerClosingFixture) { f.snapshot.Seal.Entries[0].ScoreSha256 = "changed" },
		func(f *winnerClosingFixture) { f.snapshot.Seal.Entries[0].CompletedAt = f.now },
	} {
		fixture := newEpochTwoOfficialWinnerFixture(t)
		mutate(fixture)
		fixture.step(t, "winner_review_blocked", true)
		if fixture.builds != 0 || fixture.opens != 0 || fixture.reviews != 0 {
			t.Fatal("invalid prereview reached an actor or source gate", i)
		}
	}
}

// An honesty approval cannot stand in for the independent production rollout.
func TestWinnerClosingEpochTwoOfficialReviewRetainsQualificationFailure(t *testing.T) {
	fixture := newEpochTwoOfficialWinnerFixture(t)
	fixture.failPreparation = true
	for _, state := range []string{"source_choice_pending", "source_publication_pending", "preparation_pending"} {
		fixture.step(t, state, false)
	}
	fixture.step(t, "preparation_blocked", true)
	if fixture.opens != 0 || fixture.fallbacks != 0 || fixture.snapshot.Intent.Kind != "reviewed_source_promotion" {
		t.Fatal("official review bypassed qualification or changed the selected source")
	}
}

// The normal decision engine retains source-deferred honesty approvals and
// advances only a substantiated rejection to the next already eligible entry.
func TestCompetitionEpochTwoOfficialReviewKeepsDecisionAndSourceIndependence(t *testing.T) {
	fixture := newEpochTwoOfficialWinnerFixture(t)
	review := *fixture.snapshot.WinnerReview.ApprovedReview
	review.Request.SourceDisposition = "deferred"
	review.Request.ReasonCodes = []string{"SOURCE_NOT_READY"}
	policy := fixture.snapshot.WinnerReview.Policy
	if err := model.ValidateCompetitionOfficialReviewImportForEpoch(model.CompetitionOfficialReviewImport{RoundId: review.RoundId, Review: review.Request}, policy, 2); err != nil {
		t.Fatal(err)
	}
	seal := *fixture.snapshot.Seal
	seal.Entries = append(append([]model.CompetitionSealedEntry{}, seal.Entries...), model.CompetitionSealedEntry{JobId: server.NewId(), Eligible: true, CompletedAt: fixture.now.Add(-time.Minute)})
	before := fixture.snapshot.WinnerReview.CurrentDecision
	decision := automationDecision(&seal, policy, &before, []model.CompetitionIntegrityReviewResult{review}, fixture.now)
	if decision.SelectedJobId == nil || *decision.SelectedJobId != review.Request.JobId {
		t.Fatal("source deferral revoked an honest elected reward")
	}
	result := &model.CompetitionV2RoundResult{Seal: seal, Decisions: []model.CompetitionRewardDecision{before}, IntegrityReviews: []model.CompetitionIntegrityReviewResult{review}}
	state, err := winnerReviewState(result, policy)
	if err != nil || state.ApprovedReviewId != nil {
		t.Fatal("honesty approval fabricated source approval", err)
	}
	review.Request.RewardDisposition, review.Request.SourceDisposition = "disqualified", "rejected"
	review.Request.ReasonCodes = []string{"UNSAFE_BEHAVIOR"}
	review.Request.Findings = append([]model.CompetitionReviewFinding{}, review.Request.Findings...)
	review.Request.Findings[0].Disposition, review.Request.Findings[0].ReasonCode = "fail", "UNSAFE_BEHAVIOR"
	if err := model.ValidateCompetitionOfficialReviewImportForEpoch(model.CompetitionOfficialReviewImport{RoundId: review.RoundId, Review: review.Request}, policy, 2); err != nil {
		t.Fatal(err)
	}
	decision = automationDecision(&seal, policy, &before, []model.CompetitionIntegrityReviewResult{review}, fixture.now)
	if decision.SelectedJobId == nil || *decision.SelectedJobId != seal.Entries[1].JobId || decision.PreviousDecisionSha256 != before.DecisionSha256 {
		t.Fatal("substantive rejection skipped the immutable ranking or lost decision ancestry")
	}
}

// An unchanged retained grant can be replayed without reauthorizing a different
// election or issuing another INSERT after the original rejection took effect.
type epochTwoOfficialReplayTx struct {
	server.PgTx
	now       time.Time
	same      bool
	epoch     int
	staging   bool
	mutations int
}

// The fixture accepts only the authorizer's fixed metadata reads.
func (self *epochTwoOfficialReplayTx) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	return supervisorScopeRenewTestRow{scan: func(destinations []any) error {
		switch {
		case strings.HasPrefix(query, "SELECT epoch_number,staging FROM competition_round"):
			*destinations[0].(*int), *destinations[1].(*bool) = self.epoch, self.staging
		case strings.Contains(query, "competition_review_handoff_clock()"):
			*destinations[0].(*time.Time) = self.now
		case strings.Contains(query, "FROM competition_official_review_authorization"):
			*destinations[0].(*bool) = self.same
		default:
			return errors.New("unexpected synthetic replay read")
		}
		return nil
	}}
}

// A replay must not revisit a new-grant trigger using the new election/clock.
func (self *epochTwoOfficialReplayTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	self.mutations++
	return pgconn.CommandTag{}, errors.New("synthetic replay attempted a new authorization")
}

// The actual authorizer retains the original actor/request grant. Normal
// record replay still returns the previously persisted review, not a new one.
func TestCompetitionEpochTwoOfficialReplayDoesNotReauthorizeAfterRejection(t *testing.T) {
	fixture := newEpochTwoOfficialWinnerFixture(t)
	review := fixture.snapshot.WinnerReview.ApprovedReview.Request
	review.RewardDisposition, review.SourceDisposition, review.ReasonCodes = "disqualified", "rejected", []string{"UNSAFE_BEHAVIOR"}
	review.Findings = append([]model.CompetitionReviewFinding{}, review.Findings...)
	review.Findings[0].Disposition, review.Findings[0].ReasonCode = "fail", "UNSAFE_BEHAVIOR"
	request := model.CompetitionOfficialReviewImport{RoundId: fixture.snapshot.Round.RoundId, Review: review}
	digest, err := model.CompetitionAutomationDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	settings := &Settings{OfficialReviewImportSha256: digest}
	tx := &epochTwoOfficialReplayTx{epoch: 2, same: true, now: fixture.now.Add(time.Hour)}
	if err := (PostgresStore{}).authorizeOfficialReviewInTx(t.Context(), tx, settings, "synthetic-operator", request, fixture.snapshot.WinnerReview.Policy); err != nil || tx.mutations != 0 {
		t.Fatal("exact historical rejection was reauthorized or rejected", err, tx.mutations)
	}
	newReviewer := request
	newReviewer.Review.ReviewerModel = "gpt-6.2-astra"
	if err := (PostgresStore{}).authorizeOfficialReviewInTx(t.Context(), tx, settings, "synthetic-operator", newReviewer, fixture.snapshot.WinnerReview.Policy); !errors.Is(err, ErrConflict) || tx.mutations != 0 {
		t.Fatal("changed model provenance reused the historical grant", err, tx.mutations)
	}
	for _, changed := range []*epochTwoOfficialReplayTx{
		{epoch: 2, same: false, now: tx.now}, {epoch: 2, same: true, staging: true, now: tx.now}, {epoch: 3, same: true, now: tx.now},
	} {
		if err := (PostgresStore{}).authorizeOfficialReviewInTx(t.Context(), changed, settings, "synthetic-operator", request, fixture.snapshot.WinnerReview.Policy); err == nil || changed.mutations != 0 {
			t.Fatal("changed custody acquired replay authority", err, changed.mutations)
		}
	}
}
