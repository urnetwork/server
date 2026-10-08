// Future-only active requests retain their earliest explicit deadline. A Post
// request participates in the same minimum and owns the successor arguments.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type runOnceFuturePolicy struct {
	batch         bool
	activeWakes   bool
	postImmediate bool
	postLater     bool
	postError     bool
}

// Execute the live candidate SQL with its existing explicit eligibility clock.
// This selects and locks real rows, but does not fake a lease or execute a
// future function. Full EvalTasks before the deadline is checked separately.
func runOnceFutureCandidates(t testing.TB, ctx context.Context, worker *TaskWorker,
	nowBlock int64, wanted map[server.Id]*Task,
) int {
	t.Helper()
	selected := 0
	server.Tx(ctx, func(tx server.PgTx) {
		query, args := worker.claimCandidatesQuery(nowBlock, len(wanted)+64)
		rows, err := tx.Query(ctx, query, args...)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				var functionName string
				var priority, maxTime int
				server.Raise(rows.Scan(&id, &functionName, &priority, &maxTime))
				if wanted[id] != nil {
					selected++
				}
			}
		})
	}, server.TxReadCommitted, server.OptNoRetry())
	return selected
}

// Both finalizers receive results only after the producer's real transaction
// commits while every original function is held at an explicit active barrier.
func runOnceFuturePolicyControl(t *testing.T, policy runOnceFuturePolicy) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		if policy.batch && (policy.postImmediate || policy.postLater || policy.postError) {
			t.Fatal("a no-post batch control cannot carry a transactional Post")
		}
		count := 1
		if policy.batch {
			count = 2
		}
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		// The virtual T20/T30/T60 anchor stays beyond the fixture's one-minute
		// watchdog, so completion-before-due does not depend on machine speed.
		anchor := server.NowUtc().Truncate(time.Second).Add(2 * time.Minute)
		oldRunAt := anchor.Add(-time.Hour)
		futureAt := anchor.Add(20 * time.Second)
		active := make(chan server.Id, count)
		published := make(chan struct{}, count)
		release := make(chan struct{})
		var resumed sync.Once
		var postAt time.Time
		postFailure := errors.New("synthetic future-policy Post failure")
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork), batch: policy.batch}
		target.before = func(runCtx context.Context, queued *Task) error {
			active <- queued.TaskId
			select {
			case <-release:
				return nil
			case <-runCtx.Done():
				return runCtx.Err()
			}
		}
		if policy.postImmediate || policy.postLater || policy.postError {
			target.after = func(tx server.PgTx, queued *Task) error {
				if policy.postImmediate || policy.postLater {
					var args runOnceGenerationArgs
					server.Raise(json.Unmarshal([]byte(queued.ArgsJson), &args))
					postAt = anchor.Add(90 * time.Second)
					if policy.postImmediate {
						postAt = server.NowUtc().Truncate(time.Microsecond)
					}
					ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: args.Scope, Cursor: 99}, owner,
						runOnceGenerationKey(args.Scope), RunAt(postAt))
				}
				if policy.postError {
					return postFailure
				}
				return nil
			}
		}
		worker := runOnceGenerationWorker(ctx, target)
		worker.completionResultPublished = func() { published <- struct{}{} }
		first := true
		never := make(chan time.Time)
		worker.heartbeatAfter = func(time.Duration) <-chan time.Time {
			if first {
				first = false
				for range count {
					select {
					case <-published:
					case <-ctx.Done():
						server.Raise(ctx.Err())
					}
				}
			}
			return never
		}
		batchCommits := 0
		worker.completionBatchCommitReturned = func() { batchCommits++ }
		scopes, original := make([]server.Id, count), make([]server.Id, count)
		for index := range count {
			scopes[index] = server.NewId()
			original[index] = ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scopes[index], Cursor: 7}, owner,
				runOnceGenerationKey(scopes[index]), RunAt(oldRunAt))
			// This earlier future request precedes activation and must be
			// absorbed by the original claim, not the next generation's minimum.
			ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scopes[index], Cursor: 55}, owner,
				runOnceGenerationKey(scopes[index]), RunAt(anchor.Add(10*time.Second)))
		}
		done := make(chan struct{})
		var finished, retried, posts []server.Id
		var evalErr error
		var panicValue any
		go func() {
			defer close(done)
			defer func() { panicValue = recover() }()
			finished, retried, posts, evalErr = worker.EvalTasks(count)
		}()
		defer func() {
			resumed.Do(func() { close(release) })
			worker.Close()
			joinCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			select {
			case <-done:
			case <-joinCtx.Done():
				t.Error("future-policy owner did not join during bounded cleanup")
			}
		}()
		activeIds := map[server.Id]bool{}
		for range count {
			select {
			case id := <-active:
				activeIds[id] = true
			case <-ctx.Done():
				t.Fatal("real task functions did not enter the active barrier", ctx.Err())
			}
		}
		claimed := GetTasks(ctx, original...)
		if len(activeIds) != count || len(claimed) != count {
			t.Fatal("active barrier lost an exact claimed identity")
		}
		for _, id := range original {
			row := claimed[id]
			if !activeIds[id] || row == nil || row.ClaimTime.IsZero() || row.ClaimGeneration < 1 ||
				row.RunOnceGeneration != 1 || !row.RunAt.Equal(oldRunAt) {
				t.Fatal("future requests did not follow an actual preexisting claim")
			}
		}
		if policy.activeWakes {
			server.Tx(ctx, func(tx server.PgTx) {
				for _, scope := range scopes {
					for _, seconds := range []int{30, 60, 20} {
						ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope, Cursor: 66}, owner,
							runOnceGenerationKey(scope), RunAt(anchor.Add(time.Duration(seconds)*time.Second)))
					}
				}
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		resumed.Do(func() { close(release) })
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("active owner did not finish before its future deadline", ctx.Err())
		}
		oldFinished := GetFinishedTasks(ctx, original...)
		if panicValue != nil || evalErr != nil || len(retried) != 0 || len(finished)+len(posts) != count ||
			len(oldFinished) != count || len(GetTasks(ctx, original...)) != 0 {
			t.Fatal("future-policy original owners did not finalize exactly", panicValue, evalErr, finished, retried, posts)
		}
		for _, row := range oldFinished {
			if !row.RunEndTime.Before(futureAt) || row.PostCompleted == policy.postError ||
				(policy.postError && row.PostError == "") {
				t.Fatal("future-policy original completion lost its time or Post outcome")
			}
		}
		if (policy.batch && batchCommits != 1) || (!policy.batch && batchCommits != 0) {
			t.Fatal("future-policy control did not traverse its intended finalizer", batchCommits)
		}
		pending := runOnceGenerationPending(ctx, scopes)
		if !policy.activeWakes && !policy.postImmediate && !policy.postLater {
			if len(pending) != 0 {
				t.Fatal("completion invented a successor without a post-activation request", len(pending))
			}
			return
		}
		if len(pending) != count {
			t.Fatal("active requests did not coalesce into one successor per key", len(pending))
		}
		expectedAt, expectedCursor := futureAt, 7
		if policy.postImmediate || policy.postLater {
			expectedCursor = 99
			if postAt.Before(expectedAt) {
				expectedAt = postAt
			}
		}
		var availableBlock int64
		for id, row := range pending {
			var args runOnceGenerationArgs
			server.Raise(json.Unmarshal([]byte(row.ArgsJson), &args))
			if activeIds[id] || runOnceGenerationKey(args.Scope).String() != row.RunOnceKey || args.Cursor != expectedCursor ||
				!row.RunAt.Equal(expectedAt) || !row.ClaimTime.IsZero() {
				t.Fatal("successor lost the minimum new request time or Post arguments", row.RunAt, expectedAt, args.Cursor)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var block int64
				server.Raise(conn.QueryRow(ctx, `SELECT available_block FROM pending_task WHERE task_id=$1`, id).Scan(&block))
				if availableBlock != 0 && availableBlock != block {
					t.Fatal("equal future deadlines produced different eligibility blocks")
				}
				availableBlock = block
			})
		}
		if availableBlock <= expectedAt.Unix()/BlockSizeSeconds {
			t.Fatal("future control did not retain the native generated eligibility boundary")
		}
		if got := runOnceFutureCandidates(t, ctx, worker, availableBlock-1, pending); got != 0 {
			t.Fatal("native claim predicate admitted a successor before its eligibility block", got)
		}
		if got := runOnceFutureCandidates(t, ctx, worker, availableBlock, pending); got != count {
			t.Fatal("native claim predicate did not admit the exact due successors", got, count)
		}
		if !policy.postImmediate && !policy.postError {
			if done, retried, posts, err := worker.EvalTasks(count); err != nil || len(done)+len(retried)+len(posts) != 0 {
				t.Fatal("real worker executed a future-only successor before its deadline", err)
			}
		}
	})
}

