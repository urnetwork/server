// A refresh's database work must stay bounded on the server and must never run
// beside another refresh's count. An exclusive table lock stalls a count inside
// PostgreSQL, standing in for a whole-table scan whose client stopped waiting
// while the server kept reading. pg_stat_activity shows each stalled count.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
)

// Holds an exclusive lock on pending_task on its own session, so any count
// waits inside the server until release, which is idempotent.
func holdTaskQueueTable(ctx context.Context, t testing.TB) (release func()) {
	t.Helper()
	conn, err := server.AcquireMaintenanceDbConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE pending_task IN ACCESS EXCLUSIVE MODE`); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		conn.Release()
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_ = tx.Rollback(cleanupCtx)
			conn.Release()
		})
	}
}

// Refresh statements waiting on a lock inside this test's database. A failed
// observation reports -1, which satisfies no wait.
func countStalledTaskQueueCounts(ctx context.Context) (count int) {
	count = -1
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
				SELECT count(*)::int FROM pg_stat_activity
				WHERE datname = current_database() AND pid <> pg_backend_pid()
				  AND state = 'active' AND wait_event_type = 'Lock'
				  AND query LIKE '%taskworker-queue-metrics%'
			`).Scan(&count))
		})
	})
	return
}

// Waits for an observed state, not for elapsed time; false when ctx ends first.
func waitForStalledTaskQueueCounts(ctx context.Context, count int) bool {
	for {
		if count <= countStalledTaskQueueCounts(ctx) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// The server itself ended the statement at its statement timeout.
func isTaskQueueStatementTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.QueryCanceled && strings.Contains(pgErr.Message, "statement timeout")
}

// The second refresh either skips while the first still holds the database
// key, or, after a stall longer than the statement timeout ended the first,
// is itself ended by the server. Two counts stalled at once are the overlap.
func requireNoOverlappingTaskQueueCount(ctx context.Context, t testing.TB, secondDone <-chan error) {
	t.Helper()
	watchCtx, stopWatch := context.WithCancel(ctx)
	overlapped := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		if waitForStalledTaskQueueCounts(watchCtx, 2) {
			close(overlapped)
		}
	}()
	defer func() {
		stopWatch()
		<-watched
	}()
	select {
	case err := <-secondDone:
		if !errors.Is(err, errTaskQueueMetricsBusy) && !isTaskQueueStatementTimeout(err) {
			t.Fatal("the second refresh was neither skipped nor ended by the server", err)
		}
	case <-overlapped:
		t.Fatal("a second queue count started while the first was still running on the server")
	case <-ctx.Done():
		t.Fatal("the second refresh neither returned nor overlapped the first")
	}
}

// Two runners racing for one interval execute the count once: while one count
// runs on the server, the other refresh does no database scan.
func TestTaskQueueMetricsSlowCountCannotOverlapItself(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		release := holdTaskQueueTable(ctx, t)
		var refreshes sync.WaitGroup
		defer func() {
			release()
			refreshes.Wait()
		}()
		now := server.NowUtc()
		firstDone, secondDone := make(chan error, 1), make(chan error, 1)
		refreshes.Go(func() {
			_, err := loadTaskQueueMetricsSnapshot(ctx, now)
			firstDone <- err
		})
		if !waitForStalledTaskQueueCounts(ctx, 1) {
			t.Fatal("the first count never reached the server")
		}
		refreshes.Go(func() {
			_, err := loadTaskQueueMetricsSnapshot(ctx, now)
			secondDone <- err
		})
		requireNoOverlappingTaskQueueCount(ctx, t, secondDone)
		release()
		if err := <-firstDone; err != nil && !isTaskQueueStatementTimeout(err) {
			t.Fatal("the first count failed for a reason other than its server bound", err)
		}
	})
}

