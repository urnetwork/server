package task

import (
	"context"
	"errors"
	"fmt"
	"math"
	mathrand "math/rand"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	// "github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type Work1Args struct {
}

type Work1Result struct {
}

func Work1(
	work1 *Work1Args,
	clientSession *session.ClientSession,
) (*Work1Result, error) {
	if 0 == mathrand.Intn(100) {
		select {
		case <-time.After(ReleaseTimeout / 2):
		case <-clientSession.Ctx.Done():
			return nil, errors.New("Timeout.")
		}
	}
	if 0 == mathrand.Intn(3) {
		return nil, errors.New("Error.")
	}
	return &Work1Result{}, nil
}

func Work1Post(
	work1 *Work1Args,
	work1Result *Work1Result,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if 0 == mathrand.Intn(3) {
		return errors.New("Post error.")
	}
	return nil
}

func TestTask(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		RescheduleTimeout = 1 * time.Second
		ReleaseTimeout = 1 * time.Second
		// cap the error backoff at the base so the ~1/3 random Work1 failures
		// retry fast; the stress test measures throughput under churn, not the
		// backoff (which TestTaskRescheduleErrorBackoff covers)
		RescheduleBackoffMaxTimeout = 1 * time.Second

		ctx := context.Background()

		n := 10
		m := 10000
		k := 100

		targetRunCount := 2*m + k

		stateLock := sync.Mutex{}
		runCounts := map[server.Id]int{}
		postRescheduledRunCounts := map[server.Id]int{}
		workerCount := 0

		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		for i := 0; i < m; i += 1 {
			ScheduleTask(
				Work1,
				&Work1Args{},
				clientSession,
				RunOnce("unique", i%k),
			)
		}

		for i := 0; i < n; i += 1 {
			taskWorker := NewTaskWorkerWithDefaults(ctx)
			taskWorker.AddTargets(NewTaskTargetWithPost(Work1, Work1Post))

			go func() {
				stateLock.Lock()
				workerCount += 1
				stateLock.Unlock()
				defer func() {
					stateLock.Lock()
					workerCount -= 1
					stateLock.Unlock()
				}()

				for {
					select {
					case <-clientSession.Ctx.Done():
						return
					default:
					}

					finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds, err := taskWorker.EvalTasks(10)
					if err != nil {
						panic(err)
					}
					stateLock.Lock()
					for _, taskId := range finishedTaskIds {
						runCounts[taskId] += 1
					}
					for _, taskId := range postRescheduledTaskIds {
						postRescheduledRunCounts[taskId] += 1
					}
					stateLock.Unlock()

					if 0 == len(finishedTaskIds)+len(rescheduledTaskIds)+len(postRescheduledTaskIds) {
						select {
						case <-clientSession.Ctx.Done():
							return
						case <-time.After(1 * time.Second):
						}
					}
				}
			}()
		}

		for i := 0; i < m; i += 1 {
			ScheduleTask(
				Work1,
				&Work1Args{},
				clientSession,
			)
		}
		for i := 0; i < m; i += 1 {
			ScheduleTask(
				Work1,
				&Work1Args{},
				clientSession,
				RunOnce("task", i),
			)
		}

	WaitWork:
		for {
			stateLock.Lock()
			netRunCount := 0
			for _, runCount := range runCounts {
				netRunCount += runCount
			}
			pendingTaskIds := ListPendingTasks(ctx)
			rescheduledTaskIds := ListRescheduledTasks(ctx)
			claimedTaskIds := ListClaimedTasks(ctx)
			finishedTaskIds := ListFinishedTasks(ctx)
			fmt.Printf("Tasks pending=%d (rescheduled=%d, claimed=%d, finished=%d)\n", len(pendingTaskIds), len(rescheduledTaskIds), len(claimedTaskIds), len(finishedTaskIds))
			finished := (len(pendingTaskIds) == 0)
			stateLock.Unlock()

			if finished {
				clientSession.Cancel()
				break
			}

			select {
			case <-clientSession.Ctx.Done():
				break WaitWork
			case <-time.After(1 * time.Second):
			}
		}

	WaitDrain:
		for {
			stateLock.Lock()
			finished := (workerCount == 0)
			stateLock.Unlock()

			if finished {
				break
			}

			select {
			case <-ctx.Done():
				break WaitDrain
			case <-time.After(1 * time.Second):
			}
		}

		netRunCount := 0
		for _, runCount := range runCounts {
			netRunCount += runCount
		}
		connect.AssertEqual(t, netRunCount, targetRunCount)

		netTaskCount := 0
		for _, runCount := range runCounts {
			netTaskCount += runCount
		}
		for _, runCount := range postRescheduledRunCounts {
			netTaskCount += runCount
		}

		removedCount := RemoveFinishedTasks(ctx, server.NowUtc(), server.NowUtc().Add(-7*24*time.Hour))
		connect.AssertEqual(t, int(removedCount), netTaskCount)
		connect.AssertEqual(t, 0, len(ListFinishedTasks(ctx)))
	})
}

