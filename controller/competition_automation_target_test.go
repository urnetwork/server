// A reserved future round permits real readiness without prematurely accepting submissions.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Source readiness for another UUID cannot qualify the reserved future admission window.
func TestCompetitionAutomationReadinessBindsReservedRound(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		fixture.now = fixture.round.RevealAt
		seal, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-next-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		intent := competitionAutomationSourceIntent(t, fixture, seal, 2)
		missing := intent
		missing.TargetRoundId = server.Id{}
		if _, err = fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, missing); !errors.Is(err, ErrConflict) {
			t.Fatalf("source intent did not reserve a round: %v", err)
		}
		stored, err := fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, intent)
		if err != nil {
			t.Fatal(err)
		}
		wrong := model.CompetitionSourceReadiness{Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId, Generation: 2,
			TargetRoundId: server.NewId(), Role: "api", ActorId: "synthetic-api", ManifestCommit: intent.ManifestCommit, ManifestSha256: intent.ManifestSha256,
			SourceLockSha256: intent.SourceLockSha256, RuntimeImageDigest: testApiImageDigest(), QualificationSha256: intent.HostQualificationSha256, Ready: true, RecordedAt: fixture.now}
		if err = fixture.store.RecordSourceReadiness(ctx, fixture.settings, wrong); !errors.Is(err, ErrConflict) {
			t.Fatalf("readiness accepted another prospective round: %v", err)
		}
		content, err := json.Marshal(wrong)
		if err != nil {
			t.Fatal(err)
		}
		databaseErr := captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_source_readiness(environment,competition_id,season_id,generation,role,receipt_json,created_at) VALUES($1,$2,$3,2,'api',$4::jsonb,$5)`,
					intent.Environment, intent.CompetitionId, intent.SeasonId, string(content), fixture.now))
			})
		})
		if databaseErr == nil || !strings.Contains(databaseErr.Error(), "prospectively reserved round") {
			t.Fatalf("SQL readiness guard missed a different target: %v", databaseErr)
		}
		if err = fixture.store.SourcePreparationReady(ctx, fixture.settings, 2); !errors.Is(err, ErrReviewNotReady) {
			t.Fatalf("missing receipts were treated as successful preparation: %v", err)
		}
		acknowledgeCompetitionAutomationSource(t, fixture, stored, "api", "worker")
		if err = fixture.store.SourcePreparationReady(ctx, fixture.settings, 2); err != nil {
			t.Fatal(err)
		}
		if _, err = fixture.store.ActivateSource(ctx, fixture.settings, *lease, 2, 1); err != nil {
			t.Fatal(err)
		}
		fixture.now = stored.PreparedAt.Add(16 * time.Hour)
		lease, err = fixture.store.AcquireAutomationLease(ctx, fixture.settings, lease.OwnerId, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		qualified, err := sourceSettingsFromIntent(fixture.settings, stored)
		if err != nil {
			t.Fatal(err)
		}
		qualified.automationTargetRoundId = server.NewId()
		wrongRound, err := fixture.store.prepareRound(ctx, qualified, GenerateRoundArgs{OpensAt: fixture.now, ClosesAt: fixture.now.Add(7 * 24 * time.Hour), RevealAt: fixture.now.Add(7 * 24 * time.Hour)}, false)
		if err != nil {
			t.Fatal(err)
		}
		databaseErr = captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(fixture.store.requireAutomationLeaseInTx(ctx, tx, fixture.settings, *lease))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_round(round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled)
			 VALUES($1,$2,2,false,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11,$12,false)`, wrongRound.RoundId, wrongRound.CompetitionId, wrongRound.WorkloadCommitment, wrongRound.SeedNonce, wrongRound.SeedCiphertext,
					wrongRound.ProvidersSha256, wrongRound.ProvidersPath, string(wrongRound.PolicyJson), wrongRound.OpensAt, wrongRound.ClosesAt, wrongRound.RevealAt, wrongRound.CreatedAt))
			})
		})
		if databaseErr == nil || !strings.Contains(databaseErr.Error(), "reserved before source preparation") {
			t.Fatalf("SQL round guard accepted an unqualified UUID: %v", databaseErr)
		}
		created, err := fixture.store.CreateAutomationRound(ctx, fixture.settings, *lease, 2, fixture.now)
		if err != nil || created.RoundId != stored.TargetRoundId {
			t.Fatalf("reserved round did not open: %+v, %v", created, err)
		}
	})
}

