// Legacy archive interruptions receive only an explicitly approved new cycle;
// the old terminal, local diagnostics and every durable admission remain intact.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Actual Complete emits the untyped earlier archive retry. Remove the old
// fixture's unused inventory manifest before constructing the distinct class.
func newCompetitionLegacyArchivePgFixture(t testing.TB, ctx context.Context, name string,
	terminal func(*Settings, *queuedJob) EvaluationOutcome, legacy bool) *competitionReevaluationAuthorizationPgFixture {
	t.Helper()
	if terminal == nil {
		terminal = func(*Settings, *queuedJob) EvaluationOutcome {
			return EvaluationOutcome{Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "synthetic legacy archive interruption consumed original time")}
		}
	}
	fixture := newCompetitionReevaluationAuthorizationPgFixtureWithArchiveEvidence(t, ctx, name, terminal, true, legacy)
	a := &fixture.request.Authorization
	manifest := filepath.Join(fixture.settings.ArtifactRoot, fixture.job.JobId.String(), fmt.Sprintf("attempt-%02d", a.IncidentAttempt), "failed-evidence-manifest.json")
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	a.IncidentClass = model.CompetitionJobReevaluationLegacyArchiveBudget
	a.FailureManifestSha256, a.RejectedArtifactPath = "", ""
	var err error
	fixture.request.AuthorizationSha256, err = model.CompetitionAutomationDigest(*a)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

