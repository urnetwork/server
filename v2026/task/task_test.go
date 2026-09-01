package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	mathrand "math/rand"
	"sync"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/connect/v2026"

	// "github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
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

func TestErrorRescheduleDelayDispersesCappedWave(t *testing.T) {
	base := 2 * time.Second
	cap := time.Hour

	// Unsaturated retries retain the legacy nominal + [0, base) jitter.
	if got := errorRescheduleDelay(base, cap, 0, rescheduleBackoffMaxExponent, 0); got != 2*time.Second {
		t.Fatalf("first retry with zero jitter = %s, want 2s", got)
	}
	if got := errorRescheduleDelay(base, cap, 3, rescheduleBackoffMaxExponent, 0.5); got != 17*time.Second {
		t.Fatalf("unsaturated retry midpoint = %s, want 17s", got)
	}
	// A special low exponent (deploy drain/version skew) never accidentally
	// inherits the saturated proportional jitter from a high stored count.
	if got := errorRescheduleDelay(base, cap, 30, 0, 0.5); got != 3*time.Second {
		t.Fatalf("clamped retry = %s, want 3s", got)
	}

	// Synthetic 824-row outage cohort: deterministic quantiles cover every
	// minute from 30 through 89 after reaching the one-hour nominal cap, with
	// no minute retaining more than 14 rows. The old two-second jitter put all
	// 824 rows in one or two adjacent seconds forever.
	minuteCounts := map[int]int{}
	for i := 0; i < 824; i++ {
		unit := (float64(i) + 0.5) / 824
		delay := errorRescheduleDelay(base, cap, 30, rescheduleBackoffMaxExponent, unit)
		minuteCounts[int(delay/time.Minute)]++
	}
	if len(minuteCounts) != 60 {
		t.Fatalf("capped cohort covered %d minute buckets, want 60: %v", len(minuteCounts), minuteCounts)
	}
	for minute := 30; minute < 90; minute++ {
		if count := minuteCounts[minute]; count < 13 || 14 < count {
			t.Fatalf("minute %d has %d retries, want 13 or 14", minute, count)
		}
	}
	if midpoint := errorRescheduleDelay(base, cap, 30, rescheduleBackoffMaxExponent, 0.5); midpoint != cap+base/2 {
		t.Fatalf("capped retry midpoint = %s, want %s", midpoint, cap+base/2)
	}
}

// A task that errors repeatedly must back off exponentially:
// run_at - now ~= jitter[0, RescheduleTimeout) + RescheduleTimeout * 2^errorCount
// until the nominal cap. Saturated retries use proportional 30–90 minute
// jitter with a one-hour mean. Without the backoff a wedged task
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

		// A high error count converges to proportional jitter around the
		// nominal cap instead of growing unbounded or preserving one retry wave.
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
		minDelay := RescheduleBackoffMaxTimeout/2 + RescheduleTimeout/2
		maxDelay := 3*RescheduleBackoffMaxTimeout/2 + RescheduleTimeout/2 + 10*time.Second
		connect.AssertEqual(t, minDelay <= delay, true)
		connect.AssertEqual(t, delay <= maxDelay, true)
	})
}

// lease-test work: signals when it starts and blocks until released, so the
// test can inspect the claimed task's release_time while a keepalive beat
// fires mid-run.
type LeaseWorkArgs struct{}
type LeaseWorkResult struct{}

var leaseWorkStarted = make(chan struct{}, 1)
var leaseWorkRelease = make(chan struct{})

func LeaseWork(
	args *LeaseWorkArgs,
	clientSession *session.ClientSession,
) (*LeaseWorkResult, error) {
	select {
	case leaseWorkStarted <- struct{}{}:
	default:
	}
	select {
	case <-leaseWorkRelease:
	case <-clientSession.Ctx.Done():
		return nil, errors.New("cancelled")
	}
	return &LeaseWorkResult{}, nil
}

