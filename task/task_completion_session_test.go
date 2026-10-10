// A real Run collector must finish on its existing direct connection when that
// connection occupies the entire maintenance pool. Sibling/post custody remains.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type taskCompletionSessionTarget struct {
	*taskQueueProtocolTarget
	fastId       server.Id
	slowEntered  chan struct{}
	releaseBody  <-chan struct{}
	postEntered  chan struct{}
	releasePost  <-chan struct{}
	holdCanceled bool
	bodyCanceled chan struct{}
	calls        atomic.Int32
	posts        atomic.Int32
}

func installTaskCompletionScopeRefusal(ctx context.Context) {
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `CREATE SCHEMA scope_unlock_refusal;
			CREATE FUNCTION scope_unlock_refusal.pg_advisory_unlock(first_key integer,second_key integer)
			RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$ DECLARE released boolean; BEGIN
			released := pg_catalog.pg_advisory_unlock(first_key,second_key);
			IF released THEN RAISE EXCEPTION 'synthetic lost unlock reply' USING ERRCODE='40001'; END IF;
			RETURN released; END $$`))
	}, server.OptReadWrite(), server.OptNoRetry())
}

func (self *taskCompletionSessionTarget) TaskClaimGroupIds(argsJson string) ([]server.Id, int) {
	var args runOnceGenerationArgs
	server.Raise(json.Unmarshal([]byte(argsJson), &args))
	return []server.Id{args.Scope}, 1
}

func (self *taskCompletionSessionTarget) Run(ctx context.Context, queued *Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	self.calls.Add(1)
	if queued.TaskId == self.fastId {
		taskQueueWait(ctx, self.slowEntered)
	} else {
		close(self.slowEntered)
		select {
		case <-self.releaseBody:
		case <-ctx.Done():
			if self.bodyCanceled != nil {
				close(self.bodyCanceled)
			}
			if self.holdCanceled {
				<-self.releaseBody
			}
			return nil, nil, ctx.Err()
		}
	}
	value, post, err := self.taskQueueProtocolTarget.Run(ctx, queued)
	return value, func(tx server.PgTx) ([]server.PostFunction, error) {
		posts, err := post(tx)
		if err == nil && queued.TaskId == self.fastId {
			posts = append(posts, func() any {
				self.posts.Add(1)
				close(self.postEntered)
				<-self.releasePost
				return nil
			})
		}
		return posts, err
	}, err
}

