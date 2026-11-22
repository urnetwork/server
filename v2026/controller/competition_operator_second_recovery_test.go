// Synthetic contracts exercise the closed successor without production job
// identities, operator documents, private native inventories or grants.
package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competitionagent"
	"github.com/urnetwork/server/v2026/model"
)

// The predecessor has a real-shaped first claim, fixed source and finite history.
func competitionSecondRecoveryCycleForTest(t testing.TB) model.CompetitionJobReevaluationCycle {
	t.Helper()
	previous := competitionOperatorPatchRetryCycleForTest(t)
	started := previous.AuthorizedAt.Add(time.Second)
	previous.StartedAt = &started
	cycle := competitionRecoveryTestCopy(t, previous)
	cycle.Schema, cycle.Cycle, cycle.StartedAt = 3, 2, nil
	cycle.AuthorizedAt = started.Add(time.Minute)
	cycle.Distributed.OriginalStartedAt, cycle.Distributed.OriginalDeadlineAt = started, started.Add(3*time.Hour)
	cycle.Distributed.OriginalRunId = competitionagent.RunId(cycle.JobId.String(), 1, 2)
	cycle.SecondRecovery = &model.CompetitionOperatorSecondRecovery{Schema: 1, JobId: previous.JobId,
		PreviousReceiptSha256: previous.ReceiptSha256, ApprovalEvidenceSha256: strings.Repeat("8", 64), PreviousRecovery: &previous}
	sealApexReevaluationCycle(t, &cycle)
	if err := model.ValidateCompetitionJobReevaluationCycle(&cycle); err != nil {
		t.Fatal("synthetic second recovery is invalid", err)
	}
	return cycle
}

// No key, schema marker or rehashed evidence may widen the immutable approval.
func TestCompetitionSecondRecoveryRequestRequiresExactApprovedScopeAndCycle(t *testing.T) {
	settings, principal, request := competitionOperatorPatchRetryRequestForTest(t)
	a := &request.Authorization
	settings.secondRecoveryJobIdForTest = a.JobId
	a.Schema, a.ExpectedCycle = 3, 1
	a.Distributed.OriginalRunId = competitionagent.RunId(a.JobId.String(), 1, a.ExpectedAttemptCount)
	a.SecondRecovery = &model.CompetitionOperatorSecondRecovery{Schema: 1, JobId: a.JobId,
		PreviousReceiptSha256: strings.Repeat("7", 64), ApprovalEvidenceSha256: strings.Repeat("8", 64)}
	var terminal map[string]any
	server.Raise(json.Unmarshal(a.ExpectedTerminalSnapshot, &terminal))
	terminal["evaluation_cycle"] = 1
	a.ExpectedTerminalSnapshot, _ = json.Marshal(terminal)
	a.ExpectedTerminalSha256, _ = CompetitionJobReevaluationSnapshotDigest(a.ExpectedTerminalSnapshot)
	request.AuthorizationSha256 = competitionRecoveryTestDigest(t, a)
	if err := validateCompetitionDistributedRecoveryRequest(settings, principal, request); err != nil {
		t.Fatal("exact synthetic successor refused", err)
	}
	for _, change := range []func(*model.CompetitionJobReevaluationAuthorization){
		func(a *model.CompetitionJobReevaluationAuthorization) { a.Schema = 2 },
		func(a *model.CompetitionJobReevaluationAuthorization) { a.ExpectedCycle = 0 },
		func(a *model.CompetitionJobReevaluationAuthorization) { a.ExpectedCycle = 2 },
		func(a *model.CompetitionJobReevaluationAuthorization) {
			a.JobId = server.NewId()
			a.SecondRecovery.JobId = a.JobId
		},
		func(a *model.CompetitionJobReevaluationAuthorization) { a.SecondRecovery.JobId = server.NewId() },
		func(a *model.CompetitionJobReevaluationAuthorization) { a.SecondRecovery.ApprovalEvidenceSha256 = "" },
		func(a *model.CompetitionJobReevaluationAuthorization) { a.SecondRecovery.PreviousReceiptSha256 = "" },
		func(a *model.CompetitionJobReevaluationAuthorization) {
			a.SecondRecovery.PreviousRecovery = &model.CompetitionJobReevaluationCycle{}
		},
		func(a *model.CompetitionJobReevaluationAuthorization) {
			a.Distributed.OriginalRunId = competitionagent.RunId(a.JobId.String(), 0, a.ExpectedAttemptCount)
		},
	} {
		changed := competitionRecoveryTestCopy(t, request)
		change(&changed.Authorization)
		changed.AuthorizationSha256 = competitionRecoveryTestDigest(t, changed.Authorization)
		if validateCompetitionDistributedRecoveryRequest(settings, principal, changed) == nil {
			t.Fatal("changed successor scope acquired recovery authority")
		}
	}
	settings.secondRecoveryJobIdForTest = server.NewId()
	if validateCompetitionDistributedRecoveryRequest(settings, principal, request) == nil {
		t.Fatal("unapproved job acquired a second recovery")
	}
}

// The real companion schema accepts only the finite public chain, never private
// proof, a third cycle, or successor metadata attached to old schemas.
func TestCompetitionSecondRecoveryPublicContractKeepsClosedPriorReceipt(t *testing.T) {
	validate := competitionDistributedRecoveryContract(t)
	cycle := competitionSecondRecoveryCycleForTest(t)
	objectFor := func() map[string]any {
		encoded, err := json.Marshal(cycle)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		server.Raise(json.Unmarshal(encoded, &object))
		return object
	}
	if err := validate(objectFor()); err != nil {
		t.Fatal("real companion contract rejects the closed successor", err)
	}
	for _, change := range []func(map[string]any){
		func(o map[string]any) { o["cycle"] = 3 },
		func(o map[string]any) { o["schema"] = 2 },
		func(o map[string]any) { delete(o, "second_recovery") },
		func(o map[string]any) { o["distributed_proof"] = map[string]any{"synthetic_private": true} },
		func(o map[string]any) {
			o["second_recovery"].(map[string]any)["previous_recovery"].(map[string]any)["schema"] = 3
		},
		func(o map[string]any) {
			o["second_recovery"].(map[string]any)["previous_recovery"].(map[string]any)["cycle"] = 2
		},
		func(o map[string]any) {
			delete(o["second_recovery"].(map[string]any)["previous_recovery"].(map[string]any), "started_at")
		},
		func(o map[string]any) {
			o["second_recovery"].(map[string]any)["previous_recovery"].(map[string]any)["distributed_proof"] = map[string]any{}
		},
	} {
		object := objectFor()
		change(object)
		if validate(object) == nil {
			t.Fatal("real companion contract accepted mixed or private successor authority")
		}
	}
}