// The Redis admission expires with its interval while a slow count still runs
// on the server, as it did on Main. The next owner must not scan beside it.
func TestTaskQueueMetricsExpiredAdmissionCannotStartSecondCount(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		release := holdTaskQueueTable(ctx, t)
		var refreshes sync.WaitGroup
		defer func() {
			release()
			refreshes.Wait()
		}()
		newCache := func() taskQueueMetricsCache {
			return taskQueueMetricsCache{
				store: redisTaskQueueMetricsStore{}, now: server.NowUtc,
				newToken: func() string { return server.NewId().String() },
				load:     loadTaskQueueMetricsSnapshot,
			}
		}
		firstDone, secondDone := make(chan error, 1), make(chan error, 1)
		refreshes.Go(func() {
			_, err := newCache().get(ctx)
			firstDone <- err
		})
		if !waitForStalledTaskQueueCounts(ctx, 1) {
			t.Fatal("the first owner's count never reached the server")
		}
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.PExpireAt(ctx, taskMetricsOwnerKey, time.Unix(1, 0)).Err())
		})
		refreshes.Go(func() {
			sample, err := newCache().get(ctx)
			if err == nil && sample == nil {
				err = errors.New("the next owner was not admitted after the expiry")
			}
			secondDone <- err
		})
		requireNoOverlappingTaskQueueCount(ctx, t, secondDone)
		release()
		<-firstDone
	})
}

// With no client deadline at all, the server ends a stalled count at its own
// statement timeout, and the database key ends with that transaction.
func TestTaskQueueMetricsServerEndsStalledCount(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task`))
		})
		release := holdTaskQueueTable(ctx, t)
		defer release()
		now := server.NowUtc()
		if _, err := loadTaskQueueMetricsSnapshot(ctx, now); !isTaskQueueStatementTimeout(err) {
			t.Fatal("a stalled count was not ended by the server's own bound", err)
		}
		if stalled := countStalledTaskQueueCounts(ctx); stalled != 0 {
			t.Fatal("the stalled count kept running on the server after its refresh returned", stalled)
		}
		release()
		snapshot, err := loadTaskQueueMetricsSnapshot(ctx, now)
		if err != nil || snapshot != (taskQueueMetricsSnapshot{}) {
			t.Fatal("the database key outlived the statement the server ended", err)
		}
	})
}

// Only a saturated side asks the planner, its estimate never drops below the
// rows already read, and estimated states carry their marks.
func TestTaskQueueMetricsBoundEstimatesOnlySaturatedSides(t *testing.T) {
	planned := map[string]int64{
		taskQueueMetricsDuePredicate:                                      100,
		taskQueueMetricsDuePredicate + taskQueueMetricsErrorPredicate:     7,
		taskQueueMetricsWaitingPredicate:                                  2,
		taskQueueMetricsWaitingPredicate + taskQueueMetricsErrorPredicate: 50,
	}
	requested := []string{}
	estimate := func(predicate string) int64 {
		requested = append(requested, predicate)
		return planned[predicate]
	}
	exact := taskQueueMetricsBound{dueRows: 3, dueErrors: 1, oldestOverdueSecond: 9, waitingRows: 3, waitingClaimed: 2, waitingErrors: 1}
	if got := exact.snapshot(3, estimate); len(requested) != 0 ||
		got != (taskQueueMetricsSnapshot{Total: 6, Available: 3, Claimed: 2, RescheduleError: 2, OldestOverdueSecond: 9}) {
		t.Fatal("a side within its bound was estimated", got, requested)
	}

	due := exact
	due.dueRows = 4
	want := taskQueueMetricsSnapshot{
		Total: 103, Available: 100, Claimed: 2, RescheduleError: 8, OldestOverdueSecond: 9,
		TotalEstimated: true, AvailableEstimated: true, RescheduleErrorEstimated: true,
	}
	if got := due.snapshot(3, estimate); got != want || !slices.Equal(requested, []string{
		taskQueueMetricsDuePredicate, taskQueueMetricsDuePredicate + taskQueueMetricsErrorPredicate,
	}) {
		t.Fatal("a saturated due side did not report its planner estimate", got, requested)
	}

	// The waiting estimate (2) is below the 4 rows read, and its error
	// estimate (50) above them: both are held to what the read proved.
	requested = nil
	waiting := exact
	waiting.waitingRows = 4
	want = taskQueueMetricsSnapshot{
		Total: 7, Available: 3, Claimed: 2, RescheduleError: 5, OldestOverdueSecond: 9,
		TotalEstimated: true, ClaimedEstimated: true, RescheduleErrorEstimated: true,
	}
	if got := waiting.snapshot(3, estimate); got != want || !slices.Equal(requested, []string{
		taskQueueMetricsWaitingPredicate, taskQueueMetricsWaitingPredicate + taskQueueMetricsErrorPredicate,
	}) {
		t.Fatal("a saturated waiting side was not held to the rows it read", got, requested)
	}

	future := taskQueueMetricsBound{oldestOverdueSecond: -1}
	if got := future.snapshot(3, estimate); got != (taskQueueMetricsSnapshot{}) {
		t.Fatal("an empty due side reported an overdue age", got)
	}
}

// Inserts one synthetic row per entry.
func insertTaskQueueRows(ctx context.Context, rows []taskQueueTestRow) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task`))
		for _, row := range rows {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO pending_task (
				 task_id,function_name,args_json,run_at,run_priority,run_max_time_seconds,
				 claim_time,release_time,reschedule_error
				) VALUES($1,'synthetic.queue.metrics','{}',$2,0,30,$3,$4,$5)`,
				server.NewId(), row.runAt, time.Time{}, row.releaseTime, row.failure))
		}
	})
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE pending_task`))
	}, server.OptReadWrite())
}