// A long task's lease is kept alive by heartbeats but is bounded independently
// of MaxTime. This preserves duplicate protection while ensuring a killed
// worker cannot strand a two-hour task for two hours.
func TestTaskLeaseIsBoundedAndExtendedByKeepalive(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		// small ReleaseTimeout so keepalive beats (every ReleaseTimeout/3)
		// fire quickly during the blocked run
		prevRelease := ReleaseTimeout
		prevLease := TaskLeaseTimeout
		ReleaseTimeout = 300 * time.Millisecond
		TaskLeaseTimeout = 2 * time.Second
		defer func() {
			ReleaseTimeout = prevRelease
			TaskLeaseTimeout = prevLease
		}()

		// fresh channels each run (the retry harness may re-enter)
		leaseWorkStarted = make(chan struct{}, 1)
		leaseWorkRelease = make(chan struct{})

		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		const maxTime = 60 * time.Second
		taskId := ScheduleTask(LeaseWork, &LeaseWorkArgs{}, clientSession, MaxTime(maxTime))

		taskWorker := NewTaskWorkerWithDefaults(ctx)
		taskWorker.AddTargets(NewTaskTarget(LeaseWork))

		// loop EvalTasks until the task becomes claimable (a single pass can
		// race the available-block boundary). Once it claims the task,
		// EvalTasks blocks running the (blocked) work while keepalive beats
		// fire, which is what this test inspects.
		stopEval := make(chan struct{})
		evalDone := make(chan struct{})
		go func() {
			defer close(evalDone)
			for {
				select {
				case <-stopEval:
					return
				default:
				}
				_, _, _, err := taskWorker.EvalTasks(1)
				connect.AssertEqual(t, err, nil)
				select {
				case <-stopEval:
					return
				case <-time.After(50 * time.Millisecond):
				}
			}
		}()

		// wait for the claim + work start
		select {
		case <-leaseWorkStarted:
		case <-time.After(10 * time.Second):
			t.Fatal("lease work never started")
		}

		// let several keepalive beats fire while the work is still blocked
		select {
		case <-time.After(1500 * time.Millisecond):
		}

		// Heartbeats keep the lease comfortably in the future, but it is not tied
		// to the task's 60-second MaxTime. A worker death therefore recovers in
		// roughly two seconds in this fixture, not one minute.
		var releaseTime time.Time
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, "SELECT release_time FROM pending_task WHERE task_id = $1", taskId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&releaseTime))
				}
			})
		})
		now := server.NowUtc()
		if !releaseTime.After(now.Add(1 * time.Second)) {
			t.Fatalf("heartbeat did not extend lease: release_time=%s now=%s", releaseTime, now)
		}
		if releaseTime.After(now.Add(5 * time.Second)) {
			t.Fatalf("lease is still pinned to MaxTime: release_time=%s now=%s", releaseTime, now)
		}

		// release the work and stop the eval loop cleanly
		close(leaseWorkRelease)
		close(stopEval)
		select {
		case <-evalDone:
		case <-time.After(10 * time.Second):
			t.Fatal("eval did not finish")
		}
	})
}

