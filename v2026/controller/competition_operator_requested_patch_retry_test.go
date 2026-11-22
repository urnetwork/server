// Synthetic controls prove that operator intent, original metadata and native
// closure are independent requirements; no production patch or receipt is used.
package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competitionagent"
	"github.com/urnetwork/server/v2026/model"
)

// A fully synthetic terminal snapshot binds the same paid job and unchanged
// patch while preserving the native/DB generic failure code distinction.
func competitionOperatorPatchRetryRequestForTest(t testing.TB) (*Settings, *Principal, model.CompetitionJobReevaluationRequest) {
	t.Helper()
	settings, principal, request := competitionDistributedRecoveryRequestForTest(t)
	a := &request.Authorization
	a.Kind, a.IncidentClass = model.CompetitionOperatorRequestedPatchRetryAuthorizationKind, model.CompetitionJobReevaluationOperatorRequestedPatchRetry
	a.Distributed.Reason = model.CompetitionJobReevaluationOperatorRequestedPatchRetry
	a.Distributed.OriginalStartedAt = a.AuthorizedAt.Add(-time.Minute)
	a.Distributed.OriginalDeadlineAt = a.Distributed.OriginalStartedAt.Add(3 * time.Hour)
	a.Distributed.PatchRetry = &model.CompetitionOperatorRequestedPatchRetry{Schema: 1, PatchSha256: strings.Repeat("a", 64), ApplicabilityEvidenceSha256: strings.Repeat("b", 64)}
	terminal, err := json.Marshal(map[string]any{"job_id": a.JobId, "round_id": a.RoundId, "patch_sha256": a.Distributed.PatchRetry.PatchSha256,
		"state": "failed", "evaluation_cycle": 0, "attempt_count": a.ExpectedAttemptCount,
		"eval_error_json": model.CompetitionEvaluationFailure{Kind: "infrastructure", Code: "executor_process_failed"}})
	if err != nil {
		t.Fatal(err)
	}
	a.ExpectedTerminalSnapshot = terminal
	a.ExpectedTerminalSha256, err = CompetitionJobReevaluationSnapshotDigest(terminal)
	if err != nil {
		t.Fatal(err)
	}
	request.AuthorizationSha256 = competitionRecoveryTestDigest(t, a)
	return settings, principal, request
}

// Resealing malformed requests cannot turn another class, patch or original
// attempt into the operator's specific unchanged-patch retry authority.
func TestCompetitionOperatorPatchRetryRequestRequiresExactSeparateScope(t *testing.T) {
	settings, principal, request := competitionOperatorPatchRetryRequestForTest(t)
	if err := validateCompetitionDistributedRecoveryRequest(settings, principal, request); err != nil {
		t.Fatal("explicit synthetic unchanged-patch retry refused", err)
	}
	for _, change := range []struct {
		name   string
		mutate func(*model.CompetitionJobReevaluationAuthorization)
	}{
		{name: "infrastructure-kind", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.Kind = model.CompetitionDistributedJobReevaluationAuthorizationKind
		}},
		{name: "infrastructure-class", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.IncidentClass = model.CompetitionJobReevaluationDistributedInfrastructure
		}},
		{name: "infrastructure-reason", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.Distributed.Reason = model.CompetitionDistributedReevaluationRetentionBudget
		}},
		{name: "missing-patch-evidence", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.Distributed.PatchRetry = nil }},
		{name: "empty-patch-evidence", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.Distributed.PatchRetry.ApplicabilityEvidenceSha256 = ""
		}},
		{name: "different-patch", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.Distributed.PatchRetry.PatchSha256 = strings.Repeat("c", 64)
		}},
		{name: "different-round", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.RoundId = server.NewId() }},
		{name: "different-job", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.JobId = server.NewId() }},
		{name: "different-attempt", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.ExpectedAttemptCount++ }},
		{name: "another-cycle", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.ExpectedCycle = 1 }},
		{name: "not-single-use", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.SingleUse = false }},
	} {
		changed := competitionRecoveryTestCopy(t, request)
		change.mutate(&changed.Authorization)
		changed.AuthorizationSha256 = competitionRecoveryTestDigest(t, changed.Authorization)
		if err := validateCompetitionDistributedRecoveryRequest(settings, principal, changed); err == nil {
			t.Errorf("%s acquired patch retry authority", change.name)
		}
	}
	scheduler := *principal
	scheduler.Role = competitionSchedulerRole
	if err := validateCompetitionDistributedRecoveryRequest(settings, &scheduler, request); err == nil {
		t.Fatal("scheduler acquired operator-only unchanged-patch retry")
	}
	changed := competitionRecoveryTestCopy(t, request)
	changed.Authorization.Distributed.PatchRetry.ApplicabilityEvidenceSha256 = strings.Repeat("f", 64)
	if err := validateCompetitionDistributedRecoveryRequest(settings, principal, changed); err == nil {
		t.Fatal("changed evidence reused the original authorization digest")
	}
}