// An uncertified local G2-negative score is preserved as diagnostics, never
// loaded as an accepted score or used to choose which infrastructure jobs retry.
func TestCompetitionReevaluationLegacyPgPreservesUnacceptedLocalScoreAndOriginalFailure(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionLegacyArchivePgFixture(t, ctx, "legacy-exact", nil, true)
		before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		local := competitionRankingScore(90, 110)
		local.Placeable = false
		local.Gates["G2"] = Gate{Passed: false, Details: map[string]any{"synthetic_floor_refusal": true}}
		content, err := json.Marshal(local)
		if err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join(fixture.settings.ArtifactRoot, fixture.job.JobId.String(), "attempt-01")
		localPath := filepath.Join(directory, "score.json")
		if err := os.WriteFile(localPath, content, 0o400); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(directory, "failed-evidence-manifest.json")); !os.IsNotExist(err) {
			t.Fatalf("legacy approval still depended on invented inventory evidence: %v", err)
		}
		receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request)
		if err != nil || receipt == nil {
			t.Fatalf("explicit bound legacy approval: receipt=%+v error=%v", receipt, err)
		}
		if receipt.JobId != fixture.job.JobId || receipt.Cycle != 1 || receipt.AttemptOffset != fixture.job.AttemptCount || receipt.StartedAt != nil ||
			receipt.ExecutionTimeoutSeconds != 3*60*60 || receipt.PreviousFailure.Code != "evaluation_time_budget_exhausted" ||
			receipt.PreviousFailure.Kind != "infrastructure" || receipt.PreviousFailure.Retriable ||
			receipt.PreviousTerminalSha256 != fixture.request.Authorization.ExpectedTerminalSha256 ||
			model.ValidateCompetitionJobReevaluationCycle(&receipt.CompetitionJobReevaluationCycle) != nil {
			t.Fatalf("legacy approval changed the original failure or expanded authority: %+v", receipt)
		}
		saved, err := fixture.store.GetJob(ctx, fixture.settings, fixture.job.JobId, fixture.principal)
		if err != nil || saved == nil || saved.State != "queued" || saved.Score != nil || saved.EvalError != nil ||
			saved.AttemptCount != fixture.job.AttemptCount || saved.EvaluationCycle != 1 || saved.CycleAttemptOffset != fixture.job.AttemptCount ||
			saved.CycleStartedAt != nil || saved.StartedAt == nil || !saved.StartedAt.Equal(*fixture.job.StartedAt) {
			t.Fatalf("legacy approval reset original execution identity or claimed a new attempt: job=%+v error=%v", saved, err)
		}
		after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		requireCompetitionReevaluationOriginalEvidence(t, before, after)
		var original []byte
		var incidentClass string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT original_terminal_json,authorization_json->>'incident_class' FROM competition_job_reevaluation WHERE job_id=$1`, fixture.job.JobId).Scan(&original, &incidentClass))
		})
		if !bytes.Equal(original, before.job) || incidentClass != model.CompetitionJobReevaluationLegacyArchiveBudget {
			t.Fatal("legacy approval rewrote the original snapshot or claimed a typed inventory diagnosis")
		}
		fixture.now = fixture.request.Authorization.ExpiresAt.Add(time.Minute)
		replayed, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request)
		if err != nil || !reflect.DeepEqual(replayed, receipt) {
			t.Fatalf("exact expired approval replay changed the existing cycle: receipt=%+v error=%v", replayed, err)
		}
		conflict := fixture.request
		conflict.IdempotencyKey += "-conflict"
		if duplicate, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, conflict); err == nil || duplicate != nil {
			t.Fatal("a second operator key granted another legacy cycle")
		}
		if current := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(current, after) {
			t.Fatal("legacy replay/refused duplicate appended history or changed authority")
		}
		if current, err := os.ReadFile(localPath); err != nil || !bytes.Equal(current, content) {
			t.Fatal("legacy approval changed the retained unaccepted local G2 outcome")
		}
	})
}

// The actual immutable event must match the selected job/attempt/time and be
// an untyped archive retry, not merely any event from a budget-expired job.
func TestCompetitionReevaluationLegacyPgRejectsUnboundArchiveEvidence(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionLegacyArchivePgFixture(t, ctx, "legacy-evidence", nil, true)
		for _, check := range []struct {
			name, eventType string
			errorCode       any
			attempt         int
			typed           bool
			at              time.Time
		}{
			{name: "budget-only", eventType: "infrastructure_retry", errorCode: "evaluation_time_budget_exhausted", attempt: 1, at: *fixture.job.StartedAt},
			{name: "other-cause", eventType: "infrastructure_retry", errorCode: "synthetic_other_failure", attempt: 1, at: *fixture.job.StartedAt},
			{name: "nonstring-cause", eventType: "infrastructure_retry", errorCode: 17, attempt: 1, at: *fixture.job.StartedAt},
			{name: "terminal-event", eventType: "failed", errorCode: "artifact_archive_failed", attempt: 1, at: *fixture.job.StartedAt},
			{name: "typed-other-class", eventType: "infrastructure_retry", errorCode: "artifact_archive_failed", attempt: 1, typed: true, at: *fixture.job.StartedAt},
			{name: "wrong-attempt", eventType: "infrastructure_retry", errorCode: "artifact_archive_failed", attempt: 2, at: *fixture.job.StartedAt},
			{name: "predated-execution", eventType: "infrastructure_retry", errorCode: "artifact_archive_failed", attempt: 1, at: fixture.job.StartedAt.Add(-time.Second)},
			{name: "after-terminal", eventType: "infrastructure_retry", errorCode: "artifact_archive_failed", attempt: 1, at: fixture.now},
		} {
			actor := "synthetic-legacy-proof-" + check.name
			payload := map[string]any{"attempt": check.attempt, "error_code": check.errorCode}
			if check.typed {
				payload["artifact_archive_failure"] = map[string]any{"phase": "authenticate_failed_evidence", "category": "other", "detail_sha256": strings.Repeat("c", 64)}
			}
			server.Tx(ctx, func(tx server.PgTx) {
				appendEvent(ctx, tx, fixture.job.JobId, check.at, check.eventType, actor, payload)
			})
			request := fixture.request
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT event_id,payload_sha256 FROM competition_job_event WHERE job_id=$1 AND actor_id=$2 ORDER BY event_id DESC LIMIT 1`, fixture.job.JobId, actor).Scan(&request.Authorization.IncidentEventId, &request.Authorization.IncidentEventSha256))
			})
			var err error
			request.AuthorizationSha256, err = model.CompetitionAutomationDigest(request.Authorization)
			if err != nil {
				t.Fatal(err)
			}
			before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
			if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, request); err == nil || receipt != nil {
				t.Fatalf("%s fabricated legacy recovery authority", check.name)
			}
			if after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(after, before) {
				t.Fatalf("refused %s changed the old job or audit", check.name)
			}
		}
		request := fixture.request
		request.Authorization.IncidentEventSha256 = strings.Repeat("e", 64)
		var err error
		request.AuthorizationSha256, err = model.CompetitionAutomationDigest(request.Authorization)
		if err != nil {
			t.Fatal(err)
		}
		before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, request); err == nil || receipt != nil {
			t.Fatal("another event digest granted legacy recovery")
		}
		if after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(after, before) {
			t.Fatal("a rejected event digest changed legacy state")
		}
	})
}