func TestTaskFinalizationReusesOnlyMaintenanceSlotThroughSiblingAndPostJoin(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		scopes := []server.Id{server.NewId(), server.NewId()}
		ids := make([]server.Id, len(scopes))
		for index, scope := range scopes {
			ids[index] = ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, client,
				runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour+time.Duration(index)*time.Second)))
		}
		pop := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		releaseBody, releasePost := make(chan struct{}), make(chan struct{})
		var bodyOnce, postOnce sync.Once
		unblock := func() {
			bodyOnce.Do(func() { close(releaseBody) })
			postOnce.Do(func() { close(releasePost) })
		}
		target := &taskCompletionSessionTarget{
			taskQueueProtocolTarget: &taskQueueProtocolTarget{runOnceGenerationTarget: &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}},
			fastId:                  ids[0], slowEntered: make(chan struct{}), releaseBody: releaseBody,
			postEntered: make(chan struct{}), releasePost: releasePost,
		}
		settings := DefaultTaskWorkerSettings()
		settings.BatchSize = 2
		settings.ClaimRegisteredTargetsOnly = true
		// A finite fixture refusal makes the old-code deadlock decisive. The
		// production handback/admission budgets are unchanged by this patch.
		settings.FinalizeTimeout = time.Second
		worker := NewTaskWorker(ctx, settings)
		worker.AddTargets(target)
		var guard *taskClaimGuard
		var txPosts atomic.Int32
		var poolRefused atomic.Bool
		target.after = func(tx server.PgTx, _ *Task) error {
			if guard == nil || tx.Conn() != guard.conn.Conn() {
				return errors.New("finalization did not use its exact retained claim backend")
			}
			server.AddTxPostCommit(tx, "owned-session-task-counter", func() any { txPosts.Add(1); return nil })
			return nil
		}
		worker.claimAfterCommit = func(owner *taskClaimGuard) {
			guard = owner
			probeCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			probe, err := server.AcquireMaintenanceDbConn(probeCtx)
			cancel()
			if probe != nil {
				probe.Release()
				panic("maintenance fixture unexpectedly admitted a second connection")
			}
			if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				panic("maintenance fixture failed outside its positively occupied capacity")
			}
			poolRefused.Store(true)
		}
		done := make(chan struct{})
		var runErr error
		go func() { defer close(done); server.HandleError(worker.Run, func(err error) { runErr = err }) }()
		defer func() {
			unblock()
			worker.runCancel()
			worker.Close()
			<-done
		}()
		select {
		case <-target.postEntered:
		case <-done:
			if !poolRefused.Load() || !errors.Is(runErr, context.DeadlineExceeded) || target.calls.Load() != 2 || txPosts.Load() != 0 {
				t.Fatal("finalization failed outside the controlled retained maintenance borrow", runErr)
			}
			t.Fatal("returned task attempted another maintenance acquisition while its claim guard held the only slot")
		case <-ctx.Done():
			t.Fatal("actual Run never delivered its committed external post", ctx.Err())
		}
		if !poolRefused.Load() || target.calls.Load() != 2 || txPosts.Load() != 1 || target.posts.Load() != 1 {
			t.Fatal("first real finalization did not commit exactly once with its sibling held")
		}
		server.Db(ctx, func(probe server.PgConn) {
			for index, id := range ids {
				var available bool
				server.Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint)`, taskAdvisoryLockKey(id)).Scan(&available))
				if available {
					server.RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock($1::bigint)`, taskAdvisoryLockKey(id)))
					t.Fatal("a live sibling or committed post lost its exact execution guard")
				}
				group := taskClaimGroupLockKey(target.TargetFunctionName(), scopes[index])
				taskClaimGroupRequireKeys(t, ctx, probe, []taskClaimGroupKey{group}, true)
			}
			var pending, finished int
			server.Raise(probe.QueryRow(ctx, `SELECT (SELECT count(*) FROM pending_task WHERE task_id=ANY($1)),
				(SELECT count(*) FROM finished_task WHERE task_id=ANY($1))`, ids).Scan(&pending, &finished))
			if pending != 1 || finished != 1 {
				t.Fatal("committed post and held sibling lost distinct durable states", pending, finished)
			}
		}, server.OptNoRetry())
		worker.runCancel()
		unblock()
		<-done
		if runErr != nil || target.calls.Load() != 2 || target.posts.Load() != 1 || txPosts.Load() != 2 {
			t.Fatal("Run replayed or lost a returned task/post", runErr, target.calls.Load(), txPosts.Load())
		}
		if len(GetTasks(ctx, ids...)) != 0 || len(GetFinishedTasks(ctx, ids...)) != 2 {
			t.Fatal("same-session finalization did not finish the exact two tasks")
		}
		server.Db(ctx, func(probe server.PgConn) {
			defer func() { server.RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock_all()`)) }()
			for index, id := range ids {
				var available bool
				server.Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint)`, taskAdvisoryLockKey(id)).Scan(&available))
				if !available {
					t.Fatal("joined Run retained an execution guard")
				}
				group := taskClaimGroupLockKey(target.TargetFunctionName(), scopes[index])
				taskClaimGroupRequireKeys(t, ctx, probe, []taskClaimGroupKey{group}, false)
			}
		}, server.OptNoRetry())
	})
}

func TestTaskFinalizationOwnedSessionFencesStaleClaimAndKeepsFutureWake(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		scope := server.NewId()
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, client,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		target := &taskQueueProtocolTarget{runOnceGenerationTarget: &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}}
		worker.AddTargets(target)
		tasks, guard, err := worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(tasks) != 1 || tasks[id] == nil {
			t.Fatal("generation fixture claim failed", err)
		}
		wake := server.NowUtc().Add(time.Minute).Truncate(time.Microsecond)
		ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope, Cursor: 7}, client,
			runOnceGenerationKey(scope), RunAt(wake))
		before := GetTasks(ctx, id)[id]
		if before == nil || before.RunOnceGeneration != tasks[id].RunOnceGeneration+1 {
			t.Fatal("wake did not commit behind the current claim")
		}
		result := worker.executeTask(ctx, tasks[id], target)
		if result.err != nil {
			t.Fatal("generation fixture body failed", result.err)
		}
		staleTask := *tasks[id]
		staleTask.ClaimGeneration--
		staleResult := *result
		staleResult.task = &staleTask
		failure := server.HandleError(func() { worker.finalizeTaskWithGuard(&staleResult, guard) })
		cause, _ := failure.(error)
		if !errors.Is(cause, errTaskClaimOwnership) || guard.completionSessionError() != nil ||
			!reflect.DeepEqual(before, GetTasks(ctx, id)[id]) || len(GetFinishedTasks(ctx, id)) != 0 {
			t.Fatal("stale donated finalizer changed the current durable owner", failure)
		}
		posts, postRescheduled := worker.finalizeTaskWithGuard(result, guard)
		if postRescheduled || len(posts) != 0 {
			t.Fatal("ordinary generation handback changed publication policy")
		}
		pending := runOnceGenerationPending(ctx, []server.Id{scope})
		if len(pending) != 1 || len(GetFinishedTasks(ctx, id)) != 1 {
			t.Fatal("current owner lost the future successor")
		}
		for successorId, queued := range pending {
			if successorId == id || !queued.RunAt.Equal(wake) || queued.ClaimGeneration != 0 || queued.ArgsJson != before.ArgsJson {
				t.Fatal("donated finalizer changed successor identity, deadline or retained args")
			}
		}
	})
}

