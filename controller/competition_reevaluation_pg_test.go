// Disposable PostgreSQL reevaluation tests keep original submission/history
// immutable while exercising explicit operator recovery and generation fences.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Reaches a terminal archive failure through real claim/completion entry points;
// no direct terminal reset or mutable shared fixture is needed for recovery.
func newCompetitionReevaluationPgFixture(t testing.TB, ctx context.Context, name string) (*submissionArchiveFixture, *queuedJob) {
	t.Helper()
	fixture := newSubmissionArchiveFixture(t, ctx, "synthetic-reevaluation-"+name)
	round, err := fixture.store.CreateStagingRound(ctx, fixture.settings, GenerateRoundArgs{
		OpensAt: fixture.clock, ClosesAt: fixture.clock.Add(24 * time.Hour), RevealAt: fixture.clock.Add(24 * time.Hour),
	}, false)
	if err != nil {
		t.Fatalf("create synthetic recovery round: %v", err)
	}
	if _, _, err := fixture.store.Enqueue(ctx, fixture.settings, round.RoundId, fixture.patch(t, "synthetic-reevaluation"),
		"synthetic-reevaluation-submitter", testApiImageDigest()); err != nil {
		t.Fatalf("enqueue synthetic original submission: %v", err)
	}
	job, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-lease-worker", testWorkerImageDigest())
	if err != nil || job == nil {
		t.Fatalf("claim synthetic original submission: job=%+v error=%v", job, err)
	}
	for attempt := 1; attempt <= fixture.settings.MaxInfrastructureAttempts; attempt++ {
		if job.AttemptCount != attempt || job.StartedAt == nil {
			t.Fatalf("synthetic original claim %d has attempt=%d started_at=%v", attempt, job.AttemptCount, job.StartedAt)
		}
		incidentDetail := sha256.Sum256([]byte("failure evidence manifest contains an unsafe artifact"))
		outcome := EvaluationOutcome{
			Error: infrastructureError("artifact_archive_failed", "synthetic source inventory retention rejected"),
			archiveFailure: &competitionArtifactArchiveFailure{
				Phase: "authenticate_failed_evidence", Category: "other", DetailSha256: hex.EncodeToString(incidentDetail[:]),
			},
		}
		retry, err := fixture.store.Complete(ctx, fixture.settings, "synthetic-lease-worker", job.JobId, outcome)
		if err != nil || retry != (attempt < fixture.settings.MaxInfrastructureAttempts) {
			t.Fatalf("complete original archive failure %d: retry=%t error=%v", attempt, retry, err)
		}
		if retry {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT available_at FROM competition_job WHERE job_id=$1`, job.JobId).Scan(&fixture.clock))
			})
			claimed, claimErr := fixture.store.Claim(ctx, fixture.settings, "synthetic-lease-worker", testWorkerImageDigest())
			if claimErr != nil || claimed == nil || claimed.JobId != job.JobId {
				t.Fatalf("claim original archive retry %d: job=%+v error=%v", attempt+1, claimed, claimErr)
			}
			job = claimed
		}
	}
	// Authorization must create a fresh cycle without rewriting this expired start.
	fixture.clock = job.StartedAt.Add(time.Duration(fixture.settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second + time.Minute)
	return fixture, job
}

// Full row snapshots expose partial transactions; the immutable projection
// separately covers bytes, admission identity and the original execution start.
type competitionReevaluationPgSnapshot struct {
	job, immutableJob, events, principals, round, baseline, audit []byte
}

// The production-v2 scope has an active seven-day source/round, an immutable
// shared baseline, a scoped operator and an independently bound fixed runtime.
type competitionReevaluationAuthorizationPgFixture struct {
	*competitionRankingFixture
	job       *queuedJob
	principal *Principal
	runtime   model.CompetitionJobReevaluationRuntime
	request   model.CompetitionJobReevaluationRequest
	hold      *model.CompetitionAutomationHold
}

// All paid/admitted identity is synthetic; the original job actually reaches
// its archive-failure terminal state before its original execution clock expires.
func newCompetitionReevaluationAuthorizationPgFixture(t testing.TB, ctx context.Context, name string) *competitionReevaluationAuthorizationPgFixture {
	t.Helper()
	return newCompetitionReevaluationAuthorizationPgFixtureWithTerminal(t, ctx, name, nil, true)
}

// Negative terminal controls retain a real earlier archive event, so genuine
// failures and unknown-budget failures cannot borrow that event as authority.
func newCompetitionReevaluationAuthorizationPgFixtureWithTerminal(t testing.TB, ctx context.Context, name string,
	terminalOutcome func(*Settings, *queuedJob) EvaluationOutcome, verifiedInventoryIncident bool) *competitionReevaluationAuthorizationPgFixture {
	t.Helper()
	return newCompetitionReevaluationAuthorizationPgFixtureWithArchiveEvidence(t, ctx, name, terminalOutcome, verifiedInventoryIncident, false)
}

// Legacy controls use the actual completion path without typed archive fields;
// every existing fixture keeps the original inventory evidence by default.
func newCompetitionReevaluationAuthorizationPgFixtureWithArchiveEvidence(t testing.TB, ctx context.Context, name string,
	terminalOutcome func(*Settings, *queuedJob) EvaluationOutcome, verifiedInventoryIncident, legacyArchive bool) *competitionReevaluationAuthorizationPgFixture {
	t.Helper()
	fixture := &competitionReevaluationAuthorizationPgFixture{competitionRankingFixture: newCompetitionAutomationFixture(t)}
	addCompetitionAutomationJob(t, fixture.competitionRankingFixture, 80)
	lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-reevaluation-supervisor", 5*time.Minute)
	if err != nil {
		t.Fatalf("acquire original supervisor: %v", err)
	}
	fixture.settings.automationLease, fixture.settings.automationRoundId = lease, fixture.round.RoundId
	patch, patchError := ValidateAndCanonicalizePatch(testPatch("synthetic-reevaluation-"+name), fixture.settings.PatchPolicy)
	if patchError != nil {
		t.Fatalf("canonicalize synthetic recovery patch: %v", patchError)
	}
	if _, _, err := fixture.store.Enqueue(ctx, fixture.settings, fixture.round.RoundId, patch,
		"synthetic-reevaluation-submitter", testApiImageDigest()); err != nil {
		t.Fatalf("enqueue original v2 submission: %v", err)
	}
	fixture.job, err = fixture.store.Claim(ctx, fixture.settings, "synthetic-reevaluation-worker", testWorkerImageDigest())
	if err != nil || fixture.job == nil {
		t.Fatalf("claim original v2 submission: job=%+v error=%v", fixture.job, err)
	}
	for attempt := 1; attempt <= fixture.settings.MaxInfrastructureAttempts; attempt++ {
		if fixture.job.AttemptCount != attempt || fixture.job.StartedAt == nil || fixture.job.RoundBaseline == nil {
			t.Fatalf("original v2 claim %d lacks its existing start/baseline: %+v", attempt, fixture.job)
		}
		incidentDetail := sha256.Sum256([]byte("failure evidence manifest contains an unsafe artifact"))
		outcome := EvaluationOutcome{
			Error: infrastructureError("artifact_archive_failed", "synthetic source inventory retention rejected"),
			archiveFailure: &competitionArtifactArchiveFailure{
				Phase: "authenticate_failed_evidence", Category: "other", DetailSha256: hex.EncodeToString(incidentDetail[:]),
			},
		}
		if !verifiedInventoryIncident {
			outcome.archiveFailure.DetailSha256 = strings.Repeat("f", 64)
		}
		if legacyArchive {
			outcome.archiveFailure = nil
		}
		if attempt == 2 && terminalOutcome != nil {
			outcome = terminalOutcome(fixture.settings, fixture.job)
		}
		retry, err := fixture.store.Complete(ctx, fixture.settings, "synthetic-reevaluation-worker", fixture.job.JobId, outcome)
		expectedRetry := outcome.Error != nil && outcome.Error.Retriable && attempt < fixture.settings.MaxInfrastructureAttempts
		if err != nil || retry != expectedRetry {
			t.Fatalf("complete original v2 archive failure %d: retry=%t error=%v", attempt, retry, err)
		}
		if retry {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT available_at FROM competition_job WHERE job_id=$1`, fixture.job.JobId).Scan(&fixture.now))
			})
			claimed, claimErr := fixture.store.Claim(ctx, fixture.settings, "synthetic-reevaluation-worker", testWorkerImageDigest())
			if claimErr != nil || claimed == nil || claimed.JobId != fixture.job.JobId {
				t.Fatalf("claim original v2 retry %d: job=%+v error=%v", attempt+1, claimed, claimErr)
			}
			fixture.job = claimed
		} else {
			break
		}
	}
	fixture.now = fixture.job.StartedAt.Add(time.Duration(fixture.settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second + time.Minute)
	lease, err = fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-reevaluation-supervisor", 5*time.Minute)
	if err != nil {
		t.Fatalf("acquire recovery supervisor: %v", err)
	}
	fixture.settings.automationLease = lease
	fixture.principal = &Principal{Id: "synthetic-reevaluation-operator", Role: "operator",
		Environment: fixture.settings.AutomationPolicy.Environment, CompetitionId: fixture.settings.CompetitionId, SeasonId: fixture.settings.AutomationPolicy.SeasonId}
	fixture.hold, err = fixture.store.SetAutomationHold(ctx, fixture.settings, fixture.principal.Id, model.CompetitionAutomationHoldArgs{
		Kind: "evaluation", Active: true, IdempotencyKey: "synthetic-recovery-hold-" + name, ReasonCode: "OPERATOR_HOLD",
		PublicExplanation: "Synthetic reevaluation qualification remains gated.", EvidenceSha256: strings.Repeat("6", 64),
	})
	if err != nil {
		t.Fatalf("set synthetic recovery hold: %v", err)
	}
	fixture.runtime = model.CompetitionJobReevaluationRuntime{
		Schema: 1, Kind: model.CompetitionJobReevaluationRuntimeKind, Accepted: true, Capability: model.CompetitionJobReevaluationCapability,
		SourceCommit: strings.Repeat("7", 40), ApiImageDigest: testApiImageDigest(), WorkerImageDigest: "sha256:" + strings.Repeat("8", 64),
		WorkerSha256: strings.Repeat("9", 64), EvidenceSha256: strings.Repeat("a", 64), ApiCompatible: true, WorkerCompatible: true,
	}
	runtimeSha256, err := model.CompetitionAutomationDigest(fixture.runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := BindCompetitionJobReevaluationRuntime(fixture.settings, fixture.runtime, runtimeSha256); err != nil {
		t.Fatalf("bind synthetic qualified fixed runtime: %v", err)
	}
	snapshot := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
	terminalSha256, err := CompetitionJobReevaluationSnapshotDigest(snapshot.job)
	if err != nil {
		t.Fatalf("digest original terminal snapshot: %v", err)
	}
	var policyJson []byte
	var incidentEventId int64
	var incidentAttempt int
	var incidentEventSha256 string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT policy_json FROM competition_round WHERE round_id=$1`, fixture.round.RoundId).Scan(&policyJson))
		server.Raise(conn.QueryRow(ctx, `SELECT event_id,payload_sha256,(payload_json->>'attempt')::integer FROM competition_job_event
		 WHERE job_id=$1 AND event_type='infrastructure_retry' AND payload_json->>'error_code'='artifact_archive_failed'
		 AND (($2 AND NOT (payload_json ? 'artifact_archive_failure')) OR (NOT $2 AND payload_json ? 'artifact_archive_failure'))
		 ORDER BY event_id LIMIT 1`, fixture.job.JobId, legacyArchive).Scan(&incidentEventId, &incidentEventSha256, &incidentAttempt))
	})
	policySha256, err := CompetitionJobReevaluationSnapshotDigest(policyJson)
	if err != nil {
		t.Fatalf("digest frozen round policy: %v", err)
	}
	rejectedPath := "failed-evidence/evaluation-sources.json"
	manifestJson, err := json.Marshal(map[string]any{
		"schema": 1, "kind": "sim-latency-failed-evidence-manifest", "job_id": fixture.job.JobId.String(),
		"round_id": fixture.job.RoundId.String(), "attempt": incidentAttempt, "sanitized": true,
		"artifacts": []evaluationArtifact{
			{Path: "failed-evidence/failure.json", Sha256: strings.Repeat("c", 64), Bytes: 1},
			{Path: rejectedPath, Sha256: strings.Repeat("d", 64), Bytes: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestDirectory := filepath.Join(fixture.settings.ArtifactRoot, fixture.job.JobId.String(), fmt.Sprintf("attempt-%02d", incidentAttempt))
	if err := os.MkdirAll(manifestDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDirectory, "failed-evidence-manifest.json"), manifestJson, 0o400); err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifestJson)
	authorization := model.CompetitionJobReevaluationAuthorization{
		Schema: 1, Kind: model.CompetitionJobReevaluationAuthorizationKind, IncidentClass: model.CompetitionJobReevaluationArchiveInventory,
		Environment: fixture.principal.Environment, CompetitionId: fixture.principal.CompetitionId, SeasonId: fixture.principal.SeasonId,
		JobId: fixture.job.JobId, RoundId: fixture.job.RoundId, ExpectedCycle: 0, ExpectedAttemptCount: fixture.job.AttemptCount,
		ExpectedTerminalSnapshot: json.RawMessage(snapshot.job), ExpectedTerminalSha256: terminalSha256, EvidenceSha256: strings.Repeat("b", 64),
		IncidentEventId: incidentEventId, IncidentEventSha256: incidentEventSha256, IncidentAttempt: incidentAttempt,
		FailureManifestSha256: hex.EncodeToString(manifestDigest[:]), RejectedArtifactPath: rejectedPath,
		RoundPolicySha256: policySha256, BaselineSha256: fixture.job.RoundBaseline.Sha256,
		RuntimeQualificationSha256: runtimeSha256, FixedSourceCommit: fixture.runtime.SourceCommit, FixedWorkerSha256: fixture.runtime.WorkerSha256,
		FixedWorkerImageDigest: fixture.runtime.WorkerImageDigest, ApiImageDigest: fixture.runtime.ApiImageDigest,
		AuthorizedAt: fixture.now, ExpiresAt: fixture.now.Add(time.Hour), SingleUse: true,
	}
	authorizationSha256, err := model.CompetitionAutomationDigest(authorization)
	if err != nil {
		t.Fatal(err)
	}
	fixture.request = model.CompetitionJobReevaluationRequest{IdempotencyKey: "synthetic-reevaluation-" + name,
		AuthorizationSha256: authorizationSha256, Authorization: authorization}
	return fixture
}

// Recovery may append new history, but every original event, principal,
// immutable paid-job identity, source/seed/policy and control byte stays intact.
func requireCompetitionReevaluationOriginalEvidence(t testing.TB, before, after competitionReevaluationPgSnapshot) {
	t.Helper()
	if !bytes.Equal(before.immutableJob, after.immutableJob) || !bytes.Equal(before.principals, after.principals) ||
		!bytes.Equal(before.round, after.round) || !bytes.Equal(before.baseline, after.baseline) {
		t.Fatal("reevaluation changed original admission, principal, start, source/seed/policy, or baseline")
	}
	var originalEvents, currentEvents []json.RawMessage
	if err := json.Unmarshal(before.events, &originalEvents); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after.events, &currentEvents); err != nil {
		t.Fatal(err)
	}
	if len(currentEvents) < len(originalEvents) {
		t.Fatal("reevaluation deleted original event history")
	}
	for index, original := range originalEvents {
		if !bytes.Equal(original, currentEvents[index]) {
			t.Fatalf("reevaluation rewrote original event %d", index)
		}
	}
}

// Independent reads happen only after the operation under test releases its
// connection; every query is confined to this disposable synthetic job/round.
func readCompetitionReevaluationPgSnapshot(ctx context.Context, job *queuedJob) (snapshot competitionReevaluationPgSnapshot) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT to_jsonb(j),jsonb_build_object(
		 'job_id',j.job_id,'round_id',j.round_id,'patch_bytes',encode(j.patch_bytes,'hex'),
		 'patch_sha256',j.patch_sha256,'cache_key',j.cache_key,'submitted_at',j.submitted_at,
		 'started_at',j.started_at,'artifact_retain_until',j.artifact_retain_until,'api_image_digest',j.api_image_digest)
		 FROM competition_job j WHERE job_id=$1`, job.JobId).Scan(&snapshot.job, &snapshot.immutableJob))
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY event_id),'[]'::jsonb)
		 FROM competition_job_event e WHERE job_id=$1`, job.JobId).Scan(&snapshot.events))
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(p) ORDER BY principal_id),'[]'::jsonb)
		 FROM competition_job_principal p WHERE job_id=$1`, job.JobId).Scan(&snapshot.principals))
		server.Raise(conn.QueryRow(ctx, `SELECT to_jsonb(r) FROM competition_round r WHERE round_id=$1`, job.RoundId).Scan(&snapshot.round))
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(b)),'[]'::jsonb)
		 FROM competition_round_baseline b WHERE round_id=$1`, job.RoundId).Scan(&snapshot.baseline))
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(a)),'[]'::jsonb)
		 FROM competition_job_reevaluation a WHERE job_id=$1`, job.JobId).Scan(&snapshot.audit))
	})
	return snapshot
}

