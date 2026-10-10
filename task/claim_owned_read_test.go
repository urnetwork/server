// A committed claim reads through its existing direct execution owner.
package task

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Only the interval after a real claim COMMIT is observed. Physical identities
// and finite counts prove reuse without replacing any query or pool operation.
type taskClaimReadCounts struct {
	ownerPid     uint32
	acquires     int
	reads        int
	foreignReads int
}

type taskClaimReadObserver struct {
	mutex  sync.Mutex
	active bool
	counts taskClaimReadCounts
}

func (self *taskClaimReadObserver) begin(pid uint32) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.active = true
	self.counts = taskClaimReadCounts{ownerPid: pid}
}

func (self *taskClaimReadObserver) finish() taskClaimReadCounts {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.active = false
	return self.counts
}

func (self *taskClaimReadObserver) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.active {
		self.counts.acquires++
	}
	return ctx
}

func (self *taskClaimReadObserver) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
}

func (self *taskClaimReadObserver) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.active && strings.Contains(data.SQL, "pending_task.client_address_port") &&
		strings.Contains(data.SQL, "pending_task.reschedule_error_count") {
		self.counts.reads++
		if conn.PgConn().PID() != self.counts.ownerPid {
			self.counts.foreignReads++
		}
	}
	return ctx
}

