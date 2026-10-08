// Real claims and native failures exercise both atomic completion handbacks.
package task

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Keep returned results under their real advisory owners until each control
// explicitly releases them; only the observed result's epoch can retire it.
func runOnceGenerationClaimResults(t testing.TB, ctx context.Context, batch bool) (
	*TaskWorker, *taskClaimGuard, *session.ClientSession, []server.Id, []server.Id, []*taskExecutionResult,
) {
	t.Helper()
	count := 1
	if batch {
		count = 2
	}
	owner := session.NewLocalClientSession(ctx, "", nil)
	target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork), batch: batch}
	worker := runOnceGenerationWorker(ctx, target)
	ids, scopes := make([]server.Id, count), make([]server.Id, count)
	for index := range ids {
		scopes[index] = server.NewId()
		ids[index] = ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scopes[index], Cursor: 7}, owner,
			runOnceGenerationKey(scopes[index]), RunAt(server.NowUtc().Add(-time.Hour)))
	}
	claimed, guard, err := worker.takeTasks(count)
	if err != nil || guard == nil || len(claimed) != count {
		owner.Cancel()
		worker.Close()
		t.Fatal("custody control did not acquire its real owners", err)
	}
	results := make([]*taskExecutionResult, count)
	for index, id := range ids {
		results[index] = worker.executeTask(ctx, claimed[id], target)
		if results[index].err != nil {
			t.Fatal("custody control did not execute its claimed target", results[index].err)
		}
	}
	return worker, guard, owner, ids, scopes, results
}

// Queued writes share the same handshake as direct writes. Earliest requested
// future time survives, and immutable arguments remain those of the old owner.
func TestRunOnceQueuedWakeBatchKeepsFutureDeadline(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, guard, owner, ids, scopes, results := runOnceGenerationClaimResults(t, ctx, true)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		wakeAt := server.NowUtc().Add(time.Hour).Truncate(time.Microsecond)
		server.Tx(ctx, func(tx server.PgTx) {
			server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
				for _, scope := range scopes {
					for _, at := range []time.Time{wakeAt.Add(time.Hour), wakeAt, wakeAt.Add(time.Minute)} {
						QueueTaskInBatch(batch, runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope, Cursor: 99}, owner,
							runOnceGenerationKey(scope), RunAt(at))
					}
				}
			})
		})
		if retry, err := worker.finalizeTaskBatch(results); retry || err != nil {
			t.Fatal("queued wakes did not complete atomically", retry, err)
		}
		pending := runOnceGenerationPending(ctx, scopes)
		if len(pending) != 2 || len(GetFinishedTasks(ctx, ids...)) != 2 {
			t.Fatal("queued wake custody is incomplete")
		}
		for id, row := range pending {
			var args runOnceGenerationArgs
			server.Raise(json.Unmarshal([]byte(row.ArgsJson), &args))
			if id == ids[0] || id == ids[1] || args.Cursor != 7 || !row.RunAt.Equal(wakeAt) || row.RunOnceGeneration != 0 || row.ClaimGeneration != 0 {
				t.Fatal("queued wake lost its new identity, preserved payload or future time", row)
			}
		}
		if finished, retried, posts, err := worker.EvalTasks(2); err != nil || len(finished)+len(retried)+len(posts) != 0 {
			t.Fatal("future queued wake ran before its requested time", err)
		}
	})
}

