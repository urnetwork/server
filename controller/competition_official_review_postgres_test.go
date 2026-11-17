// The real PostgreSQL guards exercise an epoch-one operator import without
// changing its frozen policy or assigning native custody to an external review.
package controller

import (
	"context"
	"encoding/json"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"strings"
	"testing"
	"time"
)

func TestCompetitionOfficialReviewPostgresExactAmendmentAndReplay(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixtureWithWinnerPolicy(t, false, model.CompetitionRuntimeBlobCapability, model.CompetitionSnowWinnerReviewPolicy, server.NowUtc().Add(-8*24*time.Hour))
		job := addCompetitionAutomationJob(t, fixture, 80)
		fixture.now = fixture.round.RevealAt.Add(time.Second)
		sealed, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || sealed.ProvisionalRewardWinner == nil || *sealed.ProvisionalRewardWinner != job.JobId {
			t.Fatal("synthetic election", err)
		}
		decision := sealed.Decisions[len(sealed.Decisions)-1]
		fixture.now = server.NowUtc()
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-official-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		checkpoint := &model.CompetitionWinnerReviewCheckpoint{RoundId: fixture.round.RoundId, JobId: job.JobId, RewardRevision: decision.Revision, RewardDecisionSha256: decision.DecisionSha256, StartedAt: sealed.Seal.PublicationAt, Deadline: sealed.Seal.PublicationAt.Add(time.Duration(fixture.settings.AutomationPolicy.WinnerReviewTimeoutSeconds) * time.Second)}
		if _, err = fixture.store.RecordSupervisorTransition(ctx, fixture.settings, *lease, model.CompetitionSupervisorTransition{TargetEpoch: 2, State: "winner_review_pending", SourceActivationGeneration: 1, Reason: "synthetic expired historical review checkpoint", WinnerReview: checkpoint}); err != nil {
			t.Fatal(err)
		}
		review := competitionAutomationReviewRequest(sealed, sealed.Seal.Entries[0], false)
		review.ExpectedDecisionSha256 = decision.DecisionSha256
		review.ReviewerModel, review.ReviewerReasoning = "gpt-6-astra", "max"
		review.SourceDisposition = "approved"
		review.OfficialReviewOverride = &model.CompetitionOfficialReviewOverride{AuthorizationSha256: strings.Repeat("e", 64), CompletedAt: server.NowUtc().Add(-time.Second), NativeQualificationWaived: true, UnacceptedCalibrationAcknowledged: true}
		args := model.CompetitionOfficialReviewImport{RoundId: fixture.round.RoundId, Review: review}
		pin, err := model.CompetitionAutomationDigest(args)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = fixture.store.ImportOfficialReview(ctx, fixture.settings, "synthetic-review-operator", args); err == nil {
			t.Fatal("missing grant accepted")
		}
		fixture.settings.OfficialReviewImportSha256 = pin
		result, err := fixture.store.ImportOfficialReview(ctx, fixture.settings, "synthetic-review-operator", args)
		if err != nil {
			t.Fatal("exact official amendment", err)
		}
		if result.Request.ReviewerModel != "gpt-6-astra" || result.ModelCompletedAt != nil || result.Request.HandoffId != "" {
			t.Fatal("official review relabeled native provenance")
		}
		replay, err := fixture.store.ImportOfficialReview(ctx, fixture.settings, "synthetic-review-operator", args)
		if err != nil || replay.ReviewId != result.ReviewId || replay.CreatedAt != result.CreatedAt {
			t.Fatal("exact replay changed approval", err)
		}
		changed := args
		changed.Review.PublicExplanation += " changed"
		if _, err = fixture.store.ImportOfficialReview(ctx, fixture.settings, "synthetic-review-operator", changed); err == nil {
			t.Fatal("changed exact grant accepted")
		}
		if _, err = fixture.store.ImportOfficialReview(ctx, fixture.settings, "different-synthetic-operator", args); err == nil {
			t.Fatal("replay lost original actor")
		}
		state, err := fixture.store.WinnerReviewState(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || state.ApprovedReviewId == nil || *state.ApprovedReviewId != result.ReviewId {
			t.Fatal("official source approval not projected", err)
		}
		public, err := fixture.store.AutomationResult(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || len(public.IntegrityReviews) != 1 || public.RewardRevision != decision.Revision {
			t.Fatal("amendment changed ranking or duplicated review", err)
		}
		intent := &model.CompetitionSourceActivation{Environment: fixture.settings.AutomationPolicy.Environment, CompetitionId: fixture.settings.CompetitionId, SeasonId: fixture.settings.AutomationPolicy.SeasonId, TargetEpoch: 2, Kind: "reviewed_source_promotion", ReviewId: &result.ReviewId, RewardDecisionDigest: decision.DecisionSha256, WinnerReviewPolicy: fixture.settings.AutomationPolicy.WinnerReviewPolicy, WinnerReview: checkpoint, BuildReadyAt: fixture.now, PreparedAt: fixture.now}
		content, err := json.Marshal(intent)
		if err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			if err := validateWinnerSourceInTx(ctx, tx, fixture.settings, intent, fixture.round.RoundId); err != nil {
				t.Fatal("official review lost Go source boundary", err)
			}
			var authorized bool
			if err := tx.QueryRow(ctx, `SELECT competition_v2_winner_source_authorized($1::jsonb,$2)`, string(content), fixture.round.RoundId).Scan(&authorized); err != nil || !authorized {
				t.Fatal("official review lost SQL source boundary", err)
			}
		})
	})
}