// The server performs one real unlock and then loses its successful reply to a
// typed statement error. The committed owner/post must survive, while its live
// sibling is canceled and joined before any caller-owned lock can be released.
func TestTaskFinalizationUnknownScopeCleanupKeepsCommittedPostAndSiblingCustody(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		installTaskCompletionScopeRefusal(ctx)
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		scopes := []server.Id{server.NewId(), server.NewId()}
		ids := make([]server.Id, len(scopes))
		for index, scope := range scopes {
			ids[index] = ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, client,
				runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour+time.Duration(index)*time.Second)))
		}
		releaseBody, releasePost := make(chan struct{}), make(chan struct{})
		var bodyOnce, postOnce sync.Once
		unblock := func() { bodyOnce.Do(func() { close(releaseBody) }); postOnce.Do(func() { close(releasePost) }) }
		target := &taskCompletionSessionTarget{
			taskQueueProtocolTarget: &taskQueueProtocolTarget{runOnceGenerationTarget: &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}},
			fastId:                  ids[0], slowEntered: make(chan struct{}), releaseBody: releaseBody,
			postEntered: make(chan struct{}), releasePost: releasePost, holdCanceled: true, bodyCanceled: make(chan struct{}),
		}
		settings := DefaultTaskWorkerSettings()
		settings.BatchSize = 2
		settings.ClaimRegisteredTargetsOnly = true
		worker := NewTaskWorker(ctx, settings)
		worker.AddTargets(target)
		var guard *taskClaimGuard
		worker.claimAfterCommit = func(owner *taskClaimGuard) {
			guard = owner
			server.RaisePgResult(owner.conn.Exec(ctx, `SET search_path=scope_unlock_refusal,pg_catalog,public`))
		}
		done := make(chan struct{})
		var runErr error
		go func() { defer close(done); server.HandleError(worker.Run, func(err error) { runErr = err }) }()
		defer func() { unblock(); worker.runCancel(); worker.Close(); <-done }()
		select {
		case <-target.postEntered:
		case <-done:
			t.Fatal("unknown cleanup erased an acknowledged external post", runErr)
		case <-ctx.Done():
			t.Fatal("unknown cleanup fixture never reached its committed post", ctx.Err())
		}
		select {
		case <-target.bodyCanceled:
		case <-done:
			t.Fatal("collector surrendered custody before its held sibling joined", runErr)
		case <-ctx.Done():
			t.Fatal("quarantined collector did not cancel its live sibling", ctx.Err())
		}
		var pgErr *pgconn.PgError
		if guard == nil || !errors.As(guard.completionSessionError(), &pgErr) || pgErr.Code != "40001" || target.posts.Load() != 1 {
			t.Fatal("real lost unlock did not quarantine the donated session")
		}
		pending := GetTasks(ctx, ids...)
		finished := GetFinishedTasks(ctx, ids...)
		if len(pending) != 1 || pending[ids[1]] == nil || pending[ids[1]].RescheduleErrorCount != 0 ||
			len(finished) != 1 || finished[ids[0]] == nil {
			t.Fatal("cleanup uncertainty changed acknowledged completion or unjoined sibling state")
		}
		server.Db(ctx, func(probe server.PgConn) {
			for index, id := range ids {
				var available bool
				server.Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint)`, taskAdvisoryLockKey(id)).Scan(&available))
				if available {
					server.RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock($1::bigint)`, taskAdvisoryLockKey(id)))
					t.Fatal("unknown scoped cleanup released a live execution owner")
				}
				taskClaimGroupRequireKeys(t, ctx, probe, []taskClaimGroupKey{taskClaimGroupLockKey(target.TargetFunctionName(), scopes[index])}, true)
			}
		}, server.OptNoRetry())
		worker.runCancel()
		unblock()
		<-done
		if runErr == nil || target.calls.Load() != 2 || target.posts.Load() != 1 ||
			!reflect.DeepEqual(pending, GetTasks(ctx, ids...)) || len(GetFinishedTasks(ctx, ids...)) != 1 {
			t.Fatal("quarantined collector replayed or changed an unacknowledged sibling", runErr)
		}
		server.Db(ctx, func(probe server.PgConn) {
			defer func() { server.RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock_all()`)) }()
			for index, id := range ids {
				var available bool
				server.Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint)`, taskAdvisoryLockKey(id)).Scan(&available))
				if !available {
					t.Fatal("joined uncertain collector retained its execution owner")
				}
				taskClaimGroupRequireKeys(t, ctx, probe, []taskClaimGroupKey{taskClaimGroupLockKey(target.TargetFunctionName(), scopes[index])}, false)
			}
		}, server.OptNoRetry())
	})
}

