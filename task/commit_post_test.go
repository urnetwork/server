// Posts that return work for after the finishing transaction commits. The
// fixtures fail that commit at the exact boundary that matters: a post writes a
// marker row, and a deferred constraint trigger on the marker table raises at
// commit, after every post in the transaction has run. A serialization failure
// makes `server.Tx` rerun the callback; a plain error rolls the finish back.
package task

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Names the marker row the post writes.
type commitPostArgs struct {
	MarkerId server.Id `json:"marker_id"`
}

// The work has nothing to report.
type commitPostResult struct{}

// Completes, so its post runs in the finishing transaction.
func commitPostWork(_ *commitPostArgs, _ *session.ClientSession) (*commitPostResult, error) {
	return &commitPostResult{}, nil
}

// The first `transientCount` commits that write a marker fail with a
// serialization failure; with `rollback`, every later one fails too.
func createCommitPostFault(ctx context.Context, transientCount int, rollback bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
			CREATE TABLE synthetic_commit_post_marker (marker_id uuid NOT NULL);
			CREATE SEQUENCE synthetic_commit_post_commit;
			CREATE FUNCTION synthetic_commit_post_fault() RETURNS trigger AS $fault$
			BEGIN
				IF nextval('synthetic_commit_post_commit') <= %d THEN
					RAISE EXCEPTION 'synthetic serialization failure at commit' USING ERRCODE = '40001';
				END IF;
				IF %t THEN
					RAISE EXCEPTION 'synthetic failure at commit';
				END IF;
				RETURN NULL;
			END
			$fault$ LANGUAGE plpgsql;
			CREATE CONSTRAINT TRIGGER synthetic_commit_post_fault
				AFTER INSERT ON synthetic_commit_post_marker
				DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW EXECUTE FUNCTION synthetic_commit_post_fault();
		`, transientCount, rollback)))
	})
}

// Writes in the post's transaction.
func writeCommitPostMarker(ctx context.Context, tx server.PgTx, markerId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`INSERT INTO synthetic_commit_post_marker (marker_id) VALUES ($1)`,
		markerId,
	))
}

// Reads on a separate pooled connection, so the marker shows only once the
// transaction that wrote it has committed.
func commitPostMarkerCommitted(ctx context.Context, markerId server.Id) (committed bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM synthetic_commit_post_marker WHERE marker_id = $1)`,
			markerId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&committed))
			}
		})
	})
	return
}

// A worker for the one commit post target, claiming nothing else.
func newCommitPostWorker(
	ctx context.Context,
	postFunction TaskCommitPostFunction[*commitPostArgs, *commitPostResult],
) *TaskWorker {
	settings := DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := NewTaskWorker(ctx, settings)
	worker.AddTargets(NewTaskTargetWithCommitPost(commitPostWork, postFunction))
	return worker
}

// Due an hour ago, so the next claim takes it.
func scheduleCommitPostWork(clientSession *session.ClientSession) server.Id {
	return ScheduleTask(
		commitPostWork,
		&commitPostArgs{MarkerId: server.NewId()},
		clientSession,
		RunAt(server.NowUtc().Add(-time.Hour)),
	)
}

// Counts the posts (numbered from 1, across the finish and its retries) and
// their work; the work records whether the post's marker had committed when it
// ran. The posts in `failedPostNumbers` fail, returning work that must never
// run; the posts in `unmarkedPostNumbers` write no marker.
type commitPostProbe struct {
	ctx                 context.Context
	failedPostNumbers   map[int32]bool
	unmarkedPostNumbers map[int32]bool
	postCount           atomic.Int32
	workCount           atomic.Int32
	workCommitted       atomic.Bool
	failedPostWorkRan   atomic.Bool
}

// A `TaskCommitPostFunction` that writes the marker and returns the work.
func (self *commitPostProbe) post(
	args *commitPostArgs,
	_ *commitPostResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) ([]server.PostFunction, error) {
	postCount := self.postCount.Add(1)
	if !self.unmarkedPostNumbers[postCount] {
		writeCommitPostMarker(clientSession.Ctx, tx, args.MarkerId)
	}
	if self.failedPostNumbers[postCount] {
		return []server.PostFunction{func() any {
			self.failedPostWorkRan.Store(true)
			return nil
		}}, fmt.Errorf("synthetic post failure %d", postCount)
	}
	return []server.PostFunction{func() any {
		self.workCount.Add(1)
		self.workCommitted.Store(commitPostMarkerCommitted(self.ctx, args.MarkerId))
		return nil
	}}, nil
}