func (self *taskClaimReadObserver) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// An unavailable ordinary pool must not strand acknowledged claims before any
// function runs. Both the first claim and a >32-row refill keep the same direct
// owner, and the real completion path retains every task exactly once.
func TestTaskClaimReadReusesGuardWhileOrdinaryPoolOccupied(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		ids := make([]server.Id, 34)
		for index := range ids {
			scope := server.NewId()
			ids[index] = ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope, Cursor: index}, client,
				runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour+time.Duration(index)*time.Second)))
		}
		pop := server.Config.PushSimpleResource(server.DefaultPgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		observer := &taskClaimReadObserver{}
		queryScope, err := server.NewTestPgQueryScope(ctx, observer)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := queryScope.Close(); err != nil {
				t.Error(err)
			}
		}()
		calls := map[server.Id]int{}
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
		target.before = func(_ context.Context, queued *Task) error {
			calls[queued.TaskId]++
			return nil
		}
		settings := DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		// Bound the deliberately blocked old-code read; this is not a runtime
		// timeout change or an elapsed-time performance assertion.
		settings.FinalizeTimeout = 2 * time.Second
		worker := NewTaskWorker(ctx, settings)
		worker.AddTargets(target)
		defer worker.Close()
		var guard *taskClaimGuard
		defer func() { guard.release() }()
		commits := 0
		worker.claimAfterCommit = func(committed *taskClaimGuard) {
			commits++
			observer.begin(committed.conn.Conn().PgConn().PID())
		}
		claimed := map[server.Id]*Task{}
		server.Db(ctx, func(occupied server.PgConn) {
			// Positive resource control: this actual one-entry ordinary pool
			// cannot admit another callback while its one slot remains held.
			probeCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			entered := false
			refusal := server.HandleError(func() {
				server.Db(probeCtx, func(server.PgConn) { entered = true }, server.OptNoRetry())
			})
			cancel()
			cause, _ := refusal.(error)
			if entered || !errors.Is(cause, context.DeadlineExceeded) || ctx.Err() != nil {
				t.Fatal("ordinary-pool occupancy control did not exhaust exactly its available slot", refusal)
			}
			var ownerPid uint32
			for turn, count := range []int{1, 33} {
				prior := guard
				var tasks map[server.Id]*Task
				var claimErr error
				failure := server.HandleError(func() {
					var retained *taskClaimGuard
					tasks, retained, _, claimErr = worker.takeTasksWithGuard(ctx, count, guard,
						taskClaimOptions{detachCommittedRead: true, runCohorts: true})
					if retained != nil {
						guard = retained
					}
				})
				observed := observer.finish()
				var committed int
				server.Raise(occupied.QueryRow(ctx, `SELECT count(*) FROM pending_task
					WHERE task_id=ANY($1) AND claim_generation=1 AND release_time>clock_timestamp() AT TIME ZONE 'UTC'
					AND reschedule_error_count=0`, ids).Scan(&committed))
				if commits != turn+1 || committed != 1+33*turn || len(calls) != 0 || ctx.Err() != nil {
					t.Fatal("claim-delivery fixture did not retain its real committed but unexecuted prefix")
				}
				if failure != nil || claimErr != nil {
					cause, _ := failure.(error)
					if !errors.Is(cause, context.DeadlineExceeded) || observed.acquires != 1 || observed.reads != 0 {
						t.Fatal("claim failed outside the controlled post-COMMIT ordinary acquisition", failure, claimErr, observed)
					}
					t.Fatal("committed claim attempted another PostgreSQL acquisition before delivering its tasks")
				}
				if guard == nil || prior != nil && guard != prior || len(tasks) != count ||
					observed.acquires != 0 || observed.reads != 1 || observed.foreignReads != 0 {
					t.Fatal("post-COMMIT read did not reuse its exact serialized claim owner", observed, len(tasks))
				}
				if turn == 0 {
					ownerPid = observed.ownerPid
				}
				if observed.ownerPid == 0 || observed.ownerPid != ownerPid || ownerPid == occupied.Conn().PgConn().PID() {
					t.Fatal("claim delivery changed its retained direct physical session")
				}
				for id, queued := range tasks {
					if claimed[id] != nil || queued.ClaimGeneration != 1 || queued.RunOnceGeneration != 0 {
						t.Fatal("delivery repeated a task or replaced its committed generation")
					}
					claimed[id] = queued
					var available bool
					server.Raise(occupied.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, taskAdvisoryLockKey(id)).Scan(&available))
					if available {
						server.RaisePgResult(occupied.Exec(ctx, `SELECT pg_advisory_unlock($1)`, taskAdvisoryLockKey(id)))
						t.Fatal("delivered task lost its exact execution guard")
					}
				}
			}
		}, server.OptNoRetry())
		// The unrelated borrower has now released the ordinary pool. Actual
		// bodies and fenced financial-neutral completion can use it normally.
		for _, id := range ids {
			queued := claimed[id]
			if queued == nil {
				t.Fatal("claimed prefix omitted an exact scheduled task")
			}
			result := worker.executeTask(ctx, queued, target)
			if result.err != nil {
				t.Fatal("delivered task did not execute", result.err)
			}
			posts, retried := worker.finalizeTask(result)
			server.RunPosts(ctx, posts...)
			if retried || calls[id] != 1 {
				t.Fatal("delivered task repeated execution or required a post retry")
			}
		}
		if len(GetTasks(ctx, ids...)) != 0 || len(GetFinishedTasks(ctx, ids...)) != len(ids) || len(guard.taskIds) != len(ids) {
			t.Fatal("claim delivery lost a durable completion or released guards before join")
		}
		guard.release()
		server.Db(ctx, func(probe server.PgConn) {
			for _, id := range ids {
				var available bool
				server.Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, taskAdvisoryLockKey(id)).Scan(&available))
				if available {
					server.RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock($1)`, taskAdvisoryLockKey(id)))
				}
				if !available {
					t.Fatal("joined claim retained an execution guard")
				}
			}
		}, server.OptNoRetry())
	})
}

// A failed fresh read is not replayed or treated as a failed claim COMMIT.
// During refill, all earlier and newly committed guards remain owned together.
func TestTaskClaimRefillReadFailureKeepsCommittedGuardWithoutReplay(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		ids := make([]server.Id, 2)
		for index := range ids {
			ids[index] = ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, client,
				RunAt(server.NowUtc().Add(-time.Hour+time.Duration(index)*time.Minute)))
		}
		worker := runOnceGenerationWorker(ctx, &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		defer worker.Close()
		claimed, guard, err := worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard == nil || len(claimed) != 1 || claimed[ids[0]] == nil {
			t.Fatal("refill failure control lacks its first committed sibling", err)
		}
		worker.claimAfterCommit = func(*taskClaimGuard) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `CREATE SEQUENCE task_owned_read_attempt;
					CREATE FUNCTION task_owned_read_fail_once(value text) RETURNS text LANGUAGE plpgsql VOLATILE AS $$
					BEGIN
						IF nextval('task_owned_read_attempt')=1 THEN
							RAISE EXCEPTION 'synthetic owned task read failure' USING ERRCODE='40001';
						END IF;
						RETURN value;
					END $$;
					ALTER TABLE pending_task RENAME TO pending_task_read_fixture;
					CREATE VIEW pending_task AS SELECT (jsonb_populate_record(NULL::pending_task_read_fixture,
						to_jsonb(p) || jsonb_build_object('function_name',task_owned_read_fail_once(p.function_name)))).*
					FROM pending_task_read_fixture p`))
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		failure := server.HandleError(func() {
			_, _, _, err := worker.takeTasksWithGuard(ctx, 1, guard, taskClaimOptions{runCohorts: true})
			server.Raise(err)
		})
		cause, _ := failure.(error)
		var pgErr *pgconn.PgError
		var attempts int64
		var committed bool
		server.Raise(guard.conn.QueryRow(ctx, `SELECT last_value FROM task_owned_read_attempt`).Scan(&attempts))
		server.Raise(guard.conn.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(claim_generation=1
			AND release_time>clock_timestamp() AT TIME ZONE 'UTC' AND reschedule_error_count=0)
			FROM pending_task_read_fixture WHERE task_id=ANY($1)`, ids).Scan(&committed))
		if !errors.As(cause, &pgErr) || pgErr.Code != "40001" || attempts != 1 || !committed || len(guard.taskIds) != 2 {
			t.Fatal("failed refill read replayed or discarded acknowledged claim custody", failure, attempts)
		}
		server.Db(ctx, func(probe server.PgConn) {
			for _, id := range ids {
				var available bool
				server.Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, taskAdvisoryLockKey(id)).Scan(&available))
				if available {
					server.RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock($1)`, taskAdvisoryLockKey(id)))
					t.Fatal("refill read failure released a committed sibling guard")
				}
			}
		}, server.OptNoRetry())
	})
}
