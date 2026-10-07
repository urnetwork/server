// Real claim boundaries expose speculative locking and work on dense queues.
// The fixture is synthetic; row visits and held locks provide causal checks
// without depending on wall-clock performance or goroutine scheduling.
package task

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Stop the first evaluator exactly after its initial row fetch. The peer must
// finish the next due task before that first claim can advance or commit.
func TestTaskClaimLeavesUnneededCandidatesForPeer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		first := NewTaskWorkerWithDefaults(ctx)
		second := NewTaskWorkerWithDefaults(ctx)
		defer first.Close()
		defer second.Close()
		first.AddTargets(NewTaskTarget(claimProfileAllowed))
		second.AddTargets(NewTaskTarget(claimProfileAllowed))
		firstId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-2*time.Hour)))
		secondId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession, RunAt(server.NowUtc().Add(-time.Hour)))
		peerCompleted := false
		first.claimCandidatesReady = func() {
			finished, retried, postRetried, err := second.EvalTasks(1)
			if err != nil || len(finished) != 1 || finished[0] != secondId || len(retried)+len(postRetried) != 0 {
				t.Fatalf("speculative fallback locks hid the next task from its peer: finished=%v retries=%v/%v error=%v", finished, retried, postRetried, err)
			}
			peerCompleted = true
		}
		finished, retried, postRetried, err := first.EvalTasks(1)
		if err != nil || !peerCompleted || len(finished) != 1 || finished[0] != firstId || len(retried)+len(postRetried) != 0 {
			t.Fatalf("ordered first task failed after its peer progressed: finished=%v retries=%v/%v peer=%t error=%v", finished, retried, postRetried, peerCompleted, err)
		}
		if len(GetTasks(ctx, firstId, secondId)) != 0 || len(GetFinishedTasks(ctx, firstId, secondId)) != 2 {
			t.Fatal("interleaved claims did not finalize each original task exactly once")
		}
	})
}

// A refusal resumes the same ordered scan without consuming the live owner's
// advisory lock or overlooking the last candidate of the existing window.
func TestTaskClaimFetchesThroughAdvisoryRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(NewTaskTarget(claimProfileAllowed))
		owner, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Release()
		defer func() { server.RaisePgResult(owner.Exec(ctx, `SELECT pg_advisory_unlock_all()`)) }()
		var expectedId server.Id
		for index := range 65 {
			taskId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession,
				RunAt(server.NowUtc().Add(-2*time.Hour+time.Duration(index)*time.Second)))
			if index < 64 {
				server.RaisePgResult(owner.Exec(ctx, `SELECT pg_advisory_lock($1)`, taskAdvisoryLockKey(taskId)))
			} else {
				expectedId = taskId
			}
		}
		claimed, guard, err := worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard == nil || len(claimed) != 1 || claimed[expectedId] == nil {
			t.Fatalf("cursor lost ordered advisory fallback: claimed=%d error=%v", len(claimed), err)
		}
	})
}

// Exhausting the existing fallback window neither extends it nor leaks a
// cursor/session; a following claim can use the same worker after release.
func TestTaskClaimFetchPreservesFallbackWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		worker := NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(NewTaskTarget(claimProfileAllowed))
		owner, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Release()
		defer func() { server.RaisePgResult(owner.Exec(ctx, `SELECT pg_advisory_unlock_all()`)) }()
		var firstId server.Id
		for index := range 66 {
			taskId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, clientSession,
				RunAt(server.NowUtc().Add(-2*time.Hour+time.Duration(index)*time.Second)))
			if index == 0 {
				firstId = taskId
			}
			if index < 65 {
				server.RaisePgResult(owner.Exec(ctx, `SELECT pg_advisory_lock($1)`, taskAdvisoryLockKey(taskId)))
			}
		}
		claimed, guard, err := worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard != nil || len(claimed) != 0 {
			t.Fatalf("claim crossed the existing fallback window: claimed=%d error=%v", len(claimed), err)
		}
		server.RaisePgResult(owner.Exec(ctx, `SELECT pg_advisory_unlock_all()`))
		claimed, guard, err = worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard == nil || len(claimed) != 1 || claimed[firstId] == nil {
			t.Fatalf("empty claim leaked its cursor or changed queue order: claimed=%d error=%v", len(claimed), err)
		}
	})
}

// Plan nodes supply actual work for the former eager read and verify that the
// cursor can stream the same indexed order without a blocking sort.
type taskClaimQueryTestPlan struct {
	NodeType     string                   `json:"Node Type"`
	RelationName string                   `json:"Relation Name"`
	IndexName    string                   `json:"Index Name"`
	ActualRows   float64                  `json:"Actual Rows"`
	ActualLoops  float64                  `json:"Actual Loops"`
	FilteredRows float64                  `json:"Rows Removed by Filter"`
	SharedHits   int64                    `json:"Shared Hit Blocks"`
	SharedReads  int64                    `json:"Shared Read Blocks"`
	Plans        []taskClaimQueryTestPlan `json:"Plans"`
}