// An expired timestamp alone must not permit duplicate execution while the
// owner's PostgreSQL session is alive. Once that session disappears (as it does
// automatically on process death), the task becomes reclaimable without
// waiting for its declared MaxTime.
func TestTaskLeaseExpiryRequiresOwnerSessionLoss(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		previousLeaseTimeout := TaskLeaseTimeout
		TaskLeaseTimeout = 500 * time.Millisecond
		defer func() { TaskLeaseTimeout = previousLeaseTimeout }()

		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		taskId := ScheduleTask(
			LeaseWork,
			&LeaseWorkArgs{},
			clientSession,
			RunAt(server.NowUtc().Add(-time.Minute)),
			MaxTime(time.Hour),
		)

		firstOwner := NewTaskWorkerWithDefaults(ctx)
		claimed, firstGuard, err := firstOwner.takeTasks(1)
		if err != nil {
			t.Fatalf("initial claim: %v", err)
		}
		if firstGuard == nil {
			t.Fatal("initial claim has no advisory ownership guard")
		}
		defer firstGuard.release()
		if _, ok := claimed[taskId]; !ok || len(claimed) != 1 {
			t.Fatalf("initial claim = %v, want only %s", claimed, taskId)
		}

		secondOwner := NewTaskWorkerWithDefaults(ctx)
		claimed, secondGuard, err := secondOwner.takeTasks(1)
		if err != nil {
			t.Fatalf("claim before lease expiry: %v", err)
		}
		if secondGuard != nil {
			secondGuard.release()
			t.Fatal("empty claim unexpectedly retained an ownership guard")
		}
		if len(claimed) != 0 {
			t.Fatalf("second owner claimed live lease: %v", claimed)
		}

		// Wait beyond both the timestamp lease and the generated available_block's
		// one-second bucket edge. The live session lock must still reject a second
		// owner even though the timestamp heartbeat was deliberately stopped.
		time.Sleep(2 * time.Second)
		claimed, secondGuard, err = secondOwner.takeTasks(1)
		if err != nil {
			t.Fatalf("claim with expired timestamp but live owner: %v", err)
		}
		if secondGuard != nil {
			secondGuard.release()
			t.Fatal("expired timestamp bypassed live owner's advisory guard")
		}
		if len(claimed) != 0 {
			t.Fatalf("second owner claimed while first owner session was live: %v", claimed)
		}

		// Simulate the process/session disappearing. PostgreSQL drops its session
		// advisory locks, and the already-expired timestamp makes the task
		// immediately eligible for another worker.
		firstGuard.release()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			claimed, secondGuard, err = secondOwner.takeTasks(1)
			if err != nil {
				t.Fatalf("reclaim after owner session loss: %v", err)
			}
			if _, ok := claimed[taskId]; ok {
				if secondGuard == nil {
					t.Fatal("reclaimed task has no advisory ownership guard")
				}
				secondGuard.release()
				return
			}
			if secondGuard != nil {
				secondGuard.release()
				t.Fatal("empty reclaim unexpectedly retained an ownership guard")
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("task %s was not reclaimable after owner session loss", taskId)
	})
}

type Work2Args struct {
	Tag string
}

type Work2Result struct{}

func Work2(
	work2 *Work2Args,
	clientSession *session.ClientSession,
) (*Work2Result, error) {
	return &Work2Result{}, nil
}

// ScheduleTaskIfAbsent must atomically insert-or-detect-conflict on the
// run_once key: a first call inserts and reports scheduled == true; a second
// call with the SAME key while the first is still pending (unclaimed) must
// report scheduled == false and must NOT touch the first call's persisted
// args -- unlike plain ScheduleTask+RunOnce, whose ON CONFLICT DO UPDATE
// silently merges only timing/priority into the existing row while leaving
// (and thus never surfacing) the second call's args.
func TestScheduleTaskIfAbsent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		key := RunOnce("test_schedule_task_if_absent", server.NewId())

		scheduled, firstTaskId := ScheduleTaskIfAbsent(
			Work2,
			&Work2Args{Tag: "first"},
			clientSession,
			key,
		)
		assert.Equal(t, scheduled, true)

		// a second call with the same key, while the first is still pending,
		// must be rejected -- not merged
		scheduledAgain, _ := ScheduleTaskIfAbsent(
			Work2,
			&Work2Args{Tag: "second"},
			clientSession,
			key,
		)
		assert.Equal(t, scheduledAgain, false)

		// the persisted task must still be the FIRST call's args; the
		// second call's args must never have been written anywhere
		tasks := GetTasks(ctx, firstTaskId)
		task, ok := tasks[firstTaskId]
		if !ok {
			t.Fatal("first task not found")
		}
		var args Work2Args
		if err := json.Unmarshal([]byte(task.ArgsJson), &args); err != nil {
			t.Fatal(err)
		}
		assert.Equal(t, args.Tag, "first")

		// once the pending row is gone (simulating the run finishing), the
		// same key must be schedulable again
		RemovePendingTask(ctx, firstTaskId)

		scheduledAfterClear, _ := ScheduleTaskIfAbsent(
			Work2,
			&Work2Args{Tag: "third"},
			clientSession,
			key,
		)
		assert.Equal(t, scheduledAfterClear, true)
	})
}
