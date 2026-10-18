// Explicit barriers exercise concurrent elections; immutable evidence and
// legacy-worker fences are checked through the real PostgreSQL guards.
package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Two independent supervisors must return the same committed seal/revision,
// even when both cross the close/drain boundary together.
func TestCompetitionAutomationConcurrentSealsProduceOneElection(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		addCompetitionAutomationJob(t, fixture, 70)
		fixture.now = fixture.round.RevealAt
		type sealOutcome struct {
			result *model.CompetitionV2RoundResult
			err    error
		}
		ready := make(chan struct{}, 2)
		start := make(chan struct{})
		outcomes := make(chan sealOutcome, 2)
		for range 2 {
			go func() {
				ready <- struct{}{}
				<-start
				result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
				outcomes <- sealOutcome{result: result, err: err}
			}()
		}
		<-ready
		<-ready
		close(start)
		first, second := <-outcomes, <-outcomes
		if first.err != nil || second.err != nil {
			t.Fatalf("concurrent seals failed: %v, %v", first.err, second.err)
		}
		if first.result.Seal.SealSha256 != second.result.Seal.SealSha256 ||
			len(first.result.Decisions) != 1 || len(second.result.Decisions) != 1 ||
			first.result.Decisions[0].DecisionSha256 != second.result.Decisions[0].DecisionSha256 {
			t.Fatal("concurrent seal created competing identities or reward revisions")
		}
		page, err := fixture.store.RewardEvents(ctx, fixture.settings, 0, 200)
		if err != nil || len(page.Events) != 1 {
			t.Fatalf("concurrent election emitted duplicate events: %+v, %v", page, err)
		}
	})
}

// A sealed result cannot be rewritten even by a stale internal program that
// does not understand the new publication marker.
func TestCompetitionAutomationDatabaseRejectsSealedEvidenceMutation(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		job := addCompetitionAutomationJob(t, fixture, 70)
		fixture.now = fixture.round.RevealAt
		original, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{
			`UPDATE competition_job SET score_json=jsonb_set(score_json,'{raw_score}','1'::jsonb) WHERE job_id=$1`,
			`DELETE FROM competition_job WHERE job_id=$1`,
		} {
			var mutationErr error
			server.Db(ctx, func(conn server.PgConn) {
				_, mutationErr = conn.Exec(ctx, statement, job.JobId)
			}, server.OptReadWrite(), server.OptNoRetry())
			if mutationErr == nil {
				t.Fatal("database accepted a rewrite of sealed evidence")
			}
		}
		current, err := fixture.store.AutomationResult(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || current.Seal.SealSha256 != original.Seal.SealSha256 {
			t.Fatalf("evidence changed: %+v, %v", current, err)
		}
		if _, err := fixture.store.RequirePromotionDecision(ctx, fixture.settings, 1, &job.JobId); !errors.Is(err, ErrUnsupportedDecisionMode) {
			t.Fatalf("legacy source promotion bypassed v2 approval: %v", err)
		}
	})
}

// A deferred commit fault and an explicit competing writer force the retry to
// find an already-recorded receipt. Per-attempt timestamps must not become
// request content and turn an exact replay into an idempotency conflict.
func TestCompetitionAutomationReceiptRetryDoesNotRetainAttemptTimestamp(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		addCompetitionAutomationJob(t, fixture, 70)
		fixture.now = fixture.round.RevealAt
		if _, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId); err != nil {
			t.Fatal(err)
		}
		page, err := fixture.store.RewardEvents(ctx, fixture.settings, 0, 200)
		if err != nil || len(page.Events) != 1 {
			t.Fatalf("event fixture = %+v, %v", page, err)
		}
		event := page.Events[0]
		receipt := model.CompetitionSettlementReceipt{
			EventId: event.EventId, Environment: event.Decision.Environment, CompetitionId: event.Decision.CompetitionId,
			SeasonId: event.Decision.SeasonId, RoundId: event.Decision.RoundId, Revision: event.Decision.Revision,
			PayloadSha256: event.PayloadSha256, DecisionSha256: event.Decision.DecisionSha256,
			Status: "received", AccountingReference: "synthetic-durable-receipt",
		}
		armCompetitionCommitFault(ctx, "automation_receipt_retry", "competition_settlement_receipt", "INSERT", "true")
		var competing *model.CompetitionSettlementReceipt
		competitor := &rerunCompetitor{compete: func() error {
			var err error
			competing, err = fixture.store.RecordSettlementReceipt(ctx, fixture.settings, "synthetic-settlement", receipt)
			return err
		}}
		saved, err := fixture.store.RecordSettlementReceipt(competitor.context(ctx), fixture.settings, "synthetic-settlement", receipt)
		competitor.require(t)
		if err != nil || saved == nil || competing == nil || !saved.ReceivedAt.Equal(competing.ReceivedAt) {
			t.Fatalf("receipt retry did not replay the committed receipt: saved=%+v, competing=%+v, error=%v", saved, competing, err)
		}
		if got := competitionCommitFaultCount(ctx, "automation_receipt_retry"); got != 2 {
			t.Fatalf("receipt insert attempts = %d, want rolled-back original plus competing commit", got)
		}
	})
}