// Only this disposable fixture's exact current hold is released; no live
// policy, operator approval, queue or service outside the test is addressed.
func (self *competitionReevaluationAuthorizationPgFixture) releaseHold(t testing.TB, ctx context.Context) {
	t.Helper()
	if self.hold.Request.Active {
		args := self.hold.Request
		args.Active, args.ExpectedRevision = false, self.hold.Revision
		args.IdempotencyKey = "synthetic-recovery-release-" + server.NewId().String()
		args.ReasonCode = "RECOVERY_APPROVED"
		var err error
		self.hold, err = self.store.SetAutomationHold(ctx, self.settings, self.principal.Id, args)
		if err != nil {
			t.Fatalf("release synthetic owned evaluation hold: %v", err)
		}
	}
}

// Every recovered claim still needs a fresh supervisor lease and the exact
// independently approved worker image, including after retry or handback.
func (self *competitionReevaluationAuthorizationPgFixture) releaseAndClaim(t testing.TB, ctx context.Context) *queuedJob {
	t.Helper()
	self.releaseHold(t, ctx)
	lease, err := self.store.AcquireAutomationLease(ctx, self.settings, "synthetic-reevaluation-supervisor", 5*time.Minute)
	if err != nil {
		t.Fatalf("acquire recovered claim supervisor: %v", err)
	}
	self.settings.automationLease = lease
	job, err := self.store.Claim(ctx, self.settings, "synthetic-reevaluation-worker", self.runtime.WorkerImageDigest)
	if err != nil || job == nil || job.JobId != self.job.JobId {
		t.Fatalf("claim exact recovered job: job=%+v error=%v", job, err)
	}
	return job
}

