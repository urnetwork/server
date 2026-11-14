// Disposable guarded lifecycle regressions retain the original admission and
// terminal bytes while exercising only synthetic signed native observations.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competitionagent"
	"github.com/urnetwork/server/model"
)

// The first grant is clockless and single-use. Original submission ordering,
// source, paid principal and event history survive the new bounded execution.
func TestCompetitionOperatorPatchRetryPgAuditedLifecyclePreservesOriginalAndThreeHourBudget(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixtureWithPatchRetry(t, tb, true)
		before := fixture.snapshot(tb.Context())
		receipt, err := fixture.authorize(tb, tb.Context())
		if err != nil || receipt == nil || receipt.Cycle != 1 || receipt.StartedAt != nil || fixture.calls != 3 ||
			receipt.PreviousFailure != (model.CompetitionEvaluationFailure{Kind: "infrastructure", Code: "executor_process_failed"}) ||
			receipt.IncidentClass != model.CompetitionJobReevaluationOperatorRequestedPatchRetry {
			tb.Fatal("explicit synthetic patch retry did not retain original generic failure", err)
		}
		after := fixture.snapshot(tb.Context())
		requireCompetitionReevaluationOriginalEvidence(tb, before.job, after.job)
		var original []byte
		server.Db(tb.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(tb.Context(), `SELECT original_terminal_json FROM competition_job_reevaluation WHERE job_id=$1`, fixture.original.JobId).Scan(&original))
		})
		if !bytes.Equal(original, before.job.job) {
			tb.Fatal("operator retry rewrote original terminal instead of sealing it")
		}
		queued, err := fixture.store.GetJob(tb.Context(), fixture.settings, fixture.original.JobId, fixture.principal)
		if err != nil || queued.ExecutionDeadline != nil || queued.CycleStartedAt != nil || queued.ExecutorHostId != "" ||
			!queued.SubmittedAt.Equal(fixture.original.SubmittedAt) || !queued.StartedAt.Equal(*fixture.original.StartedAt) {
			tb.Fatal("grant allocated a new clock or moved original admission ordering", err)
		}
		replayed, err := fixture.authorize(tb, tb.Context())
		if err != nil || replayed == nil || fixture.calls != 3 || !reflect.DeepEqual(after, fixture.snapshot(tb.Context())) {
			tb.Fatal("exact replay observed hosts or mutated clock/fences/audit", err)
		}
		requireCompetitionDistributedRecoveryReceiptEqual(tb, fixture.settings, receipt, replayed)
		originalRequest := competitionRecoveryTestCopy(tb, fixture.request)
		fixture.request.Authorization.Distributed.PatchRetry.ApplicabilityEvidenceSha256 = strings.Repeat("f", 64)
		fixture.request.AuthorizationSha256 = competitionRecoveryTestDigest(tb, fixture.request.Authorization)
		if again, err := fixture.authorize(tb, tb.Context()); err == nil || again != nil || !reflect.DeepEqual(after, fixture.snapshot(tb.Context())) {
			tb.Fatal("resealed different evidence replaced committed approval", err)
		}
		fixture.request = originalRequest
		fixture.request.IdempotencyKey = "synthetic-second-patch-retry"
		if again, err := fixture.authorize(tb, tb.Context()); err == nil || again != nil {
			tb.Fatal("another request identity granted a second cycle")
		}
		fixture.request = originalRequest
		fixture.releaseHold(tb)
		var beforeClaim, afterClaim time.Time
		server.Db(tb.Context(), func(conn server.PgConn) {
			var err error
			beforeClaim, err = competitionIntakeFirstDatabaseTime(tb.Context(), conn)
			server.Raise(err)
		})
		job, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-patch-retry-worker", fixture.runtime.WorkerImageDigest)
		server.Db(tb.Context(), func(conn server.PgConn) {
			var err error
			afterClaim, err = competitionIntakeFirstDatabaseTime(tb.Context(), conn)
			server.Raise(err)
		})
		if err != nil || job == nil || job.JobId != fixture.original.JobId || job.AttemptCount != 2 || job.EvaluationCycle != 1 || job.CycleAttemptOffset != 1 ||
			job.CycleStartedAt == nil || job.CycleStartedAt.Before(beforeClaim) || afterClaim.Before(*job.CycleStartedAt) ||
			job.ExecutionDeadline == nil || !job.ExecutionDeadline.Equal(job.CycleStartedAt.Add(3*time.Hour)) ||
			!job.SubmittedAt.Equal(fixture.original.SubmittedAt) || !job.StartedAt.Equal(*fixture.original.StartedAt) {
			tb.Fatal("patch retry did not claim same admission with fresh bounded clock", err)
		}
		if err := fixture.store.Heartbeat(withCompetitionJobAuthority(tb.Context(), fixture.original), fixture.selected(0), "synthetic-original-worker", job.JobId); err == nil {
			tb.Fatal("original cycle authority renewed the new retry")
		}
		outcome, _ := competitionOperatorPatchRetryRetainedFailureForTest(tb, fixture.selected(0), job)
		if retry, err := fixture.store.Complete(withCompetitionJobAuthority(tb.Context(), job), fixture.selected(0), "synthetic-patch-retry-worker", job.JobId, outcome); err != nil || retry {
			tb.Fatal("new retry could not finish within ordinary guarded lifecycle", retry, err)
		}
		outcome.closeReceiveCustody()
		if again, err := fixture.authorize(tb, tb.Context()); err != nil || again == nil || fixture.calls != 3 {
			tb.Fatal("completed retry lost receipt-only exact replay", err)
		}
		requireCompetitionReevaluationOriginalEvidence(tb, before.job, fixture.snapshot(tb.Context()).job)
	})
}