// A synthetic pending task.
type taskQueueTestRow struct {
	runAt       time.Time
	releaseTime time.Time
	failure     any
}

// The planner's row estimate for the same predicate the refresh asks about.
func plannedTaskQueueRows(ctx context.Context, t testing.TB, predicate string, availableBlock int64) int64 {
	t.Helper()
	var planJson []byte
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `EXPLAIN (FORMAT JSON) SELECT 1 FROM pending_task WHERE `+predicate, availableBlock).Scan(&planJson))
	})
	plans := []struct {
		Plan struct {
			Rows float64 `json:"Plan Rows"`
		} `json:"Plan"`
	}{}
	if err := json.Unmarshal(planJson, &plans); err != nil || len(plans) != 1 {
		t.Fatal("no plan estimate", err)
	}
	return int64(plans[0].Plan.Rows + 0.5)
}

// A side holding more rows than the bound reports the planner's estimate,
// never below the rows read; the side within the bound stays exact.
func TestTaskQueueMetricsBoundedReadEstimatesPastBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		now := server.NowUtc().Truncate(time.Second)
		rows := []taskQueueTestRow{}
		// The two newest due rows failed, so the first four in poll order did not.
		for age := 6; 1 <= age; age-- {
			row := taskQueueTestRow{runAt: now.Add(-time.Duration(age*10) * time.Second)}
			if age <= 2 {
				row.failure = "synthetic due failure"
			}
			rows = append(rows, row)
		}
		rows = append(rows,
			taskQueueTestRow{runAt: now.Add(-time.Hour), releaseTime: now.Add(30 * time.Second), failure: "synthetic claimed failure"},
			taskQueueTestRow{runAt: now.Add(time.Hour)},
		)
		insertTaskQueueRows(ctx, rows)

		exact, err := taskQueueMetricsQuery{statementTimeout: taskMetricsStatementTimeout, boundRows: 6}.load(ctx, now)
		if err != nil || exact != (taskQueueMetricsSnapshot{Total: 8, Available: 6, Claimed: 1, RescheduleError: 3, OldestOverdueSecond: 60}) {
			t.Fatal("sides within the bound were not counted exactly", exact, err)
		}

		availableBlock := now.Unix() / BlockSizeSeconds
		dueRows := max(4, plannedTaskQueueRows(ctx, t, taskQueueMetricsDuePredicate, availableBlock))
		dueErrors := min(dueRows, plannedTaskQueueRows(ctx, t, taskQueueMetricsDuePredicate+taskQueueMetricsErrorPredicate, availableBlock))
		want := taskQueueMetricsSnapshot{
			Total: dueRows + 2, Available: dueRows, Claimed: 1, RescheduleError: dueErrors + 1, OldestOverdueSecond: 60,
			TotalEstimated: true, AvailableEstimated: true, RescheduleErrorEstimated: true,
		}
		bounded, err := taskQueueMetricsQuery{statementTimeout: taskMetricsStatementTimeout, boundRows: 3}.load(ctx, now)
		if err != nil || bounded != want || bounded.Available < 4 || 8 < bounded.Available {
			t.Fatal("a saturated due side did not report the planner estimate", bounded, want, err)
		}
	})
}