// Even real prior archive trouble cannot resample a durable score, genuine
// gate failure or terminal archive failure belonging to another incident class.
func TestCompetitionReevaluationLegacyPgRefusesAcceptedScoresAndOtherFailureClasses(t *testing.T) {
	for _, check := range []struct {
		name   string
		legacy bool
		result func(testing.TB, *Settings, *queuedJob) EvaluationOutcome
	}{
		{name: "scored", legacy: true, result: func(t testing.TB, settings *Settings, job *queuedJob) EvaluationOutcome {
			score := competitionRankingScore(90, 110)
			score.Significance.ReplicateCount = settings.EvaluationPolicy.Replicates
			return testSharedBaselineOutcome(t, settings, job, EvaluationOutcome{Score: score})
		}},
		{name: "gate-failed", legacy: true, result: func(testing.TB, *Settings, *queuedJob) EvaluationOutcome {
			return EvaluationOutcome{Error: submissionError("run_unstable", "synthetic official gate refusal")}
		}},
		{name: "archive-terminal", legacy: true, result: func(testing.TB, *Settings, *queuedJob) EvaluationOutcome {
			return EvaluationOutcome{Error: terminalInfrastructureError("artifact_archive_failed", "synthetic terminal uses a different approved class")}
		}},
	} {
		runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
			var terminal func(*Settings, *queuedJob) EvaluationOutcome
			if check.result != nil {
				terminal = func(settings *Settings, job *queuedJob) EvaluationOutcome { return check.result(t, settings, job) }
			}
			fixture := newCompetitionLegacyArchivePgFixture(t, ctx, "legacy-refuse-"+check.name, terminal, check.legacy)
			before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
			if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request); err == nil || receipt != nil {
				t.Fatalf("%s gained legacy resampling authority", check.name)
			}
			if after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(after, before) {
				t.Fatalf("refused %s changed the original outcome or evidence", check.name)
			}
		})
	}
}