// Infrastructure qualification may name the same binary, but its capability
// cannot be borrowed for the separate operator-requested patch action.
func TestCompetitionOperatorPatchRetryRuntimeRequiresDistinctQualification(t *testing.T) {
	proof := model.CompetitionJobReevaluationRuntime{Schema: 2, Kind: model.CompetitionOperatorRequestedPatchRetryRuntimeKind,
		Capability: model.CompetitionOperatorRequestedPatchRetryCapability, IncidentClasses: []string{model.CompetitionJobReevaluationOperatorRequestedPatchRetry},
		Accepted: true, SourceCommit: strings.Repeat("a", 40), WorkerSha256: strings.Repeat("b", 64), ApiImageDigest: "sha256:" + strings.Repeat("c", 64),
		WorkerImageDigest: "sha256:" + strings.Repeat("d", 64), EvidenceSha256: strings.Repeat("e", 64), ApiCompatible: true, WorkerCompatible: true}
	settings := automationApiSettings()
	if err := BindCompetitionJobReevaluationRuntime(settings, proof, competitionRecoveryTestDigest(t, proof)); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*model.CompetitionJobReevaluationRuntime){
		func(p *model.CompetitionJobReevaluationRuntime) {
			p.Kind = model.CompetitionDistributedJobReevaluationRuntimeKind
		},
		func(p *model.CompetitionJobReevaluationRuntime) {
			p.Capability = model.CompetitionDistributedJobReevaluationCapability
		},
		func(p *model.CompetitionJobReevaluationRuntime) {
			p.IncidentClasses = []string{model.CompetitionJobReevaluationDistributedInfrastructure}
		},
		func(p *model.CompetitionJobReevaluationRuntime) { p.WorkerCompatible = false },
	} {
		changed := competitionRecoveryTestCopy(t, proof)
		change(&changed)
		if err := BindCompetitionJobReevaluationRuntime(settings, changed, competitionRecoveryTestDigest(t, changed)); err == nil {
			t.Fatal("different runtime authority qualified a patch retry")
		}
	}
}

// A signed generic process failure is only closure. The explicit evidence and
// terminal binding above are still required; no start, cancel or transfer occurs.
func TestCompetitionOperatorPatchRetryNativeClosureKeepsGenericFailureAndReadOnlyBarrier(t *testing.T) {
	settings, _, request := competitionOperatorPatchRetryRequestForTest(t)
	round := &roundRecord{RoundResult: RoundResult{RoundId: request.Authorization.RoundId, Epoch: 2}}
	calls := 0
	factory := func(_ *Settings, host CompetitionExecutorHostPolicy) (competitionAgentTransport, error) {
		return competitionDistributedRecoveryTestTransport{t: t, host: host, auth: request.Authorization, round: round, calls: &calls}, nil
	}
	proof, err := observeCompetitionDistributedRecovery(context.Background(), settings, round, request, factory)
	if err != nil || proof == nil || calls != 3 || proof.OriginalStatus.ErrorCode != "evaluator_process_failed" {
		t.Fatal("exact closed old-run/all-host observation failed", err, calls)
	}
	for _, change := range []func(*competitionagent.Response){
		func(r *competitionagent.Response) {
			if r.Identity != nil {
				r.State, r.ExitCode, r.ErrorCode = "succeeded", 0, ""
			}
		},
		func(r *competitionagent.Response) {
			if r.Identity != nil {
				r.ErrorCode = "executor_process_failed"
			}
		},
		func(r *competitionagent.Response) {
			if r.Identity != nil {
				r.Identity.Attempt++
			}
		},
		func(r *competitionagent.Response) {
			if r.Identity == nil {
				r.State = "occupied"
			}
		},
	} {
		factory := func(_ *Settings, host CompetitionExecutorHostPolicy) (competitionAgentTransport, error) {
			return competitionDistributedRecoveryTestTransport{t: t, host: host, auth: request.Authorization, round: round, mutate: change}, nil
		}
		if proof, err := observeCompetitionDistributedRecovery(context.Background(), settings, round, request, factory); err == nil || proof != nil {
			t.Fatal("unclosed/substituted/busy native run passed patch retry barrier")
		}
	}
}