// Each positive database root owns one disposable schema without implicit
// reruns; the outer qualification runner controls fresh-process repetitions.
func runCompetitionReevaluationPgTest(t *testing.T, run func(testing.TB, context.Context)) {
	t.Helper()
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		run(t, ctx)
	})
}

// A terminal job is not resettable by an ordinary writer, including one that
// tries to erase attempts, failure evidence, or the original execution start.
func TestCompetitionReevaluationPgRejectsUnauditedTerminalReset(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		_, job := newCompetitionReevaluationPgFixture(t, ctx, "direct-reset")
		before := readCompetitionReevaluationPgSnapshot(ctx, job)
		for _, check := range []struct {
			name  string
			query string
		}{
			{name: "state", query: `UPDATE competition_job SET state='queued',completed_at=NULL WHERE job_id=$1`},
			{name: "attempt-count", query: `UPDATE competition_job SET attempt_count=0 WHERE job_id=$1`},
			{name: "failure", query: `UPDATE competition_job SET eval_error_json=NULL WHERE job_id=$1`},
			{name: "original-start", query: `UPDATE competition_job SET started_at=NULL WHERE job_id=$1`},
		} {
			err := captureDatabaseError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, check.query, job.JobId))
				}, server.OptNoRetry())
			})
			if err == nil {
				t.Fatalf("direct terminal mutation %s bypassed immutable recovery authority", check.name)
			}
			if after := readCompetitionReevaluationPgSnapshot(ctx, job); !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected direct terminal mutation %s changed persisted evidence", check.name)
			}
		}
	})
}

// A real production-v2 authorization queues the same paid job once, retaining
// the expired original start and exact terminal evidence behind an immutable receipt.
func TestCompetitionReevaluationPgAuthorizesExactSingleUseRecovery(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, "exact-replay")
		before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		if fixture.now.Before(fixture.job.StartedAt.Add(3*time.Hour)) || fixture.round.Staging ||
			!fixture.settings.AutomationPolicy.Enabled || fixture.job.RoundBaseline == nil {
			t.Fatal("recovery fixture did not reproduce an expired original production-v2 evaluation with its frozen baseline")
		}
		receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request)
		if err != nil || receipt == nil {
			t.Fatalf("authorize known source-inventory defect: receipt=%+v error=%v", receipt, err)
		}
		requestSha256, err := model.CompetitionAutomationDigest(fixture.request)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.JobId != fixture.job.JobId || receipt.RoundId != fixture.job.RoundId || receipt.Cycle != 1 ||
			receipt.AttemptOffset != fixture.job.AttemptCount || receipt.ExecutionTimeoutSeconds != 3*60*60 || receipt.StartedAt != nil ||
			receipt.RequestSha256 != requestSha256 || receipt.AuthorizationSha256 != fixture.request.AuthorizationSha256 ||
			receipt.PreviousTerminalSha256 != fixture.request.Authorization.ExpectedTerminalSha256 ||
			receipt.PreviousFailure.Kind != "infrastructure" || receipt.PreviousFailure.Code != "artifact_archive_failed" || receipt.PreviousFailure.Retriable ||
			model.ValidateCompetitionJobReevaluationCycle(&receipt.CompetitionJobReevaluationCycle) != nil {
			t.Fatalf("authorization granted an unbound or expanded recovery receipt: %+v", receipt)
		}
		saved, err := fixture.store.GetJob(ctx, fixture.settings, fixture.job.JobId, fixture.principal)
		if err != nil || saved == nil || saved.State != "queued" || saved.AttemptCount != fixture.job.AttemptCount ||
			saved.EvaluationCycle != 1 || saved.CycleAttemptOffset != fixture.job.AttemptCount || saved.CycleStartedAt != nil ||
			saved.StartedAt == nil || !saved.StartedAt.Equal(*fixture.job.StartedAt) || saved.Reevaluation == nil || saved.Reevaluation.ReceiptSha256 != receipt.ReceiptSha256 {
			t.Fatalf("authorization did not queue the same original paid job without claiming it: job=%+v error=%v", saved, err)
		}
		after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		requireCompetitionReevaluationOriginalEvidence(t, before, after)
		replayed, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request)
		if err != nil || !reflect.DeepEqual(receipt, replayed) {
			t.Fatalf("exact replay did not return the immutable first receipt: receipt=%+v error=%v", replayed, err)
		}
		if replayAfter := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(replayAfter, after) {
			t.Fatal("exact replay changed state or appended duplicate history")
		}
		manifestPath := filepath.Join(fixture.settings.ArtifactRoot, fixture.job.JobId.String(),
			fmt.Sprintf("attempt-%02d", fixture.request.Authorization.IncidentAttempt), "failed-evidence-manifest.json")
		if err := os.Remove(manifestPath); err != nil {
			t.Fatal(err)
		}
		fixture.now = fixture.request.Authorization.ExpiresAt.Add(time.Minute)
		changedRuntime := fixture.runtime
		changedRuntime.SourceCommit, changedRuntime.WorkerSha256 = strings.Repeat("e", 40), strings.Repeat("e", 64)
		changedDigest, err := model.CompetitionAutomationDigest(changedRuntime)
		if err != nil {
			t.Fatal(err)
		}
		if err := BindCompetitionJobReevaluationRuntime(fixture.settings, changedRuntime, changedDigest); err != nil {
			t.Fatal(err)
		}
		replayed, err = fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request)
		if err != nil || !reflect.DeepEqual(receipt, replayed) {
			t.Fatalf("committed exact replay depended on expired authorization, old runtime, or removed local manifest: receipt=%+v error=%v", replayed, err)
		}
		if replayAfter := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(replayAfter, after) {
			t.Fatal("expired historical replay reset or appended to the authorized cycle")
		}
		for _, change := range []struct {
			name   string
			mutate func(*model.CompetitionJobReevaluationRequest)
		}{
			{name: "another-idempotency-key", mutate: func(r *model.CompetitionJobReevaluationRequest) { r.IdempotencyKey += "-conflict" }},
			{name: "another-evidence-document", mutate: func(r *model.CompetitionJobReevaluationRequest) {
				r.Authorization.EvidenceSha256 = strings.Repeat("e", 64)
			}},
		} {
			request := fixture.request
			change.mutate(&request)
			request.AuthorizationSha256, err = model.CompetitionAutomationDigest(request.Authorization)
			if err != nil {
				t.Fatal(err)
			}
			if result, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, request); !errors.Is(err, ErrConflict) || result != nil {
				t.Fatalf("%s granted another recovery: receipt=%+v error=%v", change.name, result, err)
			}
			if conflictAfter := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(conflictAfter, after) {
				t.Fatalf("%s changed an already authorized job", change.name)
			}
		}
	})
}

