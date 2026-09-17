package task

// These fixtures exercise the actual PostgreSQL claim and dispatch boundary.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

type claimProfileArgs struct{}
type claimProfileResult struct{}

// claimProfileAllowed and claimProfileExcluded give persisted rows distinct
// canonical names; claimProfileTarget supplies observable, local-only bodies.
func claimProfileAllowed(_ *claimProfileArgs, _ *session.ClientSession) (*claimProfileResult, error) {
	return &claimProfileResult{}, nil
}

func claimProfileExcluded(_ *claimProfileArgs, _ *session.ClientSession) (*claimProfileResult, error) {
	return &claimProfileResult{}, nil
}

type claimProfileTarget struct {
	Target
	runs      atomic.Int32
	posts     atomic.Int32
	failPosts bool
}

// Run counts actual dispatch and supplies the same post hook as a retry.
func (self *claimProfileTarget) Run(ctx context.Context, _ *Task) (any, func(server.PgTx) error, error) {
	self.runs.Add(1)
	return &claimProfileResult{}, func(tx server.PgTx) error {
		return self.RunPost(ctx, nil, tx)
	}, nil
}

// RunPost can deliberately leave a durable retry for another worker generation.
func (self *claimProfileTarget) RunPost(_ context.Context, _ *FinishedTask, _ server.PgTx) error {
	self.posts.Add(1)
	if self.failPosts {
		return errors.New("synthetic post retry")
	}
	return nil
}

// TestTaskClaimProfilePreservesExcludedBacklog proves filtering occurs before
// the candidate cap and that restarting a scoped worker does not mutate losers.
func TestTaskClaimProfilePreservesExcludedBacklog(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		excludedIds := []server.Id{}
		for range 130 {
			excludedIds = append(excludedIds, ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-2*time.Hour))))
		}
		before := GetTasks(ctx, excludedIds...)
		for range 2 {
			allowedId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)))
			settings := DefaultTaskWorkerSettings()
			settings.ClaimRegisteredTargetsOnly = true
			worker := NewTaskWorker(ctx, settings)
			target := &claimProfileTarget{Target: NewTaskTarget(claimProfileAllowed)}
			worker.AddTargets(target)
			finished, retried, postRetried, err := worker.EvalTasks(1)
			worker.Close()
			if err != nil || len(finished) != 1 || finished[0] != allowedId || len(retried)+len(postRetried) != 0 || target.runs.Load() != 1 {
				t.Fatalf("allowed work starved behind excluded backlog: finished=%v retries=%v/%v runs=%d error=%v", finished, retried, postRetried, target.runs.Load(), err)
			}
			if after := GetTasks(ctx, excludedIds...); !reflect.DeepEqual(before, after) {
				t.Fatal("scoped claim changed a retained excluded row")
			}
		}
	})
}

// TestTaskClaimProfilePreservesVersionedAliases checks claim and dispatch use
// the same canonicalization for retained rows from older module versions.
func TestTaskClaimProfilePreservesVersionedAliases(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		settings := DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		const legacyName = "fixture.example/tasks.LegacyAllowed"
		target := &claimProfileTarget{Target: NewTaskTarget(claimProfileAllowed, legacyName)}
		worker.AddTargets(target)
		for _, storedName := range []string{legacyName, strings.Replace(target.TargetFunctionName(), "/server/", "/server/v37/", 1)} {
			taskId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)))
			server.Tx(ctx, func(tx server.PgTx) {
				_, err := tx.Exec(ctx, `UPDATE pending_task SET function_name = $2 WHERE task_id = $1`, taskId, storedName)
				server.Raise(err)
			})
		}
		finished, retried, postRetried, err := worker.EvalTasks(2)
		if err != nil || len(finished) != 2 || len(retried)+len(postRetried) != 0 || target.runs.Load() != 2 {
			t.Fatalf("retained aliases failed claim/dispatch: finished=%v retries=%v/%v runs=%d error=%v", finished, retried, postRetried, target.runs.Load(), err)
		}
	})
}

