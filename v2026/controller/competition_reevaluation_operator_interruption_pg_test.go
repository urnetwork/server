// A real stale claim reaches its original terminal budget before the public
// operator path grants one separately fenced cycle. No history is fabricated.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Advance the disposable owner's clock across an unrenewed lease and original
// deadline, then use the actual claim and completion paths, never a row reset.
func newCompetitionOperatorInterruptionPgFixture(t testing.TB, ctx context.Context) (*competitionReevaluationAuthorizationPgFixture, map[string]any) {
	t.Helper()
	f := &competitionReevaluationAuthorizationPgFixture{competitionRankingFixture: newCompetitionAutomationFixture(t)}
	addCompetitionAutomationJob(t, f.competitionRankingFixture, 80)
	lease, err := f.store.AcquireAutomationLease(ctx, f.settings, "synthetic-reevaluation-supervisor", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.settings.automationLease, f.settings.automationRoundId = lease, f.round.RoundId
	patch, patchError := ValidateAndCanonicalizePatch(testPatch("synthetic-operator-interruption"), f.settings.PatchPolicy)
	if patchError != nil {
		t.Fatal(patchError)
	}
	if _, _, err := f.store.Enqueue(ctx, f.settings, f.round.RoundId, patch, "synthetic-reevaluation-submitter", testApiImageDigest()); err != nil {
		t.Fatal(err)
	}
	f.job, err = f.store.Claim(ctx, f.settings, "synthetic-reevaluation-worker", testWorkerImageDigest())
	if err != nil || f.job == nil || f.job.AttemptCount != 1 || f.job.StartedAt == nil {
		t.Fatalf("original claim: %+v %v", f.job, err)
	}
	var claimId int64
	var claimSha256 string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT event_id,payload_sha256 FROM competition_job_event
		 WHERE job_id=$1 AND event_type='claimed' ORDER BY event_id LIMIT 1`, f.job.JobId).Scan(&claimId, &claimSha256))
	})
	stoppedAt := f.job.StartedAt.Add(50 * time.Minute)
	f.now = f.job.StartedAt.Add(time.Duration(f.settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second + time.Minute)
	lease, err = f.store.AcquireAutomationLease(ctx, f.settings, "synthetic-reevaluation-supervisor", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.settings.automationLease = lease
	f.job, err = f.store.Claim(ctx, f.settings, "synthetic-reevaluation-worker", testWorkerImageDigest())
	if err != nil || f.job == nil || f.job.AttemptCount != 2 {
		t.Fatalf("late original reclaim: %+v %v", f.job, err)
	}
	if retry, err := f.store.Complete(ctx, f.settings, "synthetic-reevaluation-worker", f.job.JobId, EvaluationOutcome{
		Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "synthetic operator interruption retained the original deadline"),
	}); err != nil || retry {
		t.Fatalf("late reclaim did not terminate original budget: %t %v", retry, err)
	}
	f.principal = &Principal{Id: "synthetic-reevaluation-operator", Role: "operator", Environment: f.settings.AutomationPolicy.Environment,
		CompetitionId: f.settings.CompetitionId, SeasonId: f.settings.AutomationPolicy.SeasonId}
	f.hold, err = f.store.SetAutomationHold(ctx, f.settings, f.principal.Id, model.CompetitionAutomationHoldArgs{
		Kind: "evaluation", Active: true, IdempotencyKey: "synthetic-operator-stop-hold", ReasonCode: "OPERATOR_HOLD",
		PublicExplanation: "Synthetic operator-stop recovery remains held.", EvidenceSha256: strings.Repeat("6", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.runtime = model.CompetitionJobReevaluationRuntime{Schema: 1, Kind: model.CompetitionJobReevaluationRuntimeKind,
		Accepted: true, Capability: model.CompetitionJobReevaluationCapability, SourceCommit: strings.Repeat("7", 40),
		ApiImageDigest: testApiImageDigest(), WorkerImageDigest: "sha256:" + strings.Repeat("8", 64), WorkerSha256: strings.Repeat("9", 64),
		EvidenceSha256: strings.Repeat("a", 64), ApiCompatible: true, WorkerCompatible: true}
	terminal := readCompetitionReevaluationPgSnapshot(ctx, f.job).job
	a := model.CompetitionJobReevaluationAuthorization{Schema: 1, Kind: model.CompetitionJobReevaluationAuthorizationKind,
		IncidentClass: "operator_authorized_interrupted_budget_v1", Environment: f.principal.Environment, CompetitionId: f.principal.CompetitionId,
		SeasonId: f.principal.SeasonId, JobId: f.job.JobId, RoundId: f.job.RoundId, ExpectedCycle: 0, ExpectedAttemptCount: 2,
		ExpectedTerminalSnapshot: json.RawMessage(terminal), EvidenceSha256: strings.Repeat("b", 64), IncidentAttempt: 1,
		BaselineSha256: f.job.RoundBaseline.Sha256, FixedSourceCommit: f.runtime.SourceCommit, FixedWorkerSha256: f.runtime.WorkerSha256,
		FixedWorkerImageDigest: f.runtime.WorkerImageDigest, ApiImageDigest: f.runtime.ApiImageDigest, SingleUse: true}
	a.ExpectedTerminalSha256, err = CompetitionJobReevaluationSnapshotDigest(terminal)
	if err != nil {
		t.Fatal(err)
	}
	a.RoundPolicySha256, err = CompetitionJobReevaluationSnapshotDigest(f.round.PolicyJson)
	if err != nil {
		t.Fatal(err)
	}
	f.request = model.CompetitionJobReevaluationRequest{IdempotencyKey: "synthetic-operator-stop-approval", Authorization: a}
	sealCompetitionIncidentReevaluationFixture(t, ctx, f, "failed")
	proof := map[string]any{"schema": 1, "action": "stop_worker", "stopped_at": stoppedAt, "claim_event_id": claimId,
		"claim_event_sha256": claimSha256, "evidence_sha256": strings.Repeat("c", 64)}
	setCompetitionOperatorInterruptionProof(t, &f.request, proof)
	return f, proof
}

// This positive is parent-compatible source: the predecessor must reject the
// missing operator class, not pass because a heartbeat event was manufactured.
func TestCompetitionReevaluationOperatorInterruptionPgPreservesStopAndOriginalBudget(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		f, _ := newCompetitionOperatorInterruptionPgFixture(t, ctx)
		before := readCompetitionReevaluationPgSnapshot(ctx, f.job)
		receipt, err := f.store.AuthorizeJobReevaluation(ctx, f.settings, f.principal, f.request)
		if err != nil || receipt == nil || receipt.IncidentClass != "operator_authorized_interrupted_budget_v1" || receipt.Cycle != 1 ||
			receipt.PreviousFailure.Kind != "infrastructure" || receipt.PreviousFailure.Code != "evaluation_time_budget_exhausted" ||
			receipt.PreviousFailure.Retriable || receipt.StartedAt != nil || receipt.ExecutionTimeoutSeconds != 3*60*60 {
			t.Fatalf("bound real operator interruption refused or relabelled: %+v %v", receipt, err)
		}
		after := readCompetitionReevaluationPgSnapshot(ctx, f.job)
		requireCompetitionReevaluationOriginalEvidence(t, before, after)
		f.now = f.request.Authorization.ExpiresAt.Add(time.Minute)
		replayed, err := f.store.AuthorizeJobReevaluation(ctx, f.settings, f.principal, f.request)
		if err != nil || !reflect.DeepEqual(receipt, replayed) || !reflect.DeepEqual(after, readCompetitionReevaluationPgSnapshot(ctx, f.job)) {
			t.Fatalf("expired exact replay mutated the immutable cycle: %v", err)
		}
		claimed := f.releaseAndClaim(t, ctx)
		if claimed.CycleStartedAt == nil || !claimed.CycleStartedAt.Equal(f.now) || claimed.StartedAt == nil || !claimed.StartedAt.Equal(*f.job.StartedAt) ||
			claimed.EvaluationCycle != 1 || claimed.AttemptCount != f.request.Authorization.ExpectedAttemptCount+1 {
			t.Fatal("new cycle reset original start or lacked its own bounded first claim")
		}
	})
}

// Binding the stop to another claim, an expired or future time, or the wrong
// terminal event cannot mutate any old evidence even with a fresh request hash.
func TestCompetitionReevaluationOperatorInterruptionPgRejectsWrongLedgerFacts(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		f, originalProof := newCompetitionOperatorInterruptionPgFixture(t, ctx)
		before := readCompetitionReevaluationPgSnapshot(ctx, f.job)
		for _, c := range []struct {
			name   string
			mutate func(*model.CompetitionJobReevaluationRequest, map[string]any)
		}{
			{name: "wrong-claim-digest", mutate: func(_ *model.CompetitionJobReevaluationRequest, p map[string]any) {
				p["claim_event_sha256"] = strings.Repeat("d", 64)
			}},
			{name: "missing-claim", mutate: func(_ *model.CompetitionJobReevaluationRequest, p map[string]any) { p["claim_event_id"] = int64(1) }},
			{name: "before-claim", mutate: func(_ *model.CompetitionJobReevaluationRequest, p map[string]any) {
				p["stopped_at"] = f.job.StartedAt.Add(-time.Microsecond)
			}},
			{name: "at-original-expiry", mutate: func(_ *model.CompetitionJobReevaluationRequest, p map[string]any) {
				p["stopped_at"] = f.job.StartedAt.Add(3 * time.Hour)
			}},
			{name: "wrong-terminal-digest", mutate: func(r *model.CompetitionJobReevaluationRequest, _ map[string]any) {
				r.Authorization.IncidentEventSha256 = strings.Repeat("d", 64)
			}},
			{name: "claim-as-terminal", mutate: func(r *model.CompetitionJobReevaluationRequest, _ map[string]any) { r.Authorization.IncidentEventId-- }},
		} {
			request := f.request
			proof := map[string]any{}
			for key, value := range originalProof {
				proof[key] = value
			}
			c.mutate(&request, proof)
			setCompetitionOperatorInterruptionProof(t, &request, proof)
			if receipt, err := f.store.AuthorizeJobReevaluation(ctx, f.settings, f.principal, request); err == nil || receipt != nil {
				t.Fatalf("%s borrowed operator interruption authority", c.name)
			}
			if !reflect.DeepEqual(before, readCompetitionReevaluationPgSnapshot(ctx, f.job)) {
				t.Fatalf("%s changed persisted evidence", c.name)
			}
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if receipt, err := f.store.AuthorizeJobReevaluation(canceled, f.settings, f.principal, f.request); err == nil || receipt != nil {
			t.Fatal("canceled approval acquired a cycle")
		}
		if !reflect.DeepEqual(before, readCompetitionReevaluationPgSnapshot(ctx, f.job)) {
			t.Fatal("canceled approval changed persisted evidence")
		}
	})
}

// A genuine intermediate archive outcome is not an operator-stop gap. The
// separate class cannot borrow that history merely because its last code fits.
func TestCompetitionReevaluationOperatorInterruptionPgRejectsInterveningOutcome(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		f := newCompetitionReevaluationAuthorizationPgFixtureWithTerminal(t, ctx, "not-operator-stop", func(*Settings, *queuedJob) EvaluationOutcome {
			return EvaluationOutcome{Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "synthetic unrelated terminal budget")}
		}, true)
		f.request.Authorization.IncidentClass = "operator_authorized_interrupted_budget_v1"
		sealCompetitionIncidentReevaluationFixture(t, ctx, f, "failed")
		var claimId int64
		var claimDigest string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT event_id,payload_sha256 FROM competition_job_event
			 WHERE job_id=$1 AND event_type='claimed' ORDER BY event_id LIMIT 1`, f.job.JobId).Scan(&claimId, &claimDigest))
		})
		setCompetitionOperatorInterruptionProof(t, &f.request, map[string]any{
			"schema": 1, "action": "stop_worker", "stopped_at": f.job.StartedAt.Add(time.Minute), "claim_event_id": claimId,
			"claim_event_sha256": claimDigest, "evidence_sha256": strings.Repeat("c", 64),
		})
		before := readCompetitionReevaluationPgSnapshot(ctx, f.job)
		if receipt, err := f.store.AuthorizeJobReevaluation(ctx, f.settings, f.principal, f.request); !errors.Is(err, ErrConflict) || receipt != nil {
			t.Fatalf("genuine outcome acquired stopped-claim authority: %+v %v", receipt, err)
		}
		if !reflect.DeepEqual(before, readCompetitionReevaluationPgSnapshot(ctx, f.job)) {
			t.Fatal("rejected genuine outcome changed history")
		}
	})
}