// Operator identity is exact and scope-bound; a submitter, wildcard operator,
// cross-scope operator or absent principal cannot consume recovery authority.
func TestCompetitionReevaluationPgRejectsUnscopedOperatorAuthority(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, "operator-scope")
		before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		for _, check := range []struct {
			name      string
			principal *Principal
		}{
			{name: "absent"},
			{name: "submitter", principal: &Principal{Id: "synthetic-submitter", Role: "submitter", Environment: fixture.principal.Environment, CompetitionId: fixture.principal.CompetitionId, SeasonId: fixture.principal.SeasonId}},
			{name: "wildcard", principal: &Principal{Id: "synthetic-wildcard", Role: "operator"}},
			{name: "environment", principal: &Principal{Id: "synthetic-operator", Role: "operator", Environment: "synthetic-other", CompetitionId: fixture.principal.CompetitionId, SeasonId: fixture.principal.SeasonId}},
			{name: "competition", principal: &Principal{Id: "synthetic-operator", Role: "operator", Environment: fixture.principal.Environment, CompetitionId: "synthetic-other", SeasonId: fixture.principal.SeasonId}},
			{name: "season", principal: &Principal{Id: "synthetic-operator", Role: "operator", Environment: fixture.principal.Environment, CompetitionId: fixture.principal.CompetitionId, SeasonId: "synthetic-other"}},
		} {
			if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, check.principal, fixture.request); err == nil || receipt != nil {
				t.Fatalf("%s principal consumed recovery authority: receipt=%+v error=%v", check.name, receipt, err)
			}
			if after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(after, before) {
				t.Fatalf("%s principal partially changed immutable recovery state", check.name)
			}
		}
	})
}

// Self-consistent document hashes do not replace exact incident, terminal,
// baseline/source, runtime qualification, validity-window or single-use binding.
func TestCompetitionReevaluationPgRejectsConflictingAuthorizationEvidence(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, "authorization-evidence")
		before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		for _, change := range []struct {
			name   string
			mutate func(*model.CompetitionJobReevaluationAuthorization)
		}{
			{name: "unknown-incident-class", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.IncidentClass = "synthetic_unknown_incident" }},
			{name: "scope", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.SeasonId = "synthetic-other-season" }},
			{name: "job", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.JobId = server.NewId() }},
			{name: "round", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.RoundId = server.NewId() }},
			{name: "cycle", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.ExpectedCycle = 1 }},
			{name: "attempt", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.ExpectedAttemptCount++ }},
			{name: "terminal-hash", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
				a.ExpectedTerminalSha256 = strings.Repeat("e", 64)
			}},
			{name: "evidence", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.EvidenceSha256 = "" }},
			{name: "incident-event", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.IncidentEventId++ }},
			{name: "incident-event-hash", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
				a.IncidentEventSha256 = strings.Repeat("e", 64)
			}},
			{name: "incident-attempt", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.IncidentAttempt++ }},
			{name: "failure-manifest", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
				a.FailureManifestSha256 = strings.Repeat("e", 64)
			}},
			{name: "unapproved-rejected-path", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
				a.RejectedArtifactPath = "failed-evidence/vault/local/synthetic.yml"
			}},
			{name: "round-policy", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.RoundPolicySha256 = strings.Repeat("e", 64) }},
			{name: "baseline", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.BaselineSha256 = strings.Repeat("e", 64) }},
			{name: "runtime-qualification", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
				a.RuntimeQualificationSha256 = strings.Repeat("e", 64)
			}},
			{name: "fixed-source", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.FixedSourceCommit = strings.Repeat("e", 40) }},
			{name: "fixed-worker", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.FixedWorkerSha256 = strings.Repeat("e", 64) }},
			{name: "fixed-image", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
				a.FixedWorkerImageDigest = "sha256:" + strings.Repeat("e", 64)
			}},
			{name: "api-image", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
				a.ApiImageDigest = "sha256:" + strings.Repeat("e", 64)
			}},
			{name: "not-single-use", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.SingleUse = false }},
			{name: "expired", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
				a.AuthorizedAt = fixture.now.Add(-time.Hour)
				a.ExpiresAt = fixture.now.Add(-time.Second)
			}},
			{name: "future", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.AuthorizedAt = fixture.now.Add(time.Second) }},
		} {
			request := fixture.request
			change.mutate(&request.Authorization)
			var err error
			request.AuthorizationSha256, err = model.CompetitionAutomationDigest(request.Authorization)
			if err != nil {
				t.Fatal(err)
			}
			if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, request); err == nil || receipt != nil {
				t.Fatalf("conflicting %s gained recovery: receipt=%+v error=%v", change.name, receipt, err)
			}
			if after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected %s partially mutated the original job/evidence", change.name)
			}
		}
		request := fixture.request
		request.AuthorizationSha256 = strings.Repeat("e", 64)
		if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, request); err == nil || receipt != nil {
			t.Fatalf("wrong authorization document digest was accepted: receipt=%+v error=%v", receipt, err)
		}
		if after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(after, before) {
			t.Fatal("wrong authorization digest partially changed the original job")
		}
	})
}