type AlwaysFailArgs struct {
}

type AlwaysFailResult struct {
}

func AlwaysFail(
	alwaysFail *AlwaysFailArgs,
	clientSession *session.ClientSession,
) (*AlwaysFailResult, error) {
	return nil, errors.New("always fails")
}

// A task that errors repeatedly must back off exponentially:
// run_at - now ~= jitter[0, RescheduleTimeout) + RescheduleTimeout * 2^errorCount,
// capped at RescheduleBackoffMaxTimeout. Without the backoff a wedged task
// (e.g. an external 429 rate limit) retried every ~RescheduleTimeout forever;
// in prod 8k such payment tasks churned pending_task to ~94% dead tuples and
// made the poll query 39% of all db exec time.
func TestTaskRescheduleErrorBackoff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		RescheduleTimeout = 2 * time.Second
		ReleaseTimeout = 30 * time.Second
		// pin explicitly: TestTask lowers this package var for throughput, and
		// test order within one binary would otherwise leak it here
		RescheduleBackoffMaxTimeout = 1 * time.Hour

		ctx := context.Background()

		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		ScheduleTask(
			AlwaysFail,
			&AlwaysFailArgs{},
			clientSession,
			RunOnce("always_fail_backoff"),
		)

		taskWorker := NewTaskWorkerWithDefaults(ctx)
		taskWorker.AddTargets(NewTaskTarget(AlwaysFail))

		var taskId server.Id
		readState := func() (errorCount int, runAt time.Time) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT task_id, reschedule_error_count, run_at FROM pending_task LIMIT 1`,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&taskId, &errorCount, &runAt))
					}
				})
			})
			return
		}

		errorCount, _ := readState()
		connect.AssertEqual(t, errorCount, 0)

		makeDue := func() {
			// well past now: available_block is 1 + epoch(max(run_at,
			// release_time)) with numeric->bigint rounding, so a bare now-1s
			// lands exactly on the poll boundary and claims only when the
			// worker's clock tick falls late (flaky)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE pending_task SET run_at = $2, release_time = $2 WHERE task_id = $1`,
					taskId,
					server.NowUtc().Add(-5*time.Second),
				))
			})
		}

		base := float64(RescheduleTimeout / time.Second)
		for round := 0; round < 6; round += 1 {
			makeDue()
			evalStart := server.NowUtc()
			finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds, err := taskWorker.EvalTasks(10)
			connect.AssertEqual(t, err, nil)
			if len(rescheduledTaskIds) != 1 {
				// diagnostics: dump the pending row + eval buckets
				server.Db(ctx, func(conn server.PgConn) {
					result, err := conn.Query(ctx, `SELECT task_id, function_name, available_block, run_at, release_time, reschedule_error_count, extract(epoch from now())::bigint AS now_epoch FROM pending_task`)
					server.WithPgResult(result, err, func() {
						for result.Next() {
							var tid server.Id
							var fn string
							var ab int64
							var ra, rt time.Time
							var ec int
							var ne int64
							server.Raise(result.Scan(&tid, &fn, &ab, &ra, &rt, &ec, &ne))
							t.Logf("DIAG pending: id=%s fn=%s available_block=%d now_epoch=%d run_at=%s release=%s count=%d", tid, fn, ab, ne, ra, rt, ec)
						}
					})
				})
				t.Logf("DIAG eval: finished=%v rescheduled=%v postRescheduled=%v", finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds)
			}
			connect.AssertEqual(t, len(rescheduledTaskIds), 1)

			errorCount, runAt := readState()
			connect.AssertEqual(t, errorCount, round+1)
			delay := runAt.Sub(evalStart)
			// jitter[0, RescheduleTimeout) + RescheduleTimeout * 2^round, with
			// slack for the eval runtime
			minDelay := time.Duration(base*math.Pow(2, float64(round))) * time.Second
			maxDelay := minDelay + RescheduleTimeout + 10*time.Second
			connect.AssertEqual(t, minDelay <= delay, true)
			connect.AssertEqual(t, delay <= maxDelay, true)
		}

		// a high error count converges to the cap instead of growing unbounded
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE pending_task SET reschedule_error_count = 30 WHERE task_id = $1`,
				taskId,
			))
		})
		makeDue()
		evalStart := server.NowUtc()
		_, rescheduledTaskIds, _, err := taskWorker.EvalTasks(10)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(rescheduledTaskIds), 1)
		errorCount, runAt := readState()
		connect.AssertEqual(t, errorCount, 31)
		delay := runAt.Sub(evalStart)
		connect.AssertEqual(t, RescheduleBackoffMaxTimeout <= delay, true)
		connect.AssertEqual(t, delay <= RescheduleBackoffMaxTimeout+RescheduleTimeout+10*time.Second, true)
	})
}