// A refused copy rolls back the delete and any new successor in the same Tx.
// Retrying the unchanged exact owners after removing the fault hands off once.
func runOnceGenerationRollbackKeepsWake(t *testing.T, batch bool) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, guard, owner, ids, scopes, results := runOnceGenerationClaimResults(t, ctx, batch)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		for _, scope := range scopes {
			ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
				runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION test_run_once_refused() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN RAISE EXCEPTION 'synthetic run once refused'; END $$;
			 CREATE TRIGGER test_run_once_refused BEFORE INSERT ON finished_task FOR EACH ROW EXECUTE FUNCTION test_run_once_refused()`))
		})
		var caught error
		if batch {
			var retry bool
			retry, caught = worker.finalizeTaskBatch(results)
			if !retry {
				t.Fatal("known body rollback did not retain ordinary fallback authority", caught)
			}
		} else {
			server.HandleError(func() { worker.finalizeTask(results[0]) }, func(err error) { caught = err })
		}
		if caught == nil || !strings.Contains(caught.Error(), "synthetic run once refused") || len(GetFinishedTasks(ctx, ids...)) != 0 {
			t.Fatal("refused completion did not preserve the original failure and rollback", caught)
		}
		pending := runOnceGenerationPending(ctx, scopes)
		if len(pending) != len(ids) {
			t.Fatal("rollback created or lost a pending owner")
		}
		for index, id := range ids {
			if row := pending[id]; row == nil || row.RunOnceGeneration != 1 || row.ClaimGeneration != results[index].task.ClaimGeneration {
				t.Fatal("rollback lost the exact claimed row and wake")
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER test_run_once_refused ON finished_task`))
		})
		if batch {
			if retry, err := worker.finalizeTaskBatch(results); retry || err != nil {
				t.Fatal("healthy retry failed", retry, err)
			}
		} else {
			worker.finalizeTask(results[0])
		}
		pending = runOnceGenerationPending(ctx, scopes)
		if len(pending) != len(ids) || len(GetFinishedTasks(ctx, ids...)) != len(ids) {
			t.Fatal("restored completion failed exact handoff")
		}
		for _, id := range ids {
			if pending[id] != nil {
				t.Fatal("restored completion reused the old identity")
			}
		}
	})
}

func TestRunOnceOrdinaryRollbackKeepsWake(t *testing.T) {
	runOnceGenerationRollbackKeepsWake(t, false)
}

func TestRunOnceBatchRollbackKeepsWake(t *testing.T) {
	runOnceGenerationRollbackKeepsWake(t, true)
}

// A deferred database refusal occurs only at COMMIT. The conservative unknown
// acknowledgement boundary forbids singles fallback even for this known fault.
func TestRunOnceBatchCommitFailureKeepsExactDirtyOwners(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		worker, guard, owner, ids, scopes, results := runOnceGenerationClaimResults(t, ctx, true)
		defer worker.Close()
		defer guard.release()
		defer owner.Cancel()
		for _, scope := range scopes {
			ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
				runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE SEQUENCE test_run_once_attempt;
			 CREATE FUNCTION test_run_once_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN PERFORM nextval('test_run_once_attempt'); RETURN NEW; END $$;
			 CREATE TRIGGER test_run_once_attempt BEFORE INSERT ON finished_task FOR EACH ROW EXECUTE FUNCTION test_run_once_attempt();
			 CREATE FUNCTION test_run_once_commit_refused() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN RAISE EXCEPTION 'synthetic run once commit refused'; END $$;
			 CREATE CONSTRAINT TRIGGER test_run_once_commit_refused AFTER INSERT ON finished_task
			 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION test_run_once_commit_refused()`))
		})
		retry, err := worker.finalizeTaskBatch(results)
		if retry || err == nil || !strings.Contains(err.Error(), "synthetic run once commit refused") || len(GetFinishedTasks(ctx, ids...)) != 0 {
			t.Fatal("unacknowledged commit permitted fallback or claimed completion", retry, err)
		}
		pending := runOnceGenerationPending(ctx, scopes)
		if len(pending) != 2 || pending[ids[0]] == nil || pending[ids[1]] == nil || pending[ids[0]].RunOnceGeneration != 1 || pending[ids[1]].RunOnceGeneration != 1 {
			t.Fatal("failed commit lost dirty pending custody")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var attempts int
			server.Raise(conn.QueryRow(ctx, `SELECT last_value FROM test_run_once_attempt`).Scan(&attempts))
			if attempts != 2 {
				t.Fatal("unacknowledged completion replayed its results", attempts)
			}
		})
	})
}