// A dense due backlog contains one eligible target every 256 rows, alongside
// future and leased work. Actual takeTasks visits only enough of that backlog
// to fill its batch; its previous eager SELECT visits all 16,384 due rows.
func TestTaskClaimDenseSaturatedQueueWork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		limited := NewTaskTarget(claimProfileAllowed)
		ordinary := NewTaskTarget(claimProfileExcluded)
		settings := DefaultTaskWorkerSettings()
		settings.TargetClaimLimits = map[string]int{limited.TargetFunctionName(): 1}
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(limited, ordinary)
		reservation, admitted := worker.reserveTaskClaim(limited.TargetFunctionName())
		if !admitted || reservation == nil {
			t.Fatal("could not establish local target saturation")
		}
		defer reservation.release()

		const dueCount = 16 * 1024
		const futureCount = 4 * 1024
		const leasedCount = 4 * 1024
		const eligibleStride = 256
		const batchSize = 4
		past := server.NowUtc().Add(-48 * time.Hour).Truncate(time.Second)
		future := server.NowUtc().Add(24 * time.Hour).Truncate(time.Second)
		legacyName := strings.Replace(limited.TargetFunctionName(), "/server/", "/server/v37/", 1)
		fixtureRows := make([][]any, 0, dueCount+futureCount+leasedCount)
		expectedIds := []server.Id{}
		for index := range dueCount + futureCount + leasedCount {
			var taskId server.Id
			binary.BigEndian.PutUint64(taskId[8:], uint64(index+1))
			storedName := legacyName
			runAt := past.Add(time.Duration(index) * time.Second)
			releaseTime := past
			if index < dueCount && (index+1)%eligibleStride == 0 {
				storedName = ordinary.TargetFunctionName()
				if len(expectedIds) < batchSize {
					expectedIds = append(expectedIds, taskId)
				}
			} else if dueCount <= index && index < dueCount+futureCount {
				storedName = ordinary.TargetFunctionName()
				runAt = future
			} else if dueCount+futureCount <= index {
				storedName = ordinary.TargetFunctionName()
				releaseTime = future
			}
			fixtureRows = append(fixtureRows, []any{
				taskId, storedName, `{}`, runAt, DefaultPriority,
				int(DefaultMaxTime / time.Second), past, releaseTime,
			})
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := tx.CopyFrom(ctx, pgx.Identifier{"pending_task"},
				[]string{"task_id", "function_name", "args_json", "run_at", "run_priority", "run_max_time_seconds", "claim_time", "release_time"},
				pgx.CopyFromRows(fixtureRows))
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE pending_task`))
		})
		before := GetTasks(ctx, expectedIds...)
		query, queryArgs := worker.claimCandidatesQuery(server.NowUtc().Unix()/BlockSizeSeconds, batchSize+64)
		var eagerVisits float64
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			tx, err := conn.Begin(ctx)
			server.Raise(err)
			defer tx.Rollback(ctx)
			var raw []byte
			server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) `+query, queryArgs...).Scan(&raw))
			var plans []struct {
				Plan taskClaimQueryTestPlan `json:"Plan"`
			}
			server.Raise(json.Unmarshal(raw, &plans))
			if len(plans) != 1 {
				t.Fatal("missing eager control plan")
			}
			var walk func(taskClaimQueryTestPlan)
			walk = func(plan taskClaimQueryTestPlan) {
				if plan.RelationName == "pending_task" {
					eagerVisits += (plan.ActualRows + plan.FilteredRows) * plan.ActualLoops
				}
				for _, child := range plan.Plans {
					walk(child)
				}
			}
			walk(plans[0].Plan)
			t.Logf("eager candidate control: rows_visited=%.0f buffers=%d", eagerVisits, plans[0].Plan.SharedHits+plans[0].Plan.SharedReads)
			if eagerVisits < dueCount {
				t.Fatalf("fixture did not reproduce eager full-due-backlog work: %.0f", eagerVisits)
			}
		})

		// The actual cursor plan must stream under both cache modes. No planner
		// switches disable scans or sorts; these are the ordinary fixture stats.
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			server.MaintenanceDb(ctx, func(conn server.PgConn) {
				tx, err := conn.Begin(ctx)
				server.Raise(err)
				defer tx.Rollback(ctx)
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = `+mode))
				var raw []byte
				server.Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) DECLARE pending_task_claim_candidates NO SCROLL CURSOR FOR `+query, queryArgs...).Scan(&raw))
				var plans []struct {
					Plan taskClaimQueryTestPlan `json:"Plan"`
				}
				server.Raise(json.Unmarshal(raw, &plans))
				if len(plans) != 1 {
					t.Fatalf("missing %s cursor plan", mode)
				}
				streaming := false
				var walk func(taskClaimQueryTestPlan)
				walk = func(plan taskClaimQueryTestPlan) {
					if plan.NodeType == "Sort" || plan.NodeType == "Incremental Sort" || plan.NodeType == "Materialize" {
						t.Fatalf("%s cursor materialized unneeded fallback work: %s", mode, raw)
					}
					if plan.IndexName == "pending_task_poll_order" {
						streaming = true
					}
					for _, child := range plan.Plans {
						walk(child)
					}
				}
				walk(plans[0].Plan)
				if !streaming {
					t.Fatalf("%s cursor lost the ordered index: %s", mode, raw)
				}
			})
		}

		// These counters include backend-local reads since the last stats flush,
		// even across transactions. Bracket the real claim in its own transaction
		// instead of assuming a pooled backend starts at zero.
		readVisits := func(conn *pgx.Conn) (int32, int64, error) {
			var backendPid int32
			var visits int64
			err := conn.QueryRow(ctx, `
				SELECT pg_backend_pid(), seq_tup_read + idx_tup_fetch
				FROM pg_stat_xact_user_tables
				WHERE relid = 'pending_task'::regclass
			`).Scan(&backendPid, &visits)
			return backendPid, visits, err
		}
		rollback := errors.New("synthetic dense-queue claim rollback")
		for attempt := range 7 {
			var beforePid, afterPid int32
			var beforeVisits, afterVisits int64
			worker.claimBeforeQuery = func(tx server.PgTx) error {
				// Force prior pending reads before the start sample. This makes
				// absolute-counter misuse fail without depending on pool reuse or
				// the statistics flusher's timing. The read takes no row locks.
				const primingRows = 2 * eligibleStride * batchSize
				var argumentBytes int64
				if err := tx.QueryRow(ctx, `
					SELECT sum(length(args_json))
					FROM (SELECT args_json FROM pending_task LIMIT $1) AS primed_tasks
				`, primingRows).Scan(&argumentBytes); err != nil {
					return err
				}
				if argumentBytes != 2*primingRows {
					t.Fatal("counter priming did not read the expected synthetic arguments")
				}
				var err error
				beforePid, beforeVisits, err = readVisits(tx.Conn())
				if err == nil && beforeVisits < primingRows {
					t.Fatalf("counter priming did not establish pending reads: %d", beforeVisits)
				}
				return err
			}
			worker.claimBeforeCommit = func(guard *taskClaimGuard) error {
				var err error
				afterPid, afterVisits, err = readVisits(guard.conn.Conn())
				if err != nil {
					return err
				}
				return rollback
			}
			claimed, guard, err := worker.takeTasks(batchSize)
			if guard != nil {
				guard.release()
			}
			if !errors.Is(err, rollback) || guard != nil || len(claimed) != 0 {
				t.Fatalf("dense claim did not reach the real rollback boundary: error=%v", err)
			}
			if beforePid != afterPid || afterVisits < beforeVisits {
				t.Fatalf("claim counter endpoints lost backend/transaction continuity: before=%d/%d after=%d/%d", beforePid, beforeVisits, afterPid, afterVisits)
			}
			cursorVisits := afterVisits - beforeVisits
			if cursorVisits < eligibleStride*batchSize || eligibleStride*batchSize+2*batchSize < cursorVisits {
				t.Fatalf("claim attempt %d scanned unneeded fallback rows: visits=%d before=%d after=%d eager=%.0f", attempt, cursorVisits, beforeVisits, afterVisits, eagerVisits)
			}
			t.Logf("claim attempt %d: rows_visited=%d before=%d after=%d eager_rows=%.0f", attempt, cursorVisits, beforeVisits, afterVisits, eagerVisits)
		}
		if after := GetTasks(ctx, expectedIds...); !reflect.DeepEqual(before, after) {
			t.Fatal("rolled-back measurements changed durable task claims")
		}
		worker.claimBeforeQuery = nil
		worker.claimBeforeCommit = nil
		claimed, guard, err := worker.takeTasks(batchSize)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard == nil || len(claimed) != batchSize {
			t.Fatalf("dense claim failed after measurements: count=%d error=%v", len(claimed), err)
		}
		for _, taskId := range expectedIds {
			if claimed[taskId] == nil {
				t.Fatal("dense claim changed due ordering or admitted future/leased/saturated work")
			}
		}
	})
}