// Global attempts advance through 4,5,6 while the retry schedule counts only
// this cycle's three attempts. Neither retry nor replay creates a second cycle.
func TestCompetitionReevaluationPgRetainsGlobalAttemptsAndLocalRetryBudget(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, "attempt-budget")
		before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request)
		if err != nil || receipt == nil {
			t.Fatalf("authorize recovered attempts: %v", err)
		}
		fixture.now = fixture.now.Add(2 * time.Hour)
		cycleStartedAt := fixture.now
		fixture.releaseHold(t, ctx)
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-reevaluation-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatalf("fresh supervisor for wrong-worker control: %v", err)
		}
		fixture.settings.automationLease = lease
		beforeWrongWorker := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		if wrong, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-reevaluation-worker", testWorkerImageDigest()); !errors.Is(err, ErrLeaseLost) || wrong != nil {
			t.Fatalf("unapproved old worker claimed a recovered cycle: job=%+v error=%v", wrong, err)
		}
		if afterWrongWorker := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(afterWrongWorker, beforeWrongWorker) {
			t.Fatal("unapproved worker partially claimed or consumed a recovered attempt")
		}
		for localAttempt := 1; localAttempt <= 3; localAttempt++ {
			job := fixture.releaseAndClaim(t, ctx)
			if job.AttemptCount != receipt.AttemptOffset+localAttempt || job.EvaluationCycle != 1 || job.CycleAttemptOffset != receipt.AttemptOffset ||
				job.StartedAt == nil || !job.StartedAt.Equal(*fixture.job.StartedAt) || job.CycleStartedAt == nil || !job.CycleStartedAt.Equal(cycleStartedAt) ||
				job.RoundBaseline == nil || job.RoundBaseline.Sha256 != fixture.job.RoundBaseline.Sha256 {
				t.Fatalf("recovered claim %d changed global attempt, original start, one cycle start or frozen baseline: %+v", localAttempt, job)
			}
			completedAt := fixture.now
			retry, err := fixture.store.Complete(withCompetitionJobAuthority(ctx, job), fixture.settings, "synthetic-reevaluation-worker", job.JobId,
				EvaluationOutcome{Error: infrastructureError("artifact_archive_failed", "synthetic recovered archive retry")})
			if err != nil || retry != (localAttempt < 3) {
				t.Fatalf("recovered local attempt %d: retry=%t error=%v", localAttempt, retry, err)
			}
			if retry {
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT available_at FROM competition_job WHERE job_id=$1`, job.JobId).Scan(&fixture.now))
				})
				expected := completedAt.Add(time.Duration(localAttempt*localAttempt) * 15 * time.Second)
				if !fixture.now.Equal(expected) {
					t.Fatalf("local retry %d charged global attempt backoff: got %v, want %v", localAttempt, fixture.now, expected)
				}
			}
		}
		after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		requireCompetitionReevaluationOriginalEvidence(t, before, after)
		var state string
		var attempts, cycle int
		var failureJson []byte
		var auditCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT state,attempt_count,evaluation_cycle,eval_error_json FROM competition_job WHERE job_id=$1`, fixture.job.JobId).Scan(&state, &attempts, &cycle, &failureJson))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_job_reevaluation WHERE job_id=$1`, fixture.job.JobId).Scan(&auditCount))
		})
		var failure CompetitionError
		if err := json.Unmarshal(failureJson, &failure); err != nil {
			t.Fatal(err)
		}
		if state != "failed" || attempts != 6 || cycle != 1 || failure.Retriable || auditCount != 1 {
			t.Fatalf("recovered cycle did not stop after three local attempts: state=%s global=%d cycle=%d failure=%+v audit=%d", state, attempts, cycle, failure, auditCount)
		}
		replayed, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request)
		if err != nil || !reflect.DeepEqual(replayed, receipt) {
			t.Fatalf("finished cycle exact replay lost immutable first receipt: receipt=%+v error=%v", replayed, err)
		}
		request := fixture.request
		request.IdempotencyKey += "-second-cycle"
		request.Authorization.ExpectedCycle, request.Authorization.ExpectedAttemptCount = 1, attempts
		request.Authorization.ExpectedTerminalSnapshot = json.RawMessage(after.job)
		request.Authorization.ExpectedTerminalSha256, err = CompetitionJobReevaluationSnapshotDigest(after.job)
		if err != nil {
			t.Fatal(err)
		}
		request.AuthorizationSha256, err = model.CompetitionAutomationDigest(request.Authorization)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, request); !errors.Is(err, ErrConflict) || result != nil {
			t.Fatalf("single-use approval granted a second cycle: receipt=%+v error=%v", result, err)
		}
		if final := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(final, after) {
			t.Fatal("finished-cycle replay or second-cycle conflict changed immutable history")
		}
	})
}

// The same worker/job identifiers cannot let an old cycle or earlier claim
// heartbeat, complete, hand back, or indirectly release the current slot.
func TestCompetitionReevaluationPgRejectsStaleCycleAndAttemptAuthority(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, "stale-authority")
		if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request); err != nil || receipt == nil {
			t.Fatalf("authorize stale-cycle control: %v", err)
		}
		current := fixture.releaseAndClaim(t, ctx)
		assertStale := func(name string, staleCtx context.Context) {
			before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
			leaseBefore := readCompetitionLeasePgSnapshot(ctx, fixture.job.JobId)
			if err := fixture.store.Heartbeat(staleCtx, fixture.settings, "synthetic-reevaluation-worker", fixture.job.JobId); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("%s renewed a later claim: %v", name, err)
			}
			if retry, err := fixture.store.Complete(staleCtx, fixture.settings, "synthetic-reevaluation-worker", fixture.job.JobId,
				EvaluationOutcome{Error: terminalInfrastructureError("synthetic_stale_outcome", "synthetic stale completion")}); !errors.Is(err, ErrLeaseLost) || retry {
				t.Fatalf("%s completed a later claim: retry=%t error=%v", name, retry, err)
			}
			_ = fixture.store.HandBack(staleCtx, "synthetic-reevaluation-worker", fixture.job.JobId, "synthetic_stale_release")
			if after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(after, before) {
				t.Fatalf("%s changed the current job, audit or events", name)
			}
			if leaseAfter := readCompetitionLeasePgSnapshot(ctx, fixture.job.JobId); !reflect.DeepEqual(leaseAfter, leaseBefore) {
				t.Fatalf("%s extended, cleared or released the current slot", name)
			}
		}
		assertStale("old-cycle", withCompetitionJobAuthority(ctx, fixture.job))
		assertStale("unfenced-legacy-call", ctx)
		if err := fixture.store.HandBack(withCompetitionJobAuthority(ctx, current), "synthetic-reevaluation-worker", current.JobId, "synthetic_current_release"); err != nil {
			t.Fatalf("hand back current recovered claim: %v", err)
		}
		next := fixture.releaseAndClaim(t, ctx)
		if next.AttemptCount != current.AttemptCount+1 || next.EvaluationCycle != current.EvaluationCycle || next.CycleStartedAt == nil ||
			current.CycleStartedAt == nil || !next.CycleStartedAt.Equal(*current.CycleStartedAt) {
			t.Fatalf("same-cycle successor changed its cycle budget: before=%+v after=%+v", current, next)
		}
		assertStale("old-attempt", withCompetitionJobAuthority(ctx, current))
		detachedCtx, cancelDetached := context.WithTimeout(context.WithoutCancel(withCompetitionJobAuthority(ctx, current)), time.Minute)
		defer cancelDetached()
		assertStale("old-detached-attempt", detachedCtx)
		if err := fixture.store.Heartbeat(withCompetitionJobAuthority(ctx, next), fixture.settings, "synthetic-reevaluation-worker", next.JobId); err != nil {
			t.Fatalf("stale-call controls also blocked exact current authority: %v", err)
		}
	})
}

// A retry whose backoff ends exactly at the one recovered three-hour boundary
// is terminal, even with a healthy worker lease and fresh supervisor authority.
func TestCompetitionReevaluationPgDoesNotExtendThreeHourCycleForRetry(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, "cycle-boundary")
		if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request); err != nil || receipt == nil {
			t.Fatalf("authorize bounded recovered cycle: %v", err)
		}
		fixture.settings.WorkerLeaseSeconds = 4 * 60 * 60
		job := fixture.releaseAndClaim(t, ctx)
		if job.CycleStartedAt == nil {
			t.Fatal("first recovered claim did not start its cycle")
		}
		fixture.now = job.CycleStartedAt.Add(3*time.Hour - 15*time.Second)
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-reevaluation-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatalf("refresh supervisor independently of execution time: %v", err)
		}
		fixture.settings.automationLease = lease
		if retry, err := fixture.store.Complete(withCompetitionJobAuthority(ctx, job), fixture.settings, "synthetic-reevaluation-worker", job.JobId,
			EvaluationOutcome{Error: infrastructureError("synthetic_retryable_failure", "synthetic cycle-boundary retry")}); err != nil || retry {
			t.Fatalf("exact cycle-boundary retry gained fresh execution: retry=%t error=%v", retry, err)
		}
		var state string
		var failureJson []byte
		var startedAt, cycleStartedAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT state,eval_error_json,started_at,cycle_started_at FROM competition_job WHERE job_id=$1`, job.JobId).Scan(&state, &failureJson, &startedAt, &cycleStartedAt))
		})
		var failure CompetitionError
		if err := json.Unmarshal(failureJson, &failure); err != nil {
			t.Fatal(err)
		}
		if state != "failed" || failure.Kind != "infrastructure" || failure.Code != "evaluation_time_budget_exhausted" || failure.Retriable ||
			!startedAt.Equal(*fixture.job.StartedAt) || !cycleStartedAt.Equal(*job.CycleStartedAt) {
			t.Fatalf("cycle boundary changed original start, budget or terminal cause: state=%s failure=%+v", state, failure)
		}
	})
}

