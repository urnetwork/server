// Public consumers accept only explicit patch retry provenance and still
// reject private native proof, mixed infrastructure intent and changed inputs.
package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// This synthetic projection retains the old generic failure verbatim; the
// applicability diagnosis is an independently pinned operator evidence digest.
func competitionOperatorPatchRetryCycleForTest(t testing.TB) model.CompetitionJobReevaluationCycle {
	t.Helper()
	cycle := competitionDistributedRecoveryCycleForTest(t)
	cycle.IncidentClass = model.CompetitionJobReevaluationOperatorRequestedPatchRetry
	cycle.PreviousFailure.Code = "executor_process_failed"
	cycle.Distributed.Reason = model.CompetitionJobReevaluationOperatorRequestedPatchRetry
	cycle.Distributed.OriginalStartedAt = cycle.AuthorizedAt.Add(-time.Minute)
	cycle.Distributed.OriginalDeadlineAt = cycle.Distributed.OriginalStartedAt.Add(3 * time.Hour)
	cycle.Distributed.PatchRetry = &model.CompetitionOperatorRequestedPatchRetry{Schema: 1, PatchSha256: cycle.PatchSha256, ApplicabilityEvidenceSha256: strings.Repeat("b", 64)}
	sealApexReevaluationCycle(t, &cycle)
	if err := model.ValidateCompetitionJobReevaluationCycle(&cycle); err != nil {
		t.Fatal(err)
	}
	return cycle
}

// Model validation binds the unchanged patch and exact original generic code;
// a newly computed receipt digest never broadens this class's eligibility.
func TestCompetitionOperatorPatchRetryCycleRejectsMixedOrRescoredAuthority(t *testing.T) {
	cycle := competitionOperatorPatchRetryCycleForTest(t)
	for _, change := range []func(*model.CompetitionJobReevaluationCycle){
		func(c *model.CompetitionJobReevaluationCycle) { c.Schema = 1 },
		func(c *model.CompetitionJobReevaluationCycle) {
			c.IncidentClass = model.CompetitionJobReevaluationDistributedInfrastructure
		},
		func(c *model.CompetitionJobReevaluationCycle) {
			c.Distributed.Reason = model.CompetitionDistributedReevaluationRetentionBudget
		},
		func(c *model.CompetitionJobReevaluationCycle) { c.Distributed.PatchRetry = nil },
		func(c *model.CompetitionJobReevaluationCycle) {
			c.Distributed.PatchRetry.PatchSha256 = strings.Repeat("9", 64)
		},
		func(c *model.CompetitionJobReevaluationCycle) {
			c.Distributed.PatchRetry.ApplicabilityEvidenceSha256 = ""
		},
		func(c *model.CompetitionJobReevaluationCycle) {
			c.PreviousFailure.Code = "evaluation_time_budget_exhausted"
		},
		func(c *model.CompetitionJobReevaluationCycle) { c.PreviousFailure.Retriable = true },
	} {
		changed := competitionRecoveryTestCopy(t, cycle)
		change(&changed)
		sealApexReevaluationCycle(t, &changed)
		if model.ValidateCompetitionJobReevaluationCycle(&changed) == nil {
			t.Fatal("resealed mixed authority accepted by model")
		}
	}
}

// The companion's real OpenAPI schema validates the separate typed projection,
// including negative mixed labels and private-data controls.
func TestCompetitionOperatorPatchRetryPublicContractKeepsPrivateProofAndInfrastructureSeparate(t *testing.T) {
	validate := competitionDistributedRecoveryContract(t)
	cycle := competitionOperatorPatchRetryCycleForTest(t)
	objectFor := func(cycle model.CompetitionJobReevaluationCycle) map[string]any {
		content, err := json.Marshal(cycle)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		if err := json.Unmarshal(content, &object); err != nil {
			t.Fatal(err)
		}
		return object
	}
	if err := validate(objectFor(cycle)); err != nil {
		t.Fatal("explicit patch retry rejected by real companion contract", err)
	}
	for _, change := range []func(map[string]any){
		func(o map[string]any) {
			o["incident_class"] = model.CompetitionJobReevaluationDistributedInfrastructure
		},
		func(o map[string]any) {
			o["distributed"].(map[string]any)["reason"] = model.CompetitionDistributedReevaluationRetentionBudget
		},
		func(o map[string]any) { delete(o["distributed"].(map[string]any), "patch_retry") },
		func(o map[string]any) { o["distributed_proof"] = map[string]any{"synthetic_private_inventory": true} },
	} {
		object := objectFor(cycle)
		change(object)
		if validate(object) == nil {
			t.Fatal("mixed/private patch retry accepted by real public contract")
		}
	}
	settings, principal, request := competitionDistributedRecoveryRequestForTest(t)
	if validateCompetitionDistributedRecoveryRequest(settings, principal, request) != nil {
		t.Fatal("old infrastructure request changed eligibility")
	}
	for _, value := range []any{request.Authorization, competitionDistributedRecoveryCycleForTest(t)} {
		content, err := json.Marshal(value)
		if err != nil || strings.Contains(string(content), "patch_retry") {
			t.Fatal("optional patch metadata changed old canonical bytes", err)
		}
	}
}