// TestTaskClaimProfileScopesPostRetries preserves excluded, malformed and
// orphaned wrappers without letting them block a permitted deferred post.
func TestTaskClaimProfileScopesPostRetries(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		production := NewTaskWorkerWithDefaults(ctx)
		defer production.Close()
		allowed := &claimProfileTarget{Target: NewTaskTarget(claimProfileAllowed), failPosts: true}
		excluded := &claimProfileTarget{Target: NewTaskTarget(claimProfileExcluded), failPosts: true}
		production.AddTargets(allowed, excluded)
		allowedId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)))
		excludedId := ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)))
		finished, retried, postRetried, err := production.EvalTasks(2)
		if err != nil || len(finished)+len(retried) != 0 || len(postRetried) != 2 {
			t.Fatalf("could not create retained post retries: %v/%v/%v %v", finished, retried, postRetried, err)
		}
		allowed.failPosts = false
		excluded.failPosts = false
		retainedIds := []server.Id{}
		var allowedPostId server.Id
		for _, pending := range GetTasks(ctx, ListPendingTasks(ctx)...) {
			if strings.Contains(pending.ArgsJson, allowedId.String()) {
				allowedPostId = pending.TaskId
			} else {
				retainedIds = append(retainedIds, pending.TaskId)
			}
		}
		malformedArgs := []string{`{invalid`, `{"task_id":"not-an-id"}`, `{"task_id":"\u0000"}`}
		for i := range 130 {
			postId := ScheduleTask(production.RunPost, &RunPostArgs{TaskId: server.NewId()}, clientSession, RunAt(server.NowUtc().Add(-2*time.Hour)))
			retainedIds = append(retainedIds, postId)
			if i%4 < len(malformedArgs) {
				server.Tx(ctx, func(tx server.PgTx) {
					_, err := tx.Exec(ctx, `UPDATE pending_task SET args_json = $2 WHERE task_id = $1`, postId, malformedArgs[i%4])
					server.Raise(err)
				})
			}
		}
		if allowedPostId == (server.Id{}) || len(retainedIds) != 131 {
			t.Fatal("fixture lost the allowed or excluded deferred post")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := tx.Exec(ctx, `UPDATE pending_task SET run_at = $1, claim_time = $1, release_time = $1`, server.NowUtc().Add(-2*time.Hour))
			server.Raise(err)
			_, err = tx.Exec(ctx, `UPDATE pending_task SET run_at = $2 WHERE task_id = $1`, allowedPostId, server.NowUtc().Add(-time.Hour))
			server.Raise(err)
		})
		before := GetTasks(ctx, retainedIds...)
		settings := DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(allowed)
		finished, retried, postRetried, err = worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != allowedPostId || len(retried)+len(postRetried) != 0 || allowed.posts.Load() != 2 || excluded.posts.Load() != 1 {
			t.Fatalf("deferred posts crossed workload boundary: %v/%v/%v posts=%d/%d error=%v", finished, retried, postRetried, allowed.posts.Load(), excluded.posts.Load(), err)
		}
		if after := GetTasks(ctx, retainedIds...); !reflect.DeepEqual(before, after) {
			t.Fatal("scoped post claim changed a retained excluded/malformed/orphaned row")
		}
		completed := GetFinishedTasks(ctx, allowedId, excludedId)
		if !completed[allowedId].PostCompleted || completed[excludedId].PostCompleted {
			t.Fatal("post completion changed the wrong original task")
		}
	})
}

// TestTaskClaimProfileDefaultKeepsUnknownTargetRetry preserves rolling-deploy
// behavior when the opt-in registered-target claim boundary is absent.
func TestTaskClaimProfileDefaultKeepsUnknownTargetRetry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		unknownId := ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)))
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		finished, retried, postRetried, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(postRetried) != 0 || len(retried) != 1 || retried[0] != unknownId {
			t.Fatalf("ordinary production unknown-target behavior changed: %v/%v/%v %v", finished, retried, postRetried, err)
		}
		if pending := GetTasks(ctx, unknownId)[unknownId]; pending == nil || pending.RescheduleErrorCount != 1 || !strings.Contains(pending.RescheduleError, ErrTargetNotFound.Error()) {
			t.Fatal("ordinary worker did not retain the unknown-target retry")
		}
	})
}