func TestRunOnceActiveFutureOrdinaryUsesEarliestNewRequest(t *testing.T) {
	runOnceFuturePolicyControl(t, runOnceFuturePolicy{activeWakes: true})
}

func TestRunOnceActiveFutureBatchUsesEarliestNewRequest(t *testing.T) {
	runOnceFuturePolicyControl(t, runOnceFuturePolicy{batch: true, activeWakes: true})
}

func TestRunOnceActiveOrdinaryWithoutNewRequestHasNoSuccessor(t *testing.T) {
	runOnceFuturePolicyControl(t, runOnceFuturePolicy{})
}

func TestRunOnceActiveBatchWithoutNewRequestHasNoSuccessor(t *testing.T) {
	runOnceFuturePolicyControl(t, runOnceFuturePolicy{batch: true})
}

func TestRunOnceActiveFutureExplicitPostNowParticipatesInMinimum(t *testing.T) {
	runOnceFuturePolicyControl(t, runOnceFuturePolicy{activeWakes: true, postImmediate: true})
}

func TestRunOnceActiveFutureLaterPostKeepsEarlierRequestAndCursor(t *testing.T) {
	runOnceFuturePolicyControl(t, runOnceFuturePolicy{activeWakes: true, postLater: true})
}

func TestRunOnceActiveFuturePostErrorRetainsDeadlineAndCursor(t *testing.T) {
	runOnceFuturePolicyControl(t, runOnceFuturePolicy{activeWakes: true, postLater: true, postError: true})
}