// Recovery changes neither admission FIFO nor the exact-score admission tie
// break, even when a later submission finishes before the recovered entry.
func TestCompetitionReevaluationLegacyPgPreservesAdmissionOrderForEqualScoreElection(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		fixture := newCompetitionLegacyArchivePgFixture(t, ctx, "legacy-admission-order", nil, true)
		originalSubmittedAt := fixture.job.SubmittedAt
		const worker = "synthetic-admission-order-worker"
		enqueue := func(name string) server.Id {
			t.Helper()
			fixture.now = fixture.now.Add(time.Second)
			patch, patchError := ValidateAndCanonicalizePatch(testPatch(name), fixture.settings.PatchPolicy)
			if patchError != nil {
				t.Fatalf("canonicalize admission control: %v", patchError)
			}
			job, _, err := fixture.store.Enqueue(ctx, fixture.settings, fixture.round.RoundId, patch, name, testApiImageDigest())
			if err != nil || job == nil {
				t.Fatalf("enqueue admission control: job=%+v error=%v", job, err)
			}
			return job.JobId
		}
		completeEqualScore := func(job *queuedJob) {
			t.Helper()
			score := competitionRankingScore(50, 120)
			score.Significance.ReplicateCount = 9
			score.Significance.CandidateMeanRawScore = 51
			score.Gates = map[string]Gate{}
			for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources"} {
				score.Gates[name] = Gate{Passed: true, Details: map[string]any{}}
			}
			outcome := testSharedBaselineOutcome(t, fixture.settings, job, EvaluationOutcome{Score: score})
			content, err := json.MarshalIndent(score, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(content)
			outcome.ArtifactManifest, err = json.Marshal(artifactManifest{Schema: 1, JobId: job.JobId.String(), RoundId: job.RoundId.String(), Attempt: job.AttemptCount,
				PatchSha256: job.PatchSha256, Artifacts: []evaluationArtifact{{Path: "score.json", Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))}}})
			if err != nil {
				t.Fatal(err)
			}
			fixture.now = fixture.now.Add(time.Second)
			if retry, err := fixture.store.Complete(withCompetitionJobAuthority(ctx, job), fixture.settings, worker, job.JobId, outcome); err != nil || retry {
				t.Fatalf("complete equal eligible score: retry=%t error=%v", retry, err)
			}
		}
		fixture.releaseHold(t, ctx)
		laterId := enqueue("synthetic-later-equal-score")
		later, err := fixture.store.Claim(ctx, fixture.settings, worker, testWorkerImageDigest())
		if err != nil || later == nil || later.JobId != laterId {
			t.Fatalf("claim later entry before recovery: job=%+v error=%v", later, err)
		}
		completeEqualScore(later)
		hold := fixture.hold.Request
		hold.Active, hold.ExpectedRevision = true, fixture.hold.Revision
		hold.IdempotencyKey = "synthetic-admission-order-hold"
		fixture.hold, err = fixture.store.SetAutomationHold(ctx, fixture.settings, fixture.principal.Id, hold)
		if err != nil {
			t.Fatalf("hold before explicit recovery: %v", err)
		}
		controlId := enqueue("synthetic-later-fifo-control")
		controlAvailableAt := fixture.now
		fixture.now = fixture.now.Add(time.Second)
		receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request)
		if err != nil || receipt == nil || !controlAvailableAt.Before(receipt.AuthorizedAt) {
			t.Fatalf("authorize earlier original after later control is available: receipt=%+v error=%v", receipt, err)
		}
		fixture.releaseHold(t, ctx)
		recovered, err := fixture.store.Claim(ctx, fixture.settings, worker, fixture.runtime.WorkerImageDigest)
		if err != nil || recovered == nil || recovered.JobId != fixture.job.JobId || !recovered.SubmittedAt.Equal(originalSubmittedAt) {
			t.Fatalf("recovery lost original admission FIFO: job=%+v error=%v", recovered, err)
		}
		completeEqualScore(recovered)
		control, err := fixture.store.Claim(ctx, fixture.settings, worker, testWorkerImageDigest())
		if err != nil || control == nil || control.JobId != controlId {
			t.Fatalf("claim later FIFO control after recovered entry: job=%+v error=%v", control, err)
		}
		fixture.now = fixture.now.Add(time.Second)
		if retry, err := fixture.store.Complete(withCompetitionJobAuthority(ctx, control), fixture.settings, worker, controlId,
			EvaluationOutcome{Error: submissionError("synthetic_fifo_control", "synthetic control is not a winner candidate")}); err != nil || retry {
			t.Fatalf("terminal FIFO control: retry=%t error=%v", retry, err)
		}
		fixture.now = fixture.round.ClosesAt.Add(time.Second)
		fixture.settings.automationLease, err = fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-reevaluation-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatalf("acquire fresh closing authority: %v", err)
		}
		result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || result == nil {
			t.Fatalf("seal actual recovered election: result=%+v error=%v", result, err)
		}
		var originalEntry, laterEntry *model.CompetitionSealedEntry
		for i := range result.Seal.Entries {
			entry := &result.Seal.Entries[i]
			if entry.JobId == fixture.job.JobId {
				originalEntry = entry
			} else if entry.JobId == laterId {
				laterEntry = entry
			}
		}
		if originalEntry == nil || laterEntry == nil || !originalEntry.Eligible || !laterEntry.Eligible || originalEntry.Score == nil || laterEntry.Score == nil ||
			originalEntry.Score.RawScore == nil || laterEntry.Score.RawScore == nil || *originalEntry.Score.RawScore != 50 || *laterEntry.Score.RawScore != 50 ||
			!originalEntry.SubmittedAt.Equal(originalSubmittedAt) || !originalEntry.SubmittedAt.Before(laterEntry.SubmittedAt) ||
			!laterEntry.CompletedAt.Before(originalEntry.CompletedAt) || originalEntry.Rank >= laterEntry.Rank {
			t.Fatalf("equal-score election used recovery/completion order instead of original admission: original=%+v later=%+v", originalEntry, laterEntry)
		}
		if result.ProvisionalRewardWinner == nil || *result.ProvisionalRewardWinner != fixture.job.JobId || len(result.Decisions) != 1 ||
			result.Decisions[0].SelectedJobId == nil || *result.Decisions[0].SelectedJobId != fixture.job.JobId {
			t.Fatal("actual sealed decision did not elect the earlier admitted equal-score recovered submission")
		}
	})
}