// A zero-row transition after the audit INSERT must roll the entire
// transaction back, including its receipt and append-only authorization event.
func TestCompetitionReevaluationPgRollsBackSuppressedTransition(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, "suppressed-transition")
		before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_suppress_recovery() RETURNS trigger AS $body$
			 BEGIN RETURN NULL; END $body$ LANGUAGE plpgsql;
			 CREATE TRIGGER synthetic_suppress_recovery BEFORE UPDATE ON competition_job
			 FOR EACH ROW WHEN (OLD.state='failed' AND NEW.state='queued') EXECUTE FUNCTION synthetic_suppress_recovery();`))
		})
		if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request); !errors.Is(err, ErrConflict) || receipt != nil {
			t.Fatalf("suppressed recovery transition did not fail atomically: receipt=%+v error=%v", receipt, err)
		}
		if after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(after, before) {
			t.Fatal("failed transition committed an audit, event, cleared failure or queued state")
		}
	})
}

// A real serialization failure at commit reruns the public operation; only
// one immutable receipt/audit/event may survive the successful transaction.
func TestCompetitionReevaluationPgCommitRerunRecordsOneAudit(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, "commit-rerun")
		before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		armCompetitionCommitFault(ctx, "reevaluation", "competition_job", "UPDATE", "OLD.state='failed' AND NEW.state='queued'")
		receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request)
		if err != nil || receipt == nil {
			t.Fatalf("rerun recovery did not publish its committed receipt: receipt=%+v error=%v", receipt, err)
		}
		var audits, events int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_job_reevaluation WHERE job_id=$1`, fixture.job.JobId).Scan(&audits))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_job_event WHERE job_id=$1 AND event_type='reevaluation_authorized'`, fixture.job.JobId).Scan(&events))
		})
		if faults := competitionCommitFaultCount(ctx, "reevaluation"); faults < 2 || audits != 1 || events != 1 {
			t.Fatalf("recovery commit rerun duplicated or missed history: attempts=%d audits=%d events=%d", faults, audits, events)
		}
		requireCompetitionReevaluationOriginalEvidence(t, before, readCompetitionReevaluationPgSnapshot(ctx, fixture.job))
	})
}

// The stored authorization and original terminal row are permanently
// append-only, independently of later mutable job state and local settings.
func TestCompetitionReevaluationPgAuditAndOriginalSnapshotRemainImmutable(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, "immutable-audit")
		original := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request); err != nil || receipt == nil {
			t.Fatalf("authorize immutable audit control: %v", err)
		}
		before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		var terminalJson []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT original_terminal_json FROM competition_job_reevaluation WHERE job_id=$1`, fixture.job.JobId).Scan(&terminalJson))
		})
		if !bytes.Equal(terminalJson, original.job) {
			t.Fatal("immutable audit did not retain the full exact original terminal jsonb row")
		}
		for _, check := range []struct {
			name  string
			query string
		}{
			{name: "operator", query: `UPDATE competition_job_reevaluation SET operator_id='synthetic-other' WHERE job_id=$1`},
			{name: "authorization", query: `UPDATE competition_job_reevaluation SET authorization_json='{}'::jsonb WHERE job_id=$1`},
			{name: "original-terminal", query: `UPDATE competition_job_reevaluation SET original_terminal_json='{}'::jsonb WHERE job_id=$1`},
			{name: "delete", query: `DELETE FROM competition_job_reevaluation WHERE job_id=$1`},
		} {
			err := captureDatabaseError(func() {
				server.Tx(ctx, func(tx server.PgTx) { server.RaisePgResult(tx.Exec(ctx, check.query, fixture.job.JobId)) }, server.OptNoRetry())
			})
			if err == nil {
				t.Fatalf("direct %s mutation rewrote immutable reevaluation authority", check.name)
			}
			if after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected %s mutation changed the audit or original evidence", check.name)
			}
		}
	})
}

// A past archive incident cannot be used to resample a scored result, genuine
// submission failure, unknown infrastructure failure or unverified time budget.
func TestCompetitionReevaluationPgRejectsScoredGenuineAndUnknownFailures(t *testing.T) {
	for _, check := range []struct {
		name     string
		verified bool
		outcome  func(testing.TB, *Settings, *queuedJob) EvaluationOutcome
	}{
		{name: "scored", verified: true, outcome: func(t testing.TB, settings *Settings, job *queuedJob) EvaluationOutcome {
			score := competitionRankingScore(90, 110)
			score.Significance.ReplicateCount = settings.EvaluationPolicy.Replicates
			return testSharedBaselineOutcome(t, settings, job, EvaluationOutcome{Score: score})
		}},
		{name: "genuine", verified: true, outcome: func(testing.TB, *Settings, *queuedJob) EvaluationOutcome {
			return EvaluationOutcome{Error: submissionError("synthetic_invalid_candidate", "synthetic genuine candidate failure")}
		}},
		{name: "unknown", verified: true, outcome: func(testing.TB, *Settings, *queuedJob) EvaluationOutcome {
			return EvaluationOutcome{Error: terminalInfrastructureError("synthetic_unknown_failure", "synthetic unrelated infrastructure failure")}
		}},
		{name: "unverified-budget", verified: false, outcome: func(testing.TB, *Settings, *queuedJob) EvaluationOutcome {
			return EvaluationOutcome{Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "synthetic unverified expired execution")}
		}},
	} {
		runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
			fixture := newCompetitionReevaluationAuthorizationPgFixtureWithTerminal(t, ctx, check.name,
				func(settings *Settings, job *queuedJob) EvaluationOutcome { return check.outcome(t, settings, job) }, check.verified)
			before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
			if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request); err == nil || receipt != nil {
				t.Fatalf("%s terminal outcome gained resampling authority: receipt=%+v error=%v", check.name, receipt, err)
			}
			if after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(after, before) {
				t.Fatalf("refused %s resampling changed terminal evidence", check.name)
			}
		})
	}
}