// A plan node reading pending_task, with what EXPLAIN ANALYZE measured.
type taskQueuePlanNode struct {
	NodeType     string              `json:"Node Type"`
	RelationName string              `json:"Relation Name"`
	IndexName    string              `json:"Index Name"`
	ActualRows   float64             `json:"Actual Rows"`
	ActualLoops  float64             `json:"Actual Loops"`
	Plans        []taskQueuePlanNode `json:"Plans"`
}

// However many rows the queue holds, each side reads at most boundRows+1 rows
// in poll order from pending_task_poll_order, with no sequential scan.
func TestTaskQueueMetricsBoundedReadStopsAtBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		now := server.NowUtc().Truncate(time.Second)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO pending_task (
				 task_id,function_name,args_json,run_at,run_priority,run_max_time_seconds,claim_time,release_time
				) SELECT gen_random_uuid(),'synthetic.queue.metrics','{}',$1::timestamp + make_interval(secs => g),0,30,$2,$2
				FROM generate_series(-20000, 19999) AS g`, now, time.Time{}))
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE pending_task`))
		}, server.OptReadWrite())
		query := taskQueueMetricsQuery{statementTimeout: taskMetricsStatementTimeout, boundRows: 10}
		var planJson []byte
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('max_parallel_workers_per_gather', '0', true), set_config('jit', 'off', true)`))
			server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+query.boundedSql(), now, now.Unix()/BlockSizeSeconds).Scan(&planJson))
		}, server.TxReadCommitted, pgx.ReadOnly, server.OptNoRetry())
		plans := []struct {
			Plan taskQueuePlanNode `json:"Plan"`
		}{}
		if err := json.Unmarshal(planJson, &plans); err != nil || len(plans) != 1 {
			t.Fatal("no measured plan", err)
		}
		scans := 0
		var visit func(node taskQueuePlanNode)
		visit = func(node taskQueuePlanNode) {
			if node.RelationName == "pending_task" {
				scans++
				if node.NodeType != "Index Scan" && node.NodeType != "Index Only Scan" || node.IndexName != "pending_task_poll_order" ||
					11 < node.ActualRows*max(1, node.ActualLoops) {
					t.Errorf("a side read %v rows by %s %s, past its bound of 11 in poll order", node.ActualRows*max(1, node.ActualLoops), node.NodeType, node.IndexName)
				}
			}
			for _, child := range node.Plans {
				visit(child)
			}
		}
		visit(plans[0].Plan)
		if scans != 2 {
			t.Fatal("expected one bounded read per side of the availability boundary", scans)
		}
		snapshot, err := query.load(ctx, now)
		if err != nil || !snapshot.AvailableEstimated || !snapshot.ClaimedEstimated || snapshot.Available < 11 || snapshot.Total < 22 {
			t.Fatal("a queue past both bounds was not reported as estimated", snapshot, err)
		}
	})
}