// A reward appeal alone cannot revive the older source approval or activate an already prepared intent.
func TestCompetitionAutomationAppealDoesNotResurrectSourceApproval(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		addCompetitionAutomationJob(t, fixture, 80)
		fixture.now = fixture.round.RevealAt
		seal, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		approval := competitionAutomationReviewRequest(seal, seal.Seal.Entries[0], false)
		approval.SourceDisposition = "approved"
		review, err := fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, "synthetic-reviewer", approval)
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
		acknowledgeCompetitionAutomationSource(t, fixture, prepared, "api", "worker")
		disqualification := competitionAutomationReviewRequest(seal, seal.Seal.Entries[0], true)
		disqualification.SourceDisposition = "unchanged"
		rejected, err := fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, "synthetic-reviewer", disqualification)
		if err != nil {
			t.Fatal(err)
		}
		appeal := competitionAutomationReviewRequest(seal, seal.Seal.Entries[0], false)
		appeal.ExpectedRevision, appeal.RewardDisposition, appeal.SourceDisposition = rejected.RewardDecision.Revision, "restored", "unchanged"
		appeal.AppealId, appeal.SupersedesReviewId, appeal.ReasonCodes = "synthetic-appeal", &rejected.ReviewId, []string{"APPEAL_UPHELD"}
		if _, err = fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, "synthetic-appeal-authority", appeal); err != nil {
			t.Fatal(err)
		}
		if _, err = fixture.store.ReviewedSource(ctx, fixture.settings, review.ReviewId); err == nil {
			t.Fatal("private source packet resurrected a revoked approval")
		}
		if _, err = fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, intent); err == nil {
			t.Fatal("source publication replay resurrected a revoked approval")
		}
		if _, err = fixture.store.ActivateSource(ctx, fixture.settings, *lease, 2, 1); err == nil {
			t.Fatal("source activation resurrected a revoked approval")
		}
		databaseErr := captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(fixture.store.requireAutomationLeaseInTx(ctx, tx, fixture.settings, *lease))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_source_activation SET activated_at=$4 WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=2`,
					prepared.Environment, prepared.CompetitionId, prepared.SeasonId, fixture.now))
			})
		})
		if databaseErr == nil || !strings.Contains(databaseErr.Error(), "fresh explicit source approval") {
			t.Fatalf("SQL activation resurrected the old source approval: %v", databaseErr)
		}
	})
}

// The private prospective identity is ignored for legacy staging and rejected if absent in v2.
func TestCompetitionAutomationPreparationReservesIdentityBeforeArtifacts(t *testing.T) {
	settings := automationApiSettings()
	settings.ArtifactRoot = t.TempDir()
	store := PostgresStore{}
	args := GenerateRoundArgs{OpensAt: server.NowUtc(), ClosesAt: server.NowUtc().Add(7 * 24 * time.Hour), RevealAt: server.NowUtc().Add(7 * 24 * time.Hour)}
	if _, err := store.prepareRound(context.Background(), settings, args, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("v2 preparation without reserved identity: %v", err)
	}
	settings.automationTargetRoundId = server.NewId()
	first, err := store.prepareRound(context.Background(), settings, args, false)
	if err != nil || first.RoundId != settings.automationTargetRoundId {
		t.Fatalf("v2 artifact identity mismatch: %+v, %v", first, err)
	}
	settings.AutomationPolicy = model.CompetitionAutomationPolicy{}
	legacy, err := store.prepareRound(context.Background(), settings, args, true)
	if err != nil || legacy.RoundId == settings.automationTargetRoundId {
		t.Fatalf("legacy staging reused a prospective v2 identity: %+v, %v", legacy, err)
	}
}
