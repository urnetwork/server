// A wake committed after an owner's last empty read must survive its handback.
package task

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

type runOnceGenerationArgs struct {
	Scope  server.Id `json:"scope"`
	Cursor int       `json:"cursor"`
}

func runOnceGenerationWork(*runOnceGenerationArgs, *session.ClientSession) (*struct{}, error) {
	return &struct{}{}, nil
}

type runOnceGenerationTarget struct {
	Target
	batch  bool
	before func(context.Context, *Task) error
	after  func(server.PgTx, *Task) error
}

func (self *runOnceGenerationTarget) TaskCompletionBatchEnabled() bool { return self.batch }

func (self *runOnceGenerationTarget) Run(ctx context.Context, queued *Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	if self.before != nil {
		if err := self.before(ctx, queued); err != nil {
			return nil, nil, err
		}
	}
	value, post, err := self.Target.Run(ctx, queued)
	return value, func(tx server.PgTx) ([]server.PostFunction, error) {
		posts, err := post(tx)
		if err == nil && self.after != nil {
			err = self.after(tx, queued)
		}
		return posts, err
	}, err
}

func runOnceGenerationEnv(t *testing.T, run func(testing.TB, context.Context)) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		run(t, ctx)
	})
}

func runOnceGenerationKey(scope server.Id) *RunOnceOption {
	return RunOnce("synthetic-run-once-generation", scope)
}

func runOnceGenerationPending(ctx context.Context, scopes []server.Id) map[server.Id]*Task {
	keys := make([]string, len(scopes))
	for index, scope := range scopes {
		keys[index] = runOnceGenerationKey(scope).String()
	}
	var ids []server.Id
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=ANY($1)`, keys)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				server.Raise(rows.Scan(&id))
				ids = append(ids, id)
			}
		})
	})
	return GetTasks(ctx, ids...)
}

// The ordinary and explicitly opted-in paths use the same native causal order.
// All results are published before the batch collector observes its first one.
func runOnceGenerationWakeAfterEmpty(t *testing.T, batch bool) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		count := 1
		if batch {
			count = 2
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE test_run_once_work(scope uuid PRIMARY KEY, consumed boolean NOT NULL DEFAULT false)`))
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		empty, published, release := make(chan server.Id, count), make(chan struct{}, count), make(chan struct{})
		var resumed sync.Once
		var phase atomic.Int32
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork), batch: batch}
		target.before = func(runCtx context.Context, queued *Task) error {
			var args runOnceGenerationArgs
			server.Raise(json.Unmarshal([]byte(queued.ArgsJson), &args))
			server.Tx(runCtx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(runCtx, `UPDATE test_run_once_work SET consumed=true WHERE scope=$1`, args.Scope))
				var remaining int
				server.Raise(tx.QueryRow(runCtx, `SELECT count(*) FROM test_run_once_work WHERE scope=$1 AND NOT consumed`, args.Scope).Scan(&remaining))
				if remaining != 0 {
					server.Raise(context.Canceled)
				}
			})
			if phase.Load() == 0 {
				empty <- queued.TaskId
				select {
				case <-release:
				case <-runCtx.Done():
					return runCtx.Err()
				}
			}
			return nil
		}
		settings := DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := NewTaskWorker(ctx, settings)
		worker.AddTargets(target)
		worker.completionResultPublished = func() { published <- struct{}{} }
		never := make(chan time.Time)
		first := true
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
		commits := 0
		worker.completionBatchCommitReturned = func() { commits++ }
		scopes, original := make([]server.Id, count), make([]server.Id, count)
		for index := range count {
			scopes[index] = server.NewId()
			original[index] = ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scopes[index]}, owner,
				runOnceGenerationKey(scopes[index]), RunAt(server.NowUtc().Add(-time.Hour)))
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
			select {
			case <-done:
			case <-ctx.Done():
			}
			worker.Close()
		}()
		for range count {
			select {
			case <-empty:
			case <-ctx.Done():
				t.Fatal("owner did not reach the explicit final empty-read barrier", ctx.Err())
			}
		}
		// Producer work and all duplicate wakes commit while the old owner is
		// still taken. No elapsed-time or negative-arrival assertion is used.
		server.Tx(ctx, func(tx server.PgTx) {
			for _, scope := range scopes {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO test_run_once_work(scope) VALUES($1)`, scope))
				for range 3 {
					ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
						runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
				}
			}
		})
		if pending := GetTasks(ctx, original...); len(pending) != count {
			t.Fatal("a producer replaced the executing identity before handback")
		} else {
			for _, row := range pending {
				if row.ClaimTime.IsZero() {
					t.Fatal("causal wake did not overlap a real durable claim")
				}
			}
		}
		resumed.Do(func() { close(release) })
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("old owner did not complete its bounded handback", ctx.Err())
		}
		if panicValue != nil || evalErr != nil || len(finished) != count || len(retried)+len(posts) != 0 ||
			len(GetFinishedTasks(ctx, original...)) != count || len(GetTasks(ctx, original...)) != 0 {
			t.Fatal("original ownership did not finish exactly", panicValue, evalErr, finished, retried, posts)
		}
		if batch && commits != 1 {
			t.Fatal("causal control did not traverse the opted-in batch finalizer", commits)
		}
		var waiting int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM test_run_once_work WHERE NOT consumed`).Scan(&waiting))
		})
		if waiting != count {
			t.Fatal("producer work was consumed without a successor invocation", waiting)
		}
		successors := runOnceGenerationPending(ctx, scopes)
		if len(successors) != count {
			t.Fatalf("run-once wake committed while taken was lost: pending_successors=%d want=%d", len(successors), count)
		}
		for _, id := range original {
			if successors[id] != nil {
				t.Fatal("completed identity was reused as its own successor")
			}
		}
		phase.Store(1)
		finished, retried, posts, evalErr = worker.EvalTasks(count)
		if evalErr != nil || len(finished) != count || len(retried)+len(posts) != 0 || len(runOnceGenerationPending(ctx, scopes)) != 0 {
			t.Fatal("coalesced successor did not consume exactly one follow-up invocation", evalErr, finished, retried, posts)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM test_run_once_work WHERE NOT consumed`).Scan(&waiting))
		})
		if waiting != 0 {
			t.Fatal("durable successor did not consume the committed producer work", waiting)
		}
	})
}

func TestRunOnceWakeAfterLastEmptyReadSurvivesOrdinaryFinish(t *testing.T) {
	runOnceGenerationWakeAfterEmpty(t, false)
}

func TestRunOnceWakeAfterLastEmptyReadSurvivesBatchFinish(t *testing.T) {
	runOnceGenerationWakeAfterEmpty(t, true)
}