// Every changed pin fails before any persistent queue/fence/audit mutation.
// Valid replacement hashes do not replace the actual locked round and source.
func TestCompetitionOperatorPatchRetryPgRejectsChangedPinsWithoutMutation(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixtureWithPatchRetry(t, tb, true)
		original := competitionRecoveryTestCopy(tb, fixture.request)
		before := fixture.snapshot(tb.Context())
		for _, change := range []struct {
			name   string
			mutate func(*model.CompetitionJobReevaluationAuthorization)
		}{
			{name: "patch", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
				a.Distributed.PatchRetry.PatchSha256 = strings.Repeat("9", 64)
			}},
			{name: "base", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
				a.Distributed.SourceLockSha256 = strings.Repeat("9", 64)
			}},
			{name: "round-policy", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.RoundPolicySha256 = strings.Repeat("9", 64) }},
			{name: "baseline", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.BaselineSha256 = strings.Repeat("9", 64) }},
			{name: "round", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.RoundId = server.NewId() }},
			{name: "attempt", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.ExpectedAttemptCount++ }},
			{name: "evidence", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
				a.Distributed.PatchRetry.ApplicabilityEvidenceSha256 = ""
			}},
			{name: "scored", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
				var terminal map[string]any
				server.Raise(json.Unmarshal(a.ExpectedTerminalSnapshot, &terminal))
				terminal["state"], terminal["score_json"] = "succeeded", map[string]any{"synthetic_score": 1}
				a.ExpectedTerminalSnapshot, _ = json.Marshal(terminal)
				a.ExpectedTerminalSha256, _ = CompetitionJobReevaluationSnapshotDigest(a.ExpectedTerminalSnapshot)
			}},
			{name: "failure", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
				var terminal map[string]any
				server.Raise(json.Unmarshal(a.ExpectedTerminalSnapshot, &terminal))
				terminal["eval_error_json"] = model.CompetitionEvaluationFailure{Kind: "submission", Code: "resource_violation"}
				a.ExpectedTerminalSnapshot, _ = json.Marshal(terminal)
				a.ExpectedTerminalSha256, _ = CompetitionJobReevaluationSnapshotDigest(a.ExpectedTerminalSnapshot)
			}},
		} {
			fixture.request = competitionRecoveryTestCopy(tb, original)
			change.mutate(&fixture.request.Authorization)
			fixture.request.AuthorizationSha256 = competitionRecoveryTestDigest(tb, fixture.request.Authorization)
			if receipt, err := fixture.authorize(tb, tb.Context()); err == nil || receipt != nil || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) {
				tb.Fatal("changed pin changed original/fences/audit", change.name, err)
			}
		}
	})
}

// Authenticated availability is independent of the database terminal row.
// One busy frozen host prevents any partial grant of the retry.
func TestCompetitionOperatorPatchRetryPgBusyNativeHostIsAtomic(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixtureWithPatchRetry(t, tb, true)
		fixture.mutateNative = func(response *competitionagent.Response) {
			if response.Identity == nil {
				response.State = "occupied"
			}
		}
		before := fixture.snapshot(tb.Context())
		if receipt, err := fixture.authorize(tb, tb.Context()); err == nil || receipt != nil || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) {
			tb.Fatal("busy native host partially granted a retry", err)
		}
	})
}

// The old deployed query shapes keep ownership while migration-first Ddl
// adds the new optional path; no original running worker needs new metadata.
func TestCompetitionOperatorPatchRetryPgMigrationKeepsRunningCycleZeroQueries(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		version := server.CompetitionOperatorRequestedPatchRetryMigrationVersion()
		server.ApplyDbMigrationsUpTo(tb.Context(), version-1)
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		settings := fixture.selected(0)
		job, err := fixture.store.Claim(tb.Context(), settings, "synthetic-running-pre-patch-retry-worker", testWorkerImageDigest())
		if err != nil || job == nil || job.EvaluationCycle != 0 {
			tb.Fatal("previous release query could not claim", err)
		}
		server.ApplyDbMigrationsUpTo(tb.Context(), version)
		fixture.clock.Advance(time.Second)
		ctx := withCompetitionJobAuthority(context.Background(), job)
		if err := fixture.store.Heartbeat(ctx, settings, "synthetic-running-pre-patch-retry-worker", job.JobId); err != nil {
			tb.Fatal("additive patch retry migration fenced old heartbeat query", err)
		}
		if retry, err := fixture.store.Complete(ctx, settings, "synthetic-running-pre-patch-retry-worker", job.JobId, competitionPreparedIntakeCompletionOutcome(tb, settings, job)); err != nil || retry {
			tb.Fatal("additive patch retry migration rejected old completion query", retry, err)
		}
	})
}
