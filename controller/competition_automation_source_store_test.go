// Synthetic source fixtures exercise durable authority without Git pushes or production deployments.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A carry-forward keeps the reviewed source and threshold while committing a new manifest.
func competitionAutomationSourceIntent(t testing.TB, fixture *competitionRankingFixture, seal *model.CompetitionV2RoundResult, generation int64) model.CompetitionSourceActivation {
	t.Helper()
	current, err := fixture.store.CurrentSourceActivation(context.Background(), fixture.settings)
	if err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	var intent model.CompetitionSourceActivation
	if err = json.Unmarshal(content, &intent); err != nil {
		t.Fatal(err)
	}
	intent.Generation, intent.ExpectedGeneration, intent.TargetEpoch = generation, current.Generation, current.TargetEpoch+1
	intent.TargetRoundId = server.NewId()
	intent.BaseSourceLockSha256 = current.SourceLockSha256
	intent.Kind, intent.Reason = "reviewed_source_carry_forward", "review_pending"
	intent.ReviewId, intent.ReviewDecisionDigest, intent.ApprovedPatchSha256 = nil, "", ""
	intent.RewardDecisionDigest = seal.Decisions[len(seal.Decisions)-1].DecisionSha256
	intent.IntentSha256, intent.State, intent.ActivatedAt, intent.AbandonedAt = "", "prepared", nil, nil
	intent.PreparedAt, intent.OpensAt, intent.BuildReadyAt = fixture.now, time.Time{}, time.Time{}
	intent.ConfigExpectedCommit = current.ManifestCommit
	intent.ManifestCommit = fmt.Sprintf("%040x", generation+100)
	intent.Manifest = []byte(fmt.Sprintf("synthetic immutable source manifest generation %d\n", generation))
	digest := sha256.Sum256(intent.Manifest)
	intent.ManifestSha256 = hex.EncodeToString(digest[:])
	intent.GitRefs["config"] = model.CompetitionSourceGitRef{Branch: "synthetic-config", ExpectedCommit: intent.ConfigExpectedCommit, Commit: intent.ManifestCommit}
	if intent.Staging {
		ref := intent.GitRefs["config"]
		ref.Branch = "main"
		intent.GitRefs["config"] = ref
	}
	return intent
}