// The additive migration must run while an old release's cycle-zero claim,
// heartbeat, handback and completion SQL shapes still serve unchanged.
func TestCompetitionReevaluationPgMigrationPreservesLegacyCycleZeroWriters(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.ApplyDbMigrationsUpTo(ctx, server.CompetitionJobReevaluationMigrationVersion()-1)
		fixture := newCompetitionRankingFixture(t, true)
		job := fixture.addJob(t)
		server.ApplyDbMigrations(ctx)
		before := readCompetitionReevaluationPgSnapshot(ctx, job)
		server.Tx(ctx, func(tx server.PgTx) {
			// These old query shapes intentionally set no new cycle/attempt GUC.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_worker_slot SET lease_expires_at=$1,heartbeat_at=$2 WHERE slot_id=1 AND worker_id='ranking-worker' AND job_id=$3`, fixture.now.Add(time.Hour), fixture.now, job.JobId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_job SET lease_expires_at=$2 WHERE job_id=$1 AND state='running' AND lease_owner='ranking-worker'`, job.JobId, fixture.now.Add(time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_job SET state='queued',available_at=$2,lease_owner=NULL,lease_expires_at=NULL WHERE job_id=$1 AND state='running' AND lease_owner='ranking-worker'`, job.JobId, fixture.now))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_worker_slot SET worker_id=NULL,job_id=NULL,lease_expires_at=NULL,heartbeat_at=$1 WHERE slot_id=1 AND worker_id='ranking-worker' AND job_id=$2`, fixture.now, job.JobId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_job SET state='running',started_at=COALESCE(started_at,$2),lease_owner='ranking-worker',lease_expires_at=$3,attempt_count=attempt_count+1,worker_image_digest=$4 WHERE job_id=$1`, job.JobId, fixture.now, fixture.now.Add(time.Hour), testWorkerImageDigest()))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_worker_slot SET worker_id='ranking-worker',job_id=$1,lease_expires_at=$2,heartbeat_at=$3 WHERE slot_id=1`, job.JobId, fixture.now.Add(time.Hour), fixture.now))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_job SET state='failed',completed_at=$2,lease_owner=NULL,lease_expires_at=NULL,score_json=NULL,eval_error_json=$3::jsonb,artifact_manifest_json=NULL,artifact_manifest_sha256=NULL WHERE job_id=$1`, job.JobId, fixture.now, `{"kind":"infrastructure","code":"synthetic_legacy_failure","message":"synthetic old release outcome","retriable":false}`))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_worker_slot SET worker_id=NULL,job_id=NULL,lease_expires_at=NULL,heartbeat_at=$1 WHERE slot_id=1 AND worker_id='ranking-worker' AND job_id=$2`, fixture.now, job.JobId))
		})
		var cycle, offset, attempts int
		var cycleStartedAt *time.Time
		var state string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT evaluation_cycle,cycle_attempt_offset,cycle_started_at,attempt_count,state FROM competition_job WHERE job_id=$1`, job.JobId).Scan(&cycle, &offset, &cycleStartedAt, &attempts, &state))
		})
		if cycle != 0 || offset != 0 || cycleStartedAt != nil || attempts != 2 || state != "failed" {
			t.Fatalf("additive migration changed old writer semantics: cycle=%d offset=%d start=%v attempts=%d state=%s", cycle, offset, cycleStartedAt, attempts, state)
		}
		after := readCompetitionReevaluationPgSnapshot(ctx, job)
		requireCompetitionReevaluationOriginalEvidence(t, before, after)
		if !bytes.Equal(after.audit, []byte("[]")) {
			t.Fatal("cycle-zero old release writes manufactured reevaluation authority")
		}
	})
}

// Even an otherwise exact request cannot proceed without its independently
// qualified release and the current explicit evaluation hold.
func TestCompetitionReevaluationPgRequiresQualifiedRuntimeAndEvaluationHold(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, "runtime-and-hold")
		before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		for _, change := range []struct {
			name   string
			mutate func(*Settings)
		}{
			{name: "automation-disabled", mutate: func(s *Settings) { s.AutomationPolicy.Enabled = false }},
			{name: "unbound-runtime", mutate: func(s *Settings) { s.reevaluationRuntime = nil }},
			{name: "qualification-hash", mutate: func(s *Settings) { s.reevaluationRuntimeSha256 = strings.Repeat("e", 64) }},
			{name: "acceptance", mutate: func(s *Settings) { s.reevaluationRuntime.Accepted = false }},
			{name: "api-compatibility", mutate: func(s *Settings) { s.reevaluationRuntime.ApiCompatible = false }},
			{name: "worker-compatibility", mutate: func(s *Settings) { s.reevaluationRuntime.WorkerCompatible = false }},
			{name: "changed-evidence", mutate: func(s *Settings) { s.reevaluationRuntime.EvidenceSha256 = strings.Repeat("e", 64) }},
		} {
			settings := *fixture.settings
			proof := *fixture.settings.reevaluationRuntime
			settings.reevaluationRuntime = &proof
			change.mutate(&settings)
			if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, &settings, fixture.principal, fixture.request); err == nil || receipt != nil {
				t.Fatalf("%s runtime authorized recovery: receipt=%+v error=%v", change.name, receipt, err)
			}
			if after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected %s runtime changed persisted recovery state", change.name)
			}
		}
		fixture.releaseHold(t, ctx)
		if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request); !errors.Is(err, ErrAutomationHeld) || receipt != nil {
			t.Fatalf("recovery bypassed the explicit evaluation hold: receipt=%+v error=%v", receipt, err)
		}
		if after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(after, before) {
			t.Fatal("refused no-hold authorization changed original evidence or audit")
		}
	})
}

// Actual live work or an irreversibly sealed round blocks recovery despite a
// matching original terminal snapshot, fixed release and operator document.
func TestCompetitionReevaluationPgRejectsLiveWorkAndSealedRound(t *testing.T) {
	for _, mode := range []string{"live-work", "sealed-round"} {
		runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
			fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, mode)
			fixture.releaseHold(t, ctx)
			var other *queuedJob
			if mode == "live-work" {
				patch, patchError := ValidateAndCanonicalizePatch(testPatch("synthetic-competing-live-work"), fixture.settings.PatchPolicy)
				if patchError != nil {
					t.Fatal(patchError)
				}
				if _, _, err := fixture.store.Enqueue(ctx, fixture.settings, fixture.round.RoundId, patch,
					"synthetic-other-submitter", testApiImageDigest()); err != nil {
					t.Fatal(err)
				}
				var err error
				other, err = fixture.store.Claim(ctx, fixture.settings, "synthetic-other-worker", fixture.runtime.WorkerImageDigest)
				if err != nil || other == nil || other.JobId == fixture.job.JobId {
					t.Fatalf("claim competing synthetic work: job=%+v error=%v", other, err)
				}
			} else {
				authorizationClock := fixture.now
				fixture.now = fixture.round.ClosesAt.Add(time.Second)
				if seal, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId); err != nil || seal == nil {
					t.Fatalf("seal actual production-v2 round: seal=%+v error=%v", seal, err)
				}
				// A stale in-window clock must not reopen an already sealed round.
				fixture.now = authorizationClock
			}
			args := fixture.hold.Request
			args.Active, args.ExpectedRevision = true, fixture.hold.Revision
			args.IdempotencyKey = "synthetic-recovery-rehold-" + mode
			args.ReasonCode = "OPERATOR_HOLD"
			var err error
			fixture.hold, err = fixture.store.SetAutomationHold(ctx, fixture.settings, fixture.principal.Id, args)
			if err != nil {
				t.Fatalf("rehold synthetic %s control: %v", mode, err)
			}
			before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
			var otherBefore competitionLeasePgSnapshot
			if other != nil {
				otherBefore = readCompetitionLeasePgSnapshot(ctx, other.JobId)
			}
			if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request); err == nil || receipt != nil {
				t.Fatalf("%s allowed recovery: receipt=%+v error=%v", mode, receipt, err)
			}
			if after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(after, before) {
				t.Fatalf("refused %s recovery changed original evidence", mode)
			}
			if other != nil {
				if otherAfter := readCompetitionLeasePgSnapshot(ctx, other.JobId); !reflect.DeepEqual(otherAfter, otherBefore) {
					t.Fatal("refused recovery changed another live job or its slot")
				}
			}
		})
	}
}

// Queue age and completed-duration gauges use the recovered cycle, while the
// original admitted start remains unchanged for private history and audit.
func TestCompetitionReevaluationPgMetricsUseRecoveredCycleAnchors(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, "metrics-cycle")
		if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request); err != nil || receipt == nil {
			t.Fatalf("authorize metrics recovery: %v", err)
		}
		var availableAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT available_at FROM competition_job WHERE job_id=$1`, fixture.job.JobId).Scan(&availableAt))
		})
		queued, err := fixture.store.GetJob(ctx, fixture.settings, fixture.job.JobId, fixture.principal)
		if err != nil || queued == nil || queued.Reevaluation == nil || queued.Reevaluation.StartedAt != nil ||
			queued.CycleStartedAt != nil || queued.StartedAt == nil || !queued.StartedAt.Equal(*fixture.job.StartedAt) {
			t.Fatalf("queued recovery exposed an effective old cycle start: job=%+v error=%v", queued, err)
		}
		beforeRefresh := server.NowUtc()
		if err := refreshOperationalMetrics(ctx, fixture.settings); err != nil {
			t.Fatal(err)
		}
		afterRefresh := server.NowUtc()
		queuedAge := testutil.ToFloat64(competitionOldestJobAge.WithLabelValues("queued"))
		if queuedAge < beforeRefresh.Sub(availableAt).Seconds() || afterRefresh.Sub(availableAt).Seconds() < queuedAge ||
			testutil.ToFloat64(competitionSubmissionQueueSize) != 1 || testutil.ToFloat64(competitionCurrentEvaluationElapsed) != 0 {
			t.Fatalf("queued recovery metrics borrowed the original admission/execution clock: age=%f", queuedAge)
		}
		fixture.settings.WorkerLeaseSeconds = 600
		job := fixture.releaseAndClaim(t, ctx)
		if job.CycleStartedAt == nil {
			t.Fatal("metrics fixture lacks its actual first recovered claim time")
		}
		var effectiveStart time.Time
		var projectedElapsed float64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(cycle_started_at,started_at),
			 extract(epoch FROM ($2::timestamp-COALESCE(cycle_started_at,started_at)))::double precision
			 FROM competition_job WHERE job_id=$1 AND state='running'`, job.JobId, job.CycleStartedAt.Add(time.Minute)).Scan(&effectiveStart, &projectedElapsed))
		})
		if !effectiveStart.Equal(*job.CycleStartedAt) || projectedElapsed != 60 || !job.StartedAt.Equal(*fixture.job.StartedAt) {
			t.Fatal("running projection used the expired original start instead of the recovered cycle")
		}
		beforeRefresh = server.NowUtc()
		if err := refreshOperationalMetrics(ctx, fixture.settings); err != nil {
			t.Fatal(err)
		}
		afterRefresh = server.NowUtc()
		elapsed := testutil.ToFloat64(competitionCurrentEvaluationElapsed)
		if elapsed < math.Max(0, beforeRefresh.Sub(*job.CycleStartedAt).Seconds()) ||
			math.Max(0, afterRefresh.Sub(*job.CycleStartedAt).Seconds()) < elapsed {
			t.Fatalf("running gauge borrowed a previous execution clock: %f", elapsed)
		}
		fixture.now = job.CycleStartedAt.Add(2 * time.Minute)
		outcome := testSharedBaselineOutcome(t, fixture.settings, job, EvaluationOutcome{Score: competitionRankingScore(90, 110)})
		if retry, err := fixture.store.Complete(withCompetitionJobAuthority(ctx, job), fixture.settings, "synthetic-reevaluation-worker", job.JobId, outcome); err != nil || retry {
			t.Fatalf("complete deterministic two-minute recovered metrics sample: retry=%t error=%v", retry, err)
		}
		var priorDuration float64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT extract(epoch FROM (completed_at-started_at))::double precision
			 FROM competition_job WHERE round_id=$1 AND job_id<>$2 AND state='succeeded'`, job.RoundId, job.JobId).Scan(&priorDuration))
		})
		if priorDuration >= 120 {
			t.Fatal("synthetic prior scored fixture no longer has its short deterministic duration")
		}
		if err := refreshOperationalMetrics(ctx, fixture.settings); err != nil {
			t.Fatal(err)
		}
		expected := priorDuration + (120-priorDuration)*0.75
		if estimate := testutil.ToFloat64(competitionEvaluationDurationEstimate); estimate != expected {
			t.Fatalf("completed estimate charged the old failed three-hour cycle: got %f, want %f", estimate, expected)
		}
	})
}