// The whole migrated chain must reject stop evidence smuggled through an older
// class, even when a direct writer bypasses request validation and sets its scope.
func TestCompetitionReevaluationOperatorInterruptionPgInsertGuardRejectsBorrowedClass(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		f, _ := newCompetitionOperatorInterruptionPgFixture(t, ctx)
		before := readCompetitionReevaluationPgSnapshot(ctx, f.job)
		request := f.request
		request.Authorization.IncidentClass = model.CompetitionJobReevaluationHeartbeatBudget
		var err error
		request.AuthorizationSha256, err = model.CompetitionAutomationDigest(request.Authorization)
		if err != nil {
			t.Fatal(err)
		}
		authorizationBytes, err := json.Marshal(request.Authorization)
		if err != nil {
			t.Fatal(err)
		}
		requestDigest, err := model.CompetitionAutomationDigest(request)
		if err != nil {
			t.Fatal(err)
		}
		receiptBytes, err := json.Marshal(map[string]any{"incident_class": request.Authorization.IncidentClass,
			"previous_failure":          model.CompetitionEvaluationFailure{Kind: "infrastructure", Code: "evaluation_time_budget_exhausted", Retriable: false},
			"execution_timeout_seconds": 10800})
		if err != nil {
			t.Fatal(err)
		}
		var insertError error
		_ = captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_reevaluation_operator',$1,true),set_config('urnetwork.competition_reevaluation_request',$2,true)`, f.principal.Id, requestDigest))
				_, insertError = tx.Exec(ctx, `INSERT INTO competition_job_reevaluation(job_id,round_id,environment,competition_id,season_id,operator_id,idempotency_key,request_sha256,authorization_sha256,authorized_at,original_terminal_json,authorization_json,receipt_json)
				 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12::jsonb,$13::jsonb)`, f.job.JobId, f.job.RoundId,
					f.principal.Environment, f.principal.CompetitionId, f.principal.SeasonId, f.principal.Id, request.IdempotencyKey,
					requestDigest, request.AuthorizationSha256, f.now, string(before.job), string(authorizationBytes), string(receiptBytes))
				server.Raise(insertError)
			}, server.OptNoRetry())
		})
		var pgError *pgconn.PgError
		if !errors.As(insertError, &pgError) || pgError.Code != "P0001" || !strings.Contains(pgError.Message, "operator-stop evidence cannot authorize another incident class") {
			t.Fatalf("whole migration chain missed exact borrowed-proof refusal: %v", insertError)
		}
		if !reflect.DeepEqual(before, readCompetitionReevaluationPgSnapshot(ctx, f.job)) {
			t.Fatal("rejected direct insert changed immutable evidence")
		}
		if receipt, err := f.store.AuthorizeJobReevaluation(ctx, f.settings, f.principal, f.request); err != nil || receipt == nil {
			t.Fatalf("valid operator path did not survive the full guard chain: %+v %v", receipt, err)
		}
	})
}
