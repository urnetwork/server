// A new-write rule must not alter historical rows or strand their exact replay.
package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The real transaction rejects a missing new addendum, preserves a seeded
// historical receipt, and persists the explicit identity on a new review.
func TestCompetitionProductionBenefitReviewNewWritesAndHistoricalReplay(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		addCompetitionAutomationJob(t, fixture, 70)
		fixture.now = fixture.round.RevealAt
		sealed, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		request := competitionAutomationReviewRequest(sealed, sealed.Seal.Entries[0], false)
		request.ProductionBenefitAddendum = nil
		if _, err := fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, "synthetic-reviewer", request); err == nil || !strings.Contains(err.Error(), "production-benefit") {
			t.Fatal("new review without eligibility addendum accepted", err)
		}
		digest, err := model.CompetitionAutomationDigest(request)
		if err != nil {
			t.Fatal(err)
		}
		historical := model.CompetitionIntegrityReviewResult{ReviewId: server.NewId(), RoundId: fixture.round.RoundId, ActorId: "synthetic-reviewer", CreatedAt: fixture.now,
			Request: request, RequestSha256: digest, RewardDecision: sealed.Decisions[len(sealed.Decisions)-1]}
		content, err := json.Marshal(historical)
		if err != nil {
			t.Fatal(err)
		}
		// This fixture represents an already persisted pre-addendum request,
		// not a compatibility bypass in the new application write path.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_v2_review_actor',$1,true)`, historical.ActorId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_integrity_review(review_id,round_id,job_id,actor_id,idempotency_key,request_sha256,review_json,created_at) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8)`,
				historical.ReviewId, historical.RoundId, request.JobId, historical.ActorId, request.IdempotencyKey, digest, string(content), historical.CreatedAt))
		})
		replay, err := fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, historical.ActorId, request)
		if err != nil || replay.ReviewId != historical.ReviewId || replay.RequestSha256 != digest || replay.Request.ProductionBenefitAddendum != nil {
			t.Fatal("historical exact replay was rewritten or stranded", err)
		}
		request.IdempotencyKey = "synthetic-new-review"
		request.ProductionBenefitAddendum = model.CompetitionProductionBenefitReviewAddendum()
		created, err := fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, historical.ActorId, request)
		if err != nil || created.Request.ProductionBenefitAddendum == nil || *created.Request.ProductionBenefitAddendum != *request.ProductionBenefitAddendum {
			t.Fatal("new receipt lost bound eligibility addendum", err)
		}
	})
}