// Trusted claim maintenance can reclaim both expired authorities of a
// canceled recovered round, but never a live job or an unfenced supervisor.
func TestCompetitionReevaluationPgReclaimsOnlyExpiredFencedRecoverySlot(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, "expired-slot")
		if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request); err != nil || receipt == nil {
			t.Fatalf("authorize expired-slot control: %v", err)
		}
		job := fixture.releaseAndClaim(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(setCompetitionJobAuthorityInTx(ctx, tx, job.JobId, job.EvaluationCycle, job.AttemptCount))
			// Deliberately expire only the slot; the independently live job
			// lease must still prevent clearing or replacing its authority.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_worker_slot SET lease_expires_at=$2 WHERE slot_id=1 AND job_id=$1`, job.JobId, fixture.now))
		})
		before := readCompetitionReevaluationPgSnapshot(ctx, job)
		leaseBefore := readCompetitionLeasePgSnapshot(ctx, job.JobId)
		if claimed, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-replacement-worker", fixture.runtime.WorkerImageDigest); !errors.Is(err, ErrLeaseLost) || claimed != nil {
			t.Fatalf("expired slot erased a still-live recovered job: job=%+v error=%v", claimed, err)
		}
		if after := readCompetitionReevaluationPgSnapshot(ctx, job); !reflect.DeepEqual(after, before) {
			t.Fatal("refused live-job takeover changed its row, budget, history or audit")
		}
		if after := readCompetitionLeasePgSnapshot(ctx, job.JobId); !reflect.DeepEqual(after, leaseBefore) {
			t.Fatal("refused live-job takeover changed the expired slot or live job lease")
		}
		fixture.now = *job.LeaseExpiresAt
		for _, mode := range []string{"missing-supervisor", "stale-supervisor"} {
			settings := *fixture.settings
			if mode == "missing-supervisor" {
				settings.automationLease = nil
			} else {
				lease := *fixture.settings.automationLease
				lease.FencingToken--
				settings.automationLease = &lease
			}
			if claimed, err := fixture.store.Claim(ctx, &settings, "synthetic-replacement-worker", fixture.runtime.WorkerImageDigest); !errors.Is(err, ErrLeaseLost) || claimed != nil {
				t.Fatalf("%s reclaimed an expired recovered slot: job=%+v error=%v", mode, claimed, err)
			}
			if after := readCompetitionReevaluationPgSnapshot(ctx, job); !reflect.DeepEqual(after, before) {
				t.Fatalf("%s mutated the old recovered job or budget", mode)
			}
			if after := readCompetitionLeasePgSnapshot(ctx, job.JobId); !reflect.DeepEqual(after, leaseBefore) {
				t.Fatalf("%s cleared or replaced the recovered slot", mode)
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_round SET canceled=true WHERE round_id=$1`, job.RoundId))
		})
		canceledBefore := readCompetitionReevaluationPgSnapshot(ctx, job)
		if claimed, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-replacement-worker", fixture.runtime.WorkerImageDigest); err != nil || claimed != nil {
			t.Fatalf("trusted expired-slot cleanup could not drain a canceled recovered round: job=%+v error=%v", claimed, err)
		}
		if after := readCompetitionReevaluationPgSnapshot(ctx, job); !reflect.DeepEqual(after, canceledBefore) {
			t.Fatal("expired-slot maintenance mutated the old recovered row, attempts, clock, events or audit")
		}
		cleared := readCompetitionLeasePgSnapshot(ctx, job.JobId)
		if cleared.slotWorker != nil || cleared.slotJob != nil || cleared.slotExpires != nil ||
			cleared.attempts != leaseBefore.attempts || !reflect.DeepEqual(cleared.started, leaseBefore.started) ||
			!reflect.DeepEqual(cleared.jobExpires, leaseBefore.jobExpires) || !reflect.DeepEqual(cleared.jobOwner, leaseBefore.jobOwner) {
			t.Fatal("trusted maintenance did not clear only the exact expired singleton slot")
		}
	})
}