// The work runs once, after the finishing transaction committed.
func TestCommitPostRunsAfterTheFinishCommits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		createCommitPostFault(ctx, 0, false)

		probe := &commitPostProbe{ctx: ctx}
		worker := newCommitPostWorker(ctx, probe.post)
		defer worker.Close()
		taskId := scheduleCommitPostWork(clientSession)

		finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds, err := worker.EvalTasks(1)
		if err != nil || len(finishedTaskIds) != 1 || finishedTaskIds[0] != taskId || len(rescheduledTaskIds)+len(postRescheduledTaskIds) != 0 {
			t.Fatalf("finish = %v/%v/%v err=%v, want %s finished", finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds, err, taskId)
		}
		if workCount := probe.workCount.Load(); workCount != 1 {
			t.Fatalf("the post's work ran %d times, want once", workCount)
		}
		if !probe.workCommitted.Load() {
			t.Fatal("the post's work ran before the finishing transaction committed")
		}
	})
}

// A finish that rolls back drops the work, and the task stays pending for a
// later finish.
func TestCommitPostDoesNotRunWhenTheFinishRollsBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		createCommitPostFault(ctx, 0, true)

		probe := &commitPostProbe{ctx: ctx}
		worker := newCommitPostWorker(ctx, probe.post)
		defer worker.Close()
		taskId := scheduleCommitPostWork(clientSession)

		var evalPanic any
		func() {
			defer func() {
				evalPanic = recover()
			}()
			worker.EvalTasks(1)
		}()
		if err, ok := evalPanic.(error); !ok || !strings.Contains(err.Error(), "synthetic failure at commit") {
			t.Fatalf("finish panic = %v, want the commit failure", evalPanic)
		}
		if postCount := probe.postCount.Load(); postCount != 1 {
			t.Fatalf("the post ran %d times, want once", postCount)
		}
		if workCount := probe.workCount.Load(); workCount != 0 {
			t.Fatalf("the work of a rolled-back finish ran %d times", workCount)
		}
		if _, ok := GetTasks(ctx, taskId)[taskId]; !ok {
			t.Fatal("the rolled-back finish removed the pending task")
		}
		if _, ok := GetFinishedTasks(ctx, taskId)[taskId]; ok {
			t.Fatal("the rolled-back finish left a finished task")
		}
	})
}

// A finish whose callback reruns after a serialization failure at commit runs
// the work of the committed attempt only.
func TestCommitPostRunsOnceWhenTheFinishReruns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		createCommitPostFault(ctx, 1, false)

		probe := &commitPostProbe{ctx: ctx}
		worker := newCommitPostWorker(ctx, probe.post)
		defer worker.Close()
		taskId := scheduleCommitPostWork(clientSession)

		finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds, err := worker.EvalTasks(1)
		if err != nil || len(finishedTaskIds) != 1 || finishedTaskIds[0] != taskId || len(rescheduledTaskIds)+len(postRescheduledTaskIds) != 0 {
			t.Fatalf("finish = %v/%v/%v err=%v, want %s finished", finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds, err, taskId)
		}
		if postCount := probe.postCount.Load(); postCount != 2 {
			t.Fatalf("the post ran %d times, want 2 (the finish did not rerun)", postCount)
		}
		if workCount := probe.workCount.Load(); workCount != 1 {
			t.Fatalf("the post's work ran %d times, want once", workCount)
		}
		if !probe.workCommitted.Load() {
			t.Fatal("the post's work ran before the finishing transaction committed")
		}
	})
}

// A post error recorded by a rolled-back attempt does not outlive it: the
// committed attempt's post succeeded, so the task is reported finished and no
// post retry exists.
func TestFinishReportsOnlyTheCommittedAttemptPostErrors(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		createCommitPostFault(ctx, 1, false)

		probe := &commitPostProbe{ctx: ctx, failedPostNumbers: map[int32]bool{1: true}}
		worker := newCommitPostWorker(ctx, probe.post)
		defer worker.Close()
		taskId := scheduleCommitPostWork(clientSession)

		finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds, err := worker.EvalTasks(1)
		if err != nil || len(finishedTaskIds) != 1 || finishedTaskIds[0] != taskId || len(rescheduledTaskIds)+len(postRescheduledTaskIds) != 0 {
			t.Fatalf("finish = %v/%v/%v err=%v, want %s finished without a post retry", finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds, err, taskId)
		}
		if postCount := probe.postCount.Load(); postCount != 2 {
			t.Fatalf("the post ran %d times, want 2 (the finish did not rerun)", postCount)
		}
		finishedTask, ok := GetFinishedTasks(ctx, taskId)[taskId]
		if !ok || finishedTask.PostError != "" {
			t.Fatalf("finished task = %+v, want no post error", finishedTask)
		}
		if workCount := probe.workCount.Load(); workCount != 1 || probe.failedPostWorkRan.Load() {
			t.Fatalf("work ran %d times (failed post's work ran: %t), want the committed post's work once", workCount, probe.failedPostWorkRan.Load())
		}
	})
}

