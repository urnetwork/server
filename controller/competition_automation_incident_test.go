// Source incidents follow exact content ancestry even with deferred approval,
// repeated carry-forward, and unrelated later generations.
package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Numeric generation ordering alone must not taint an unrelated reviewed base.
func TestCompetitionAutomationIncidentFollowsExactSourceAncestry(t *testing.T) {
	base := strings.Repeat("a", 64)
	compromised := strings.Repeat("b", 64)
	child := strings.Repeat("c", 64)
	unrelated := strings.Repeat("d", 64)
	intents := []model.CompetitionSourceActivation{
		{Generation: 5, BaseSourceLockSha256: child, SourceLockSha256: child},
		{Generation: 4, BaseSourceLockSha256: compromised, SourceLockSha256: child},
		{Generation: 3, BaseSourceLockSha256: base, SourceLockSha256: unrelated},
		{Generation: 2, BaseSourceLockSha256: base, SourceLockSha256: compromised},
		{Generation: 1, BaseSourceLockSha256: base, SourceLockSha256: base},
	}
	if got := affectedAutomationSourceGenerations(intents, compromised); !slices.Equal(got, []int64{2, 4, 5}) {
		t.Fatalf("affected generations = %v, want exact compromised lineage", got)
	}
}

// Empty and unknown source ancestry cannot manufacture an affected activation.
func TestCompetitionAutomationIncidentDoesNotInferUnrelatedDescendants(t *testing.T) {
	intents := []model.CompetitionSourceActivation{{
		Generation: 10, BaseSourceLockSha256: strings.Repeat("a", 64), SourceLockSha256: strings.Repeat("b", 64),
	}}
	if got := affectedAutomationSourceGenerations(intents, strings.Repeat("c", 64)); len(got) != 0 {
		t.Fatalf("unrelated generation marked compromised: %v", got)
	}
}

// Independent pauses remain auditable and usable during a rollback that turns
// off new automation. Replays cannot create duplicate revisions or change scope.
func TestCompetitionAutomationHoldsAreScopedAndRecoverableWhenDisabled(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		args := model.CompetitionAutomationHoldArgs{
			Kind: "admission", Active: true, IdempotencyKey: "synthetic-admission-hold",
			ReasonCode: "OPERATOR_HOLD", PublicExplanation: "Synthetic archive investigation.", EvidenceSha256: strings.Repeat("a", 64),
		}
		hold, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-operator", args)
		if err != nil || hold.Revision != 1 {
			t.Fatalf("hold = %+v, error = %v", hold, err)
		}
		fixture.settings.AutomationPolicy.Enabled = false
		replay, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-operator", args)
		if err != nil || replay.HoldId != hold.HoldId {
			t.Fatalf("disabled recovery replay = %+v, error = %v", replay, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			if err := ensureAutomationNotHeldInTx(ctx, tx, fixture.settings, "admission"); !errors.Is(err, ErrAutomationHeld) {
				t.Fatalf("admission not paused: %v", err)
			}
			for _, kind := range []string{"evaluation", "source_activation", "reward_delivery"} {
				if err := ensureAutomationNotHeldInTx(ctx, tx, fixture.settings, kind); err != nil {
					t.Fatalf("unrelated capability %s paused: %v", kind, err)
				}
			}
		})
		conflict := args
		conflict.Kind = "evaluation"
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-operator", conflict); !errors.Is(err, ErrConflict) {
			t.Fatalf("key reused for a different pause = %v", err)
		}
		args.Active, args.ExpectedRevision, args.IdempotencyKey, args.ReasonCode = false, 1, "synthetic-recovery", "RECOVERY_APPROVED"
		release, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-operator", args)
		if err != nil || release.Revision != 2 {
			t.Fatalf("disabled recovery = %+v, error = %v", release, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			if err := ensureAutomationNotHeldInTx(ctx, tx, fixture.settings, "admission"); err != nil {
				t.Fatalf("explicit recovery did not release admission: %v", err)
			}
		})
	})
}

// Invalidation adds a distinct decision/event without erasing a previously
// elected recipient, editing measurements, or disguising the result as no winner.
func TestCompetitionAutomationIncidentInvalidatesWithoutRewritingEvidence(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixture(t)
		addCompetitionAutomationJob(t, fixture, 70)
		fixture.now = fixture.round.RevealAt
		original, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || original.ProvisionalRewardWinner == nil {
			t.Fatalf("initial election = %+v, error = %v", original, err)
		}
		args := model.CompetitionAutomationIncidentArgs{
			IdempotencyKey: "synthetic-invalid-evidence", ExpectedRewardRevision: 1, InvalidatesEvaluation: true,
			ReasonCode: "EVALUATION_INTEGRITY_INCIDENT", PublicExplanation: "Synthetic archived evidence failed authentication.", EvidenceSha256: strings.Repeat("b", 64),
		}
		incident, err := fixture.store.RecordAutomationIncident(ctx, fixture.settings, fixture.round.RoundId, "synthetic-operator", args)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := fixture.store.RecordAutomationIncident(ctx, fixture.settings, fixture.round.RoundId, "synthetic-operator", args)
		if err != nil || replay.IncidentId != incident.IncidentId {
			t.Fatalf("incident replay = %+v, error = %v", replay, err)
		}
		result, err := fixture.store.AutomationResult(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || !result.Invalidated || len(result.Incidents) != 1 || len(result.Decisions) != 2 {
			t.Fatalf("incident result = %+v, error = %v", result, err)
		}
		if result.Seal.SealSha256 != original.Seal.SealSha256 || result.Decisions[0].DecisionSha256 != original.Decisions[0].DecisionSha256 ||
			result.ProvisionalRewardWinner != nil || result.Decisions[1].Action != "invalidated" {
			t.Fatal("invalidation rewrote history or resembled ordinary no improvement")
		}
		page, err := fixture.store.RewardEvents(ctx, fixture.settings, 0, 200)
		if err != nil || len(page.Events) != 2 || page.Events[1].Decision.Action != "invalidated" {
			t.Fatalf("invalidation delivery = %+v, error = %v", page, err)
		}
		approval := competitionAutomationReviewRequest(result, result.Seal.Entries[0], false)
		if _, err := fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, "synthetic-reviewer", approval); !errors.Is(err, ErrEvaluationInvalidated) {
			t.Fatalf("ordinary review restored invalid evidence: %v", err)
		}
	})
}
