// Exact-ID reads retain fresh committed state without an extra transaction.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestGetTasksSmallReadUsesReadCommittedSnapshot(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer owner.Cancel()
		ids := make([]server.Id, 33)
		for index := range ids {
			scope := server.NewId()
			options := []any{RunAt(server.NowUtc().Add(-time.Hour))}
			if index%2 == 0 {
				options = append(options, runOnceGenerationKey(scope))
			}
			ids[index] = ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope, Cursor: index}, owner, options...)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET reschedule_error='synthetic retained read metadata',reschedule_error_count=3 WHERE task_id=$1`, ids[0]))
		}, server.TxReadCommitted, server.OptNoRetry())
		want := GetTasks(ctx, ids...)
		for _, count := range []int{1, 3, 31, 32} {
			got := GetTasks(ctx, ids[:count]...)
			if len(got) != count {
				t.Fatal("exact-ID read lost a requested row", count, len(got))
			}
			for _, id := range ids[:count] {
				if !reflect.DeepEqual(got[id], want[id]) {
					t.Fatal("small/large task reads disagree on a complete stored row", count)
				}
			}
		}
		duplicate := GetTasks(ctx, ids[0], server.NewId(), ids[0])
		if len(duplicate) != 1 || !reflect.DeepEqual(duplicate[ids[0]], want[ids[0]]) || len(GetTasks(ctx)) != 0 {
			t.Fatal("exact-ID read changed empty, absent or duplicate identity behavior")
		}
		// Expose the actual statement's isolation through the same read path.
		// Small reads use PostgreSQL's fresh read-committed snapshot; the large
		// transaction-local table keeps its existing repeatable-read behavior.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE pending_task RENAME TO pending_task_read_fixture;
                CREATE VIEW pending_task AS SELECT (jsonb_populate_record(NULL::pending_task_read_fixture,
                    to_jsonb(p) || jsonb_build_object('args_json',
                        jsonb_build_object('isolation',current_setting('transaction_isolation'))::text))).*
                FROM pending_task_read_fixture p`))
		}, server.TxReadCommitted, server.OptNoRetry())
		server.PgReset()
		for _, count := range []int{1, 3, 31, 32} {
			got := GetTasks(ctx, ids[:count]...)
			if len(got) != count {
				t.Fatal("native statement probe lost a requested identity", count)
			}
			for _, queued := range got {
				var proof struct {
					Isolation string `json:"isolation"`
				}
				server.Raise(json.Unmarshal([]byte(queued.ArgsJson), &proof))
				wantIsolation := "read committed"
				if count >= 32 {
					wantIsolation = "repeatable read"
				}
				if proof.Isolation != wantIsolation {
					t.Fatal("exact-ID read used the wrong snapshot isolation", count, proof)
				}
			}
		}
	})
}

func TestGetTasksSmallReadDoesNotReplayFailedStatement(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		id := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId()}, owner)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE SEQUENCE task_read_attempt;
                CREATE FUNCTION task_read_fail_once(value text) RETURNS text LANGUAGE plpgsql VOLATILE AS $$
                BEGIN
                    IF nextval('task_read_attempt')=1 THEN
                        RAISE EXCEPTION 'synthetic task read failure' USING ERRCODE='40001';
                    END IF;
                    RETURN value;
                END $$;
                ALTER TABLE pending_task RENAME TO pending_task_read_fixture;
                CREATE VIEW pending_task AS SELECT (jsonb_populate_record(NULL::pending_task_read_fixture,
                    to_jsonb(p) || jsonb_build_object('function_name',task_read_fail_once(p.function_name)))).*
                FROM pending_task_read_fixture p`))
		}, server.TxReadCommitted, server.OptNoRetry())
		server.PgReset()
		recovered := server.HandleError(func() { GetTasks(ctx, id) })
		cause, _ := recovered.(error)
		var pgErr *pgconn.PgError
		var attempts int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT last_value FROM task_read_attempt`).Scan(&attempts))
		}, server.OptNoRetry())
		if !errors.As(cause, &pgErr) || pgErr.Code != "40001" || attempts != 1 {
			t.Fatal("failed exact-ID read was hidden or automatically replayed", recovered, attempts)
		}
	})
}

func TestTaskClaimPostCommitReadRetiresDeletedAndKeepsWake(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		deletedScope, liveScope := server.NewId(), server.NewId()
		deleted := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: deletedScope}, owner,
			runOnceGenerationKey(deletedScope), RunAt(server.NowUtc().Add(-2*time.Hour)))
		live := ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: liveScope, Cursor: 7}, owner,
			runOnceGenerationKey(liveScope), RunAt(server.NowUtc().Add(-time.Hour)))
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
		worker := runOnceGenerationWorker(ctx, target)
		defer worker.Close()
		worker.claimAfterCommit = func(*taskClaimGuard) {
			RemovePendingTask(ctx, deleted)
			ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: liveScope, Cursor: 99}, owner,
				runOnceGenerationKey(liveScope), RunAt(server.NowUtc().Add(-time.Hour)))
			server.OwnedTx(ctx, []server.PgOwnershipKey{RunOnceOwnershipKey(runOnceGenerationKey(liveScope))}, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET reschedule_error='committed after claim' WHERE task_id=$1`, live))
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		claimed, guard, err := worker.takeTasks(2)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard == nil || len(claimed) != 1 || claimed[live] == nil || guard.taskIds[deleted] {
			t.Fatal("post-commit refresh retained a deleted task owner", len(claimed), err)
		}
		queued := claimed[live]
		durable := GetTasks(ctx, live)[live]
		if queued.RescheduleError != "committed after claim" || queued.RunOnceGeneration != 0 ||
			queued.ClaimGeneration != 1 || durable.RunOnceGeneration != 1 {
			t.Fatal("claim borrowed a successor wake or missed committed metadata", queued, durable)
		}
		probe := server.RaisePgResult(server.AcquireMaintenanceDbConn(ctx))
		defer probe.Release()
		var retired bool
		server.Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, taskAdvisoryLockKey(deleted)).Scan(&retired))
		if retired {
			server.RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock($1)`, taskAdvisoryLockKey(deleted)))
		}
		if !retired {
			t.Fatal("deleted post-commit task retained its session guard")
		}
		probe.Release()
		result := worker.executeTask(ctx, queued, target)
		if result.err != nil {
			t.Fatal("freshly read live task failed", result.err)
		}
		worker.finalizeTask(result)
		pending := runOnceGenerationPending(ctx, []server.Id{liveScope})
		if len(pending) != 1 || pending[live] != nil || len(GetFinishedTasks(ctx, live)) != 1 {
			t.Fatal("post-commit wake did not retain its distinct successor")
		}
	})
}