// The post retry runs the work after its own transaction commits, once, when
// that transaction reruns.
func TestRunPostRunsCommitPostAfterItsTransactionCommits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		createCommitPostFault(ctx, 1, false)

		// the finish records the first post's error without a marker, so the
		// fault applies to the retry's transaction
		probe := &commitPostProbe{
			ctx:                 ctx,
			failedPostNumbers:   map[int32]bool{1: true},
			unmarkedPostNumbers: map[int32]bool{1: true},
		}
		worker := newCommitPostWorker(ctx, probe.post)
		defer worker.Close()
		taskId := scheduleCommitPostWork(clientSession)

		_, _, postRescheduledTaskIds, err := worker.EvalTasks(1)
		if err != nil || len(postRescheduledTaskIds) != 1 || postRescheduledTaskIds[0] != taskId {
			t.Fatalf("finish post retries = %v err=%v, want %s", postRescheduledTaskIds, err, taskId)
		}
		if probe.workCount.Load() != 0 || probe.failedPostWorkRan.Load() {
			t.Fatal("the failed post's work ran")
		}

		result, err := worker.RunPost(&RunPostArgs{TaskId: taskId}, clientSession)
		if err != nil || result == nil {
			t.Fatalf("post retry = %v err=%v", result, err)
		}
		if postCount := probe.postCount.Load(); postCount != 3 {
			t.Fatalf("the post ran %d times, want 3 (the retry's transaction did not rerun)", postCount)
		}
		if workCount := probe.workCount.Load(); workCount != 1 {
			t.Fatalf("the retried post's work ran %d times, want once", workCount)
		}
		if !probe.workCommitted.Load() {
			t.Fatal("the retried post's work ran before its transaction committed")
		}
	})
}

// A post retry whose failed attempt rolls back and whose rerun succeeds reports
// the committed success. Reporting the rolled-back error would reschedule the
// retry and run the post, and its work, a second time.
func TestRunPostReportsOnlyTheCommittedAttemptOutcome(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		createCommitPostFault(ctx, 1, false)

		// the finish's post fails without a marker; the retry's first attempt
		// fails with one, so its commit fails and the transaction reruns
		probe := &commitPostProbe{
			ctx:                 ctx,
			failedPostNumbers:   map[int32]bool{1: true, 2: true},
			unmarkedPostNumbers: map[int32]bool{1: true},
		}
		worker := newCommitPostWorker(ctx, probe.post)
		defer worker.Close()
		taskId := scheduleCommitPostWork(clientSession)

		_, _, postRescheduledTaskIds, err := worker.EvalTasks(1)
		if err != nil || len(postRescheduledTaskIds) != 1 || postRescheduledTaskIds[0] != taskId {
			t.Fatalf("finish post retries = %v err=%v, want %s", postRescheduledTaskIds, err, taskId)
		}

		result, err := worker.RunPost(&RunPostArgs{TaskId: taskId}, clientSession)
		if postCount := probe.postCount.Load(); postCount != 3 {
			t.Fatalf("the post ran %d times, want 3 (the retry's transaction did not rerun)", postCount)
		}
		if err != nil || result == nil {
			t.Fatalf("post retry = %v err=%v, want the committed success", result, err)
		}
		if workCount := probe.workCount.Load(); workCount != 1 || probe.failedPostWorkRan.Load() {
			t.Fatalf("work ran %d times (failed post's work ran: %t), want the committed post's work once", workCount, probe.failedPostWorkRan.Load())
		}
		if !probe.workCommitted.Load() {
			t.Fatal("the retried post's work ran before its transaction committed")
		}
	})
}

// A post retry whose first attempt succeeds but rolls back, and whose rerun
// fails, runs no work: the rolled-back attempt's work must not outlive it.
func TestRunPostDropsTheWorkOfARolledBackAttempt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		createCommitPostFault(ctx, 1, false)

		// the finish's post fails without a marker; the retry's first attempt
		// succeeds with one, so its commit fails, and the rerun's post fails
		probe := &commitPostProbe{
			ctx:                 ctx,
			failedPostNumbers:   map[int32]bool{1: true, 3: true},
			unmarkedPostNumbers: map[int32]bool{1: true},
		}
		worker := newCommitPostWorker(ctx, probe.post)
		defer worker.Close()
		taskId := scheduleCommitPostWork(clientSession)

		_, _, postRescheduledTaskIds, err := worker.EvalTasks(1)
		if err != nil || len(postRescheduledTaskIds) != 1 || postRescheduledTaskIds[0] != taskId {
			t.Fatalf("finish post retries = %v err=%v, want %s", postRescheduledTaskIds, err, taskId)
		}

		result, err := worker.RunPost(&RunPostArgs{TaskId: taskId}, clientSession)
		if postCount := probe.postCount.Load(); postCount != 3 {
			t.Fatalf("the post ran %d times, want 3 (the retry's transaction did not rerun)", postCount)
		}
		if err == nil || !strings.Contains(err.Error(), "synthetic post failure 3") {
			t.Fatalf("post retry = %v err=%v, want the rerun's failure", result, err)
		}
		if workCount := probe.workCount.Load(); workCount != 0 || probe.failedPostWorkRan.Load() {
			t.Fatalf("work ran %d times (failed post's work ran: %t), want none", workCount, probe.failedPostWorkRan.Load())
		}
	})
}