// API and worker attest the exact immutable source identity independently.
func acknowledgeCompetitionAutomationSource(t testing.TB, fixture *competitionRankingFixture, intent *model.CompetitionSourceActivation, roles ...string) {
	t.Helper()
	for _, role := range roles {
		if err := fixture.store.RecordSourceReadiness(context.Background(), fixture.settings, model.CompetitionSourceReadiness{
			Staging:     intent.Staging,
			Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId, Generation: intent.Generation, TargetRoundId: intent.TargetRoundId,
			Role: role, ActorId: "synthetic-" + role, ManifestCommit: intent.ManifestCommit, ManifestSha256: intent.ManifestSha256,
			SourceLockSha256: intent.SourceLockSha256, RuntimeImageDigest: testWorkerImageDigest(), QualificationSha256: intent.HostQualificationSha256, Ready: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// A synthetic genuine approval binds the exact immutable score and reviewed server-only change.
func competitionAutomationPromotedSourceIntent(t testing.TB, fixture *competitionRankingFixture, seal *model.CompetitionV2RoundResult, review *model.CompetitionIntegrityReviewResult) model.CompetitionSourceActivation {
	t.Helper()
	intent := competitionAutomationSourceIntent(t, fixture, seal, 2)
	intent.Kind, intent.Reason, intent.SourceGeneration = "reviewed_source_promotion", "genuine_review_completed", 1
	intent.ReviewId, intent.ReviewDecisionDigest, intent.ApprovedPatchSha256 = &review.ReviewId, review.RequestSha256, review.Request.PatchSha256
	intent.EvaluatedImageDigest, intent.BuildReadyAt = fixture.settings.EvaluatorImageDigest, fixture.now
	intent.ThresholdEvidenceSha256 = review.Request.ScoreSha256
	intent.Threshold = max(intent.Threshold, *seal.Seal.Entries[0].Score.Significance.RecommendedNextEpochTakeoverMarginPercent/100)
	intent.Commits["server"] = strings.Repeat("2", 40)
	intent.GitRefs["server"] = model.CompetitionSourceGitRef{Branch: "synthetic-source", ExpectedCommit: fixture.settings.BaseSha, Commit: intent.Commits["server"]}
	var err error
	intent.SourceLockSha256, err = model.CompetitionSourceLockDigest(intent.Commits)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

// Even an internal SQL writer cannot bypass readiness, pointer CAS, or the full preparation window.
func TestCompetitionAutomationSourceDatabaseFencesAndCarryForward(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		addCompetitionAutomationJob(t, fixture, 80)
		fixture.now = fixture.round.RevealAt
		seal, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-next-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		intent, err := fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, competitionAutomationSourceIntent(t, fixture, seal, 2))
		if err != nil {
			t.Fatal(err)
		}
		// Qualification is possible for the reserved UUID before there is a round or scoring control.
		if _, err = fixture.store.GetRound(ctx, fixture.settings, intent.TargetRoundId); !errors.Is(err, ErrNotFound) {
			t.Fatalf("source preparation prematurely created the admission round: %v", err)
		}
		for _, fenced := range []bool{false, true} {
			mutationErr := captureDatabaseError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					if fenced {
						server.Raise(fixture.store.requireAutomationLeaseInTx(ctx, tx, fixture.settings, *lease))
					}
					server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_source_activation SET activated_at=$4 WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=2`,
						intent.Environment, intent.CompetitionId, intent.SeasonId, fixture.now))
				})
			})
			if mutationErr == nil {
				t.Fatalf("source activation bypassed readiness with fenced=%t", fenced)
			}
		}
		acknowledgeCompetitionAutomationSource(t, fixture, intent, "api")
		if _, err = fixture.store.ActivateSource(ctx, fixture.settings, *lease, 2, 1); !errors.Is(err, ErrReviewNotReady) {
			t.Fatalf("one-component readiness activated source: %v", err)
		}
		acknowledgeCompetitionAutomationSource(t, fixture, intent, "worker")
		if _, err = fixture.store.ActivateSource(ctx, fixture.settings, *lease, 2, 1); err != nil {
			t.Fatal(err)
		}
		pointerErr := captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(fixture.store.requireAutomationLeaseInTx(ctx, tx, fixture.settings, *lease))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_source_pointer SET generation=1 WHERE environment=$1 AND competition_id=$2 AND season_id=$3`, intent.Environment, intent.CompetitionId, intent.SeasonId))
			})
		})
		if pointerErr == nil {
			t.Fatal("source pointer accepted a backwards activation")
		}
		if _, err = fixture.store.CreateAutomationRound(ctx, fixture.settings, *lease, 2, fixture.now); !errors.Is(err, ErrReviewNotReady) {
			t.Fatalf("source bypassed sixteen-hour preparation: %v", err)
		}
		fixture.now = intent.PreparedAt.Add(16 * time.Hour)
		lease, err = fixture.store.AcquireAutomationLease(ctx, fixture.settings, lease.OwnerId, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		next, err := fixture.store.CreateAutomationRound(ctx, fixture.settings, *lease, 2, fixture.now)
		if err != nil || next.RoundId != intent.TargetRoundId || next.Epoch != 2 || next.ClosesAt.Sub(next.OpensAt) != 7*24*time.Hour {
			t.Fatalf("qualified next round = %+v, error = %v", next, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var baselineCount int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_round_baseline WHERE round_id=$1`, next.RoundId).Scan(&baselineCount))
			if baselineCount != 0 {
				t.Fatal("source preparation manufactured or reused a scoring baseline")
			}
		})
		replayed, err := fixture.store.CreateAutomationRound(ctx, fixture.settings, *lease, 2, fixture.now)
		if err != nil || replayed.RoundId != next.RoundId {
			t.Fatalf("round creation replay = %+v, error = %v", replayed, err)
		}
		result, err := fixture.store.AutomationResult(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || result.SourceDecision == nil || result.SourceDecision.Kind != "reviewed_source_carry_forward" || result.SourceDecision.State != "active" {
			t.Fatalf("published source choice = %+v, error = %v", result, err)
		}
	})
}

// An unpublished failed promotion can be abandoned once and replaced by the unchanged incumbent.
func TestCompetitionAutomationUnpublishedPromotionFallbackRetainsHistory(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		addCompetitionAutomationJob(t, fixture, 80)
		fixture.now = fixture.round.RevealAt
		seal, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		request := competitionAutomationReviewRequest(seal, seal.Seal.Entries[0], false)
		request.SourceDisposition = "approved"
		review, err := fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, "synthetic-source-reviewer", request)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-next-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		intent := competitionAutomationPromotedSourceIntent(t, fixture, seal, review)
		prepared, err := fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, intent)
		if err != nil {
			t.Fatal(err)
		}
		if err = refreshAutomationOperationalMetrics(ctx, fixture.settings, fixture.now); err != nil || testutil.ToFloat64(competitionAutomationPendingSources) != 1 {
			t.Fatalf("pending source metric before fallback: %v", err)
		}
		proof := strings.Repeat("9", 64)
		if err = fixture.store.AbandonSourceIntent(ctx, fixture.settings, *lease, 2, "publication_failed", proof); err != nil {
			t.Fatal(err)
		}
		if err = fixture.store.AbandonSourceIntent(ctx, fixture.settings, *lease, 2, "publication_failed", proof); err != nil {
			t.Fatalf("abandonment replay failed: %v", err)
		}
		if err = refreshAutomationOperationalMetrics(ctx, fixture.settings, fixture.now); err != nil || testutil.ToFloat64(competitionAutomationPendingSources) != 0 {
			t.Fatalf("abandoned source remains pending: %v", err)
		}
		abandoned, err := fixture.store.SourceActivation(ctx, fixture.settings, 2)
		if err != nil || abandoned.AbandonedAt == nil || abandoned.State != "abandoned" || abandoned.IntentSha256 != prepared.IntentSha256 {
			t.Fatalf("immutable abandonment history = %+v, error = %v", abandoned, err)
		}
		if _, err = fixture.store.SourceActivationForTarget(ctx, fixture.settings, 2); !errors.Is(err, ErrNotFound) {
			t.Fatalf("abandoned proposal selected again: %v", err)
		}
		if _, err = fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, intent); !errors.Is(err, ErrConflict) {
			t.Fatalf("abandoned proposal replay reauthorized publication: %v", err)
		}
		generation, err := fixture.store.NextSourceActivationGeneration(ctx, fixture.settings)
		if err != nil || generation != 3 {
			t.Fatalf("fallback generation = %d, error = %v", generation, err)
		}
		carry := competitionAutomationSourceIntent(t, fixture, seal, generation)
		carry.Reason = "readiness_fallback"
		fallback, err := fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, carry)
		if err != nil || fallback.SourceGeneration != 0 || fallback.SourceLockSha256 != fixture.settings.AutomationPolicy.SourceLockSha256 {
			t.Fatalf("fallback changed reviewed source: %+v, %v", fallback, err)
		}
		acknowledgeCompetitionAutomationSource(t, fixture, fallback, "api", "worker")
		active, err := fixture.store.ActivateSource(ctx, fixture.settings, *lease, 3, 1)
		if err != nil || active.Generation != 3 || active.ExpectedGeneration != 1 {
			t.Fatalf("fallback activation = %+v, error = %v", active, err)
		}
		if _, err = fixture.store.ActivateSource(ctx, fixture.settings, *lease, 2, 1); !errors.Is(err, ErrConflict) {
			t.Fatalf("abandoned promotion activated after carry-forward: %v", err)
		}
	})
}

// An intervening source-only decision must not erase a reward disqualification or bypass appeal authority.
func TestCompetitionAutomationSourceOnlyReviewCannotRestoreReward(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		addCompetitionAutomationJob(t, fixture, 70)
		addCompetitionAutomationJob(t, fixture, 80)
		fixture.now = fixture.round.RevealAt
		seal, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		request := competitionAutomationReviewRequest(seal, seal.Seal.Entries[0], true)
		rejected, err := fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, "synthetic-reviewer", request)
		if err != nil {
			t.Fatal(err)
		}
		request = competitionAutomationReviewRequest(seal, seal.Seal.Entries[0], false)
		request.ExpectedRevision, request.RewardDisposition = rejected.RewardDecision.Revision, "unchanged"
		if _, err = fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, "synthetic-reviewer", request); err != nil {
			t.Fatal(err)
		}
		request.IdempotencyKey, request.RewardDisposition = "synthetic-invalid-restoration", "approved"
		if _, err = fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, "synthetic-reviewer", request); !errors.Is(err, ErrConflict) {
			t.Fatalf("ordinary approval bypassed explicit appeal: %v", err)
		}
	})
}
