package controller

import (
	"encoding/json"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competitionagent"
	"github.com/urnetwork/server/model"
)

func TestCompetitionSilleStorageRecoveryPublicReason(t *testing.T) {
	cycle := competitionDistributedRecoveryCycleForTest(t)
	cycle.Distributed.Reason = model.CompetitionSilleStorageRecoveryReason
	raw, err := json.Marshal(cycle)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	if err := competitionDistributedRecoveryContract(t)(object); err != nil {
		t.Fatalf("public recovery reason is missing from the contract: %v", err)
	}
}

func TestCompetitionSilleStorageRecoveryExactScope(t *testing.T) {
	fixture := func() model.CompetitionJobReevaluationAuthorization {
		job, err := server.ParseId("01a1e738-6c64-357a-8d4f-143e4b3364d3")
		if err != nil {
			t.Fatal(err)
		}
		round, err := server.ParseId("01a1c110-585b-ce38-4f29-ca4860a1c058")
		if err != nil {
			t.Fatal(err)
		}
		return model.CompetitionJobReevaluationAuthorization{
			Environment: "main", CompetitionId: "sim-latency-season-1",
			SeasonId: "sim-latency-production-20261028-r2", JobId: job, RoundId: round,
			Schema: 2, ExpectedAttemptCount: 1, IncidentAttempt: 1, SingleUse: true,
			PostCloseApproval: &model.CompetitionJobReevaluationPostCloseApproval{},
			Distributed: &model.CompetitionDistributedJobReevaluationAuthorization{
				Reason: "sille_storage_interruption_20261117", OriginalHostId: "sille",
				OriginalRunId:         "35e95bdc8f4db995113463b989323b19be5f5f4681b375ca7d8f731a35eece9b",
				ClosureEvidenceSha256: "3fc5cbab1e3e2ad6c7c85929f94d67ba599be415c5521ceb9e54bb8c59ef15e0",
			},
		}
	}
	if !competitionSilleStorageRecoveryScope(fixture()) {
		t.Fatal("the exact scope was rejected")
	}
	for name, mutate := range map[string]func(*model.CompetitionJobReevaluationAuthorization){
		"job":        func(a *model.CompetitionJobReevaluationAuthorization) { a.JobId = a.RoundId },
		"round":      func(a *model.CompetitionJobReevaluationAuthorization) { a.RoundId = a.JobId },
		"cycle":      func(a *model.CompetitionJobReevaluationAuthorization) { a.ExpectedCycle++ },
		"attempt":    func(a *model.CompetitionJobReevaluationAuthorization) { a.ExpectedAttemptCount++ },
		"host":       func(a *model.CompetitionJobReevaluationAuthorization) { a.Distributed.OriginalHostId = "goofe" },
		"run":        func(a *model.CompetitionJobReevaluationAuthorization) { a.Distributed.OriginalRunId = "" },
		"closure":    func(a *model.CompetitionJobReevaluationAuthorization) { a.Distributed.ClosureEvidenceSha256 = "" },
		"postclose":  func(a *model.CompetitionJobReevaluationAuthorization) { a.PostCloseApproval = nil },
		"single use": func(a *model.CompetitionJobReevaluationAuthorization) { a.SingleUse = false },
	} {
		t.Run(name, func(t *testing.T) {
			a := fixture()
			mutate(&a)
			if competitionSilleStorageRecoveryScope(a) {
				t.Fatal("altered recovery scope was accepted")
			}
		})
	}
}

func TestCompetitionDistributedRecoveryTerminalMatchesOnlyItsReason(t *testing.T) {
	reasons := []string{
		model.CompetitionDistributedReevaluationRetentionBudget,
		model.CompetitionDistributedReevaluationLegacyPrelaunch,
		model.CompetitionJobReevaluationOperatorRequestedPatchRetry,
		competitionSilleStorageRecoveryReason,
		"unknown",
	}
	responses := []competitionagent.Response{
		{State: "succeeded", ExitCode: 0},
		{State: "failed", ExitCode: -1, ErrorCode: "legacy_prelaunch_infrastructure_failure"},
		{State: "failed", ExitCode: 1, ErrorCode: "evaluator_process_failed"},
		{State: "expired", ExitCode: 1, ErrorCode: "original_deadline_elapsed"},
	}
	for i, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			for j, response := range responses {
				want := i == j && i < len(responses)
				if got := competitionDistributedRecoveryTerminalMatches(reason, response); got != want {
					t.Errorf("terminal tuple %d: got %t, want %t", j, got, want)
				}
			}
		})
	}
}