func TestFiniteFinalizationUnknownScopeCleanupJoinsSiblingAndCommittedPost(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		installTaskCompletionScopeRefusal(ctx)
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		scopes := []server.Id{server.NewId(), server.NewId()}
		ids := make([]server.Id, len(scopes))
		for index, scope := range scopes {
			ids[index] = ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, client,
				runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour+time.Duration(index)*time.Second)))
		}
		releaseBody, releasePost := make(chan struct{}), make(chan struct{})
		var bodyOnce, postOnce sync.Once
		unblock := func() { bodyOnce.Do(func() { close(releaseBody) }); postOnce.Do(func() { close(releasePost) }) }
		target := &taskCompletionSessionTarget{
			taskQueueProtocolTarget: &taskQueueProtocolTarget{runOnceGenerationTarget: &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}},
			fastId:                  ids[0], slowEntered: make(chan struct{}), releaseBody: releaseBody,
			postEntered: make(chan struct{}), releasePost: releasePost, holdCanceled: true, bodyCanceled: make(chan struct{}),
		}
		worker := NewTaskWorkerWithDefaults(ctx)
		worker.AddTargets(target)
		var guard *taskClaimGuard
		worker.claimAfterCommit = func(owner *taskClaimGuard) {
			guard = owner
			server.RaisePgResult(owner.conn.Exec(ctx, `SET search_path=scope_unlock_refusal,pg_catalog,public`))
		}
		done := make(chan struct{})
		var evalErr error
		go func() {
			defer close(done)
			server.HandleError(func() { _, _, _, evalErr = worker.EvalTasks(2) }, func(err error) { evalErr = err })
		}()
		defer func() { unblock(); worker.Close(); <-done }()
		select {
		case <-target.bodyCanceled:
		case <-done:
			t.Fatal("finite collector released before its held sibling acknowledged cancellation", evalErr)
		case <-ctx.Done():
			t.Fatal("finite collector did not cancel its quarantined scope's sibling", ctx.Err())
		}
		var pgErr *pgconn.PgError
		if guard == nil || !errors.As(guard.completionSessionError(), &pgErr) || pgErr.Code != "40001" {
			t.Fatal("finite fixture failed outside the real scoped cleanup refusal")
		}
		pending := GetTasks(ctx, ids...)
		if len(pending) != 1 || pending[ids[1]] == nil || len(GetFinishedTasks(ctx, ids...)) != 1 || target.posts.Load() != 0 {
			t.Fatal("finite collector erased committed state or ran external posts before sibling join")
		}
		requireHeld := func() {
			server.Db(ctx, func(probe server.PgConn) {
				for index, id := range ids {
					var available bool
					server.Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint)`, taskAdvisoryLockKey(id)).Scan(&available))
					if available {
						server.RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock($1::bigint)`, taskAdvisoryLockKey(id)))
						t.Fatal("finite collector released a caller key before every join")
					}
					taskClaimGroupRequireKeys(t, ctx, probe, []taskClaimGroupKey{taskClaimGroupLockKey(target.TargetFunctionName(), scopes[index])}, true)
				}
			}, server.OptNoRetry())
		}
		requireHeld()
		bodyOnce.Do(func() { close(releaseBody) })
		select {
		case <-target.postEntered:
		case <-done:
			t.Fatal("finite collector lost its acknowledged post while reporting cleanup failure", evalErr)
		case <-ctx.Done():
			t.Fatal("finite committed post did not join after its sibling", ctx.Err())
		}
		requireHeld()
		postOnce.Do(func() { close(releasePost) })
		<-done
		if evalErr == nil || target.calls.Load() != 2 || target.posts.Load() != 1 ||
			!reflect.DeepEqual(pending, GetTasks(ctx, ids...)) || len(GetFinishedTasks(ctx, ids...)) != 1 {
			t.Fatal("finite uncertain collector replayed or changed its unacknowledged sibling", evalErr)
		}
		server.Db(ctx, func(probe server.PgConn) {
			defer func() { server.RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock_all()`)) }()
			for index, id := range ids {
				var available bool
				server.Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint)`, taskAdvisoryLockKey(id)).Scan(&available))
				if !available {
					t.Fatal("joined finite collector retained its execution key")
				}
				taskClaimGroupRequireKeys(t, ctx, probe, []taskClaimGroupKey{taskClaimGroupLockKey(target.TargetFunctionName(), scopes[index])}, false)
			}
		}, server.OptNoRetry())
	})
}
