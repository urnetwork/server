package task

import (
	"context"
	"errors"
	mathrand "math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// helpers shared by the drain tests

func drainTestSettings() *TaskWorkerSettings {
	settings := DefaultTaskWorkerSettings()
	// fast claim polling so tests do not wait out the 5s idle poll
	settings.PollTimeout = 200 * time.Millisecond
	return settings
}

// pinRescheduleTimeouts pins the package-level reschedule tunables that other
// tests in this binary mutate (TestTask lowers them for throughput).
func pinRescheduleTimeouts(t testing.TB) {
	prevReschedule := RescheduleTimeout
	prevBackoffMax := RescheduleBackoffMaxTimeout
	RescheduleTimeout = 2 * time.Second
	RescheduleBackoffMaxTimeout = 1 * time.Hour
	t.Cleanup(func() {
		RescheduleTimeout = prevReschedule
		RescheduleBackoffMaxTimeout = prevBackoffMax
	})
}

type pendingTaskState struct {
	exists          bool
	runAt           time.Time
	releaseTime     time.Time
	rescheduleError string
	errorCount      int
}

func readPendingTaskState(ctx context.Context, taskId server.Id) (state pendingTaskState) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT run_at, release_time, reschedule_error, reschedule_error_count
				FROM pending_task
				WHERE task_id = $1
			`,
			taskId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				state.exists = true
				var rescheduleError *string
				server.Raise(result.Scan(
					&state.runAt,
					&state.releaseTime,
					&rescheduleError,
					&state.errorCount,
				))
				if rescheduleError != nil {
					state.rescheduleError = *rescheduleError
				}
			}
		})
	})
	return
}

// waitForSignal fails the test if the signal does not arrive in time.
func waitForSignal(t testing.TB, c chan struct{}, timeout time.Duration, what string) {
	select {
	case <-c:
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for %s", what)
	}
}

// 1. natural finish: a drain waits out an in-flight task, the task finishes,
// its post runs, and nothing is cancel-rescheduled

type drainFinishArgs struct{}
type drainFinishResult struct{}

var drainFinishStarted chan struct{}
var drainFinishPosted chan struct{}

func drainFinishWork(
	args *drainFinishArgs,
	clientSession *session.ClientSession,
) (*drainFinishResult, error) {
	select {
	case drainFinishStarted <- struct{}{}:
	default:
	}
	// natural completion, well within DrainFinishTimeout
	time.Sleep(300 * time.Millisecond)
	return &drainFinishResult{}, nil
}

func drainFinishWorkPost(
	args *drainFinishArgs,
	result *drainFinishResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	select {
	case drainFinishPosted <- struct{}{}:
	default:
	}
	return nil
}

func TestTaskWorkerDrainFinishesInFlight(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		pinRescheduleTimeouts(t)
		drainFinishStarted = make(chan struct{}, 1)
		drainFinishPosted = make(chan struct{}, 1)

		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		taskId := ScheduleTask(drainFinishWork, &drainFinishArgs{}, clientSession)

		taskWorker := NewTaskWorker(ctx, drainTestSettings())
		taskWorker.AddTargets(NewTaskTargetWithPost(drainFinishWork, drainFinishWorkPost))
		go taskWorker.Run()

		waitForSignal(t, drainFinishStarted, 15*time.Second, "task start")

		taskWorker.Drain()

		// the drain waited for the in-flight task: it finished, its post ran,
		// and nothing was canceled
		waitForSignal(t, drainFinishPosted, 1*time.Second, "task post")
		connect.AssertEqual(t, taskWorker.DrainCanceledCount(), 0)
		connect.AssertEqual(t, readPendingTaskState(ctx, taskId).exists, false)
		finishedTasks := GetFinishedTasks(ctx, taskId)
		finishedTask, ok := finishedTasks[taskId]
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, finishedTask.PostError, "")

		// once draining, Run must return immediately without re-registering
		// (the WaitGroup reuse guard)
		connect.AssertEqual(t, taskWorker.Draining(), true)
		runReturned := make(chan struct{})
		go func() {
			defer close(runReturned)
			taskWorker.Run()
		}()
		waitForSignal(t, runReturned, 1*time.Second, "post-drain Run return")
	})
}

// 2. cancel-to-reschedule: a straggler past DrainFinishTimeout is canceled,
// errors into the reschedule path with NO error-count advance and its claim
// released, and a second worker re-runs it immediately

type drainCancelArgs struct{}
type drainCancelResult struct{}

var drainCancelStarted chan struct{}
var drainCancelRerun chan struct{}
var drainCancelRerunMode bool
var drainCancelRerunModeLock sync.Mutex

func drainCancelWork(
	args *drainCancelArgs,
	clientSession *session.ClientSession,
) (*drainCancelResult, error) {
	drainCancelRerunModeLock.Lock()
	rerun := drainCancelRerunMode
	drainCancelRerunModeLock.Unlock()
	if rerun {
		select {
		case drainCancelRerun <- struct{}{}:
		default:
		}
		return &drainCancelResult{}, nil
	}

	select {
	case drainCancelStarted <- struct{}{}:
	default:
	}
	select {
	case <-clientSession.Ctx.Done():
		// a ctx-respecting task: unwind promptly on the drain cancel
		return nil, clientSession.Ctx.Err()
	case <-time.After(60 * time.Second):
		return nil, errors.New("drain cancel never arrived")
	}
}

func TestTaskWorkerDrainCancelsStragglerAndHandsBackClaim(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		pinRescheduleTimeouts(t)
		drainCancelStarted = make(chan struct{}, 1)
		drainCancelRerun = make(chan struct{}, 1)
		drainCancelRerunModeLock.Lock()
		drainCancelRerunMode = false
		drainCancelRerunModeLock.Unlock()

		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		taskId := ScheduleTask(drainCancelWork, &drainCancelArgs{}, clientSession, MaxTime(10*time.Minute))

		settings := drainTestSettings()
		settings.DrainFinishTimeout = 300 * time.Millisecond
		settings.DrainCancelTimeout = 10 * time.Second
		taskWorker := NewTaskWorker(ctx, settings)
		taskWorker.AddTargets(NewTaskTarget(drainCancelWork))
		go taskWorker.Run()

		waitForSignal(t, drainCancelStarted, 15*time.Second, "task start")

		drainStartTime := time.Now()
		taskWorker.Drain()
		drainElapsed := time.Since(drainStartTime)

		// the drain was bounded: finish wait + prompt cancel unwind
		connect.AssertEqual(t, drainElapsed < 5*time.Second, true)
		connect.AssertEqual(t, taskWorker.DrainCanceledCount(), 1)

		// the reschedule handed the claim back with no backoff advance
		state := readPendingTaskState(ctx, taskId)
		connect.AssertEqual(t, state.exists, true)
		connect.AssertEqual(t, state.errorCount, 0)
		connect.AssertEqual(t, strings.Contains(state.rescheduleError, "Drained"), true)
		now := server.NowUtc()
		// claim released: release_time is not held into the future
		connect.AssertEqual(t, state.releaseTime.Before(now.Add(1*time.Second)), true)
		// flat retry: run_at ~ now + jitter[0, RescheduleTimeout) + RescheduleTimeout
		connect.AssertEqual(t, state.runAt.Before(now.Add(2*RescheduleTimeout+time.Second)), true)

		// a fresh worker re-claims and completes the task promptly
		drainCancelRerunModeLock.Lock()
		drainCancelRerunMode = true
		drainCancelRerunModeLock.Unlock()

		taskWorker2 := NewTaskWorker(ctx, drainTestSettings())
		taskWorker2.AddTargets(NewTaskTarget(drainCancelWork))
		go taskWorker2.Run()
		waitForSignal(t, drainCancelRerun, 30*time.Second, "task re-run on second worker")
		taskWorker2.Drain()

		// completed on re-run: pending row gone, error count never advanced
		connect.AssertEqual(t, readPendingTaskState(ctx, taskId).exists, false)
	})
}

// 3. a function that completes successfully AFTER the drain cancel fired
// still records finished and still runs its post (the post context drops the
// function context's cancellation), so chains re-arm instead of stranding
// into the RunPost retry path

type drainPostArgs struct{}
type drainPostResult struct{}

var drainPostStarted chan struct{}

func drainPostWork(
	args *drainPostArgs,
	clientSession *session.ClientSession,
) (*drainPostResult, error) {
	select {
	case drainPostStarted <- struct{}{}:
	default:
	}
	// deliberately ignore the context: complete successfully after the drain
	// cancel has fired
	time.Sleep(500 * time.Millisecond)
	return &drainPostResult{}, nil
}

func drainPostWorkPost(
	args *drainPostArgs,
	result *drainPostResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	// exercise a db write on the post session context, like every real chain
	// re-arm does; a severed post context fails this loudly
	ScheduleTaskInTx(
		tx,
		drainPostWork,
		&drainPostArgs{},
		clientSession,
		RunOnce("drain_post_rearm"),
		RunAt(server.NowUtc().Add(1*time.Hour)),
	)
	return nil
}

func TestTaskWorkerDrainCompletedTaskStillPosts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		pinRescheduleTimeouts(t)
		drainPostStarted = make(chan struct{}, 1)

		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		taskId := ScheduleTask(drainPostWork, &drainPostArgs{}, clientSession)

		settings := drainTestSettings()
		settings.DrainFinishTimeout = 50 * time.Millisecond
		settings.DrainCancelTimeout = 10 * time.Second
		taskWorker := NewTaskWorker(ctx, settings)
		taskWorker.AddTargets(NewTaskTargetWithPost(drainPostWork, drainPostWorkPost))
		go taskWorker.Run()

		waitForSignal(t, drainPostStarted, 15*time.Second, "task start")

		taskWorker.Drain()

		// the function returned success despite the canceled context: it is
		// finished (not rescheduled), its post ran cleanly, and the chain
		// re-armed
		connect.AssertEqual(t, taskWorker.DrainCanceledCount(), 0)
		connect.AssertEqual(t, readPendingTaskState(ctx, taskId).exists, false)
		finishedTasks := GetFinishedTasks(ctx, taskId)
		finishedTask, ok := finishedTasks[taskId]
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, finishedTask.PostError, "")

		rearmed := false
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT 1 FROM pending_task WHERE run_once_key = $1`,
				RunOnce("drain_post_rearm").String(),
			)
			server.WithPgResult(result, err, func() {
				rearmed = result.Next()
			})
		})
		connect.AssertEqual(t, rearmed, true)
	})
}

// 4. a function that ignores its context entirely does not hang the drain:
// the drain gives up after finish+cancel timeouts and the task keeps its
// lease (the SIGKILL path the lease exists for)

type drainIgnoreArgs struct{}
type drainIgnoreResult struct{}

var drainIgnoreStarted chan struct{}

func drainIgnoreWork(
	args *drainIgnoreArgs,
	clientSession *session.ClientSession,
) (*drainIgnoreResult, error) {
	select {
	case drainIgnoreStarted <- struct{}{}:
	default:
	}
	// ignore the context far past DrainFinishTimeout + DrainCancelTimeout
	time.Sleep(2 * time.Second)
	return &drainIgnoreResult{}, nil
}

func TestTaskWorkerDrainBoundedWhenTaskIgnoresContext(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		pinRescheduleTimeouts(t)
		drainIgnoreStarted = make(chan struct{}, 1)

		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		taskId := ScheduleTask(drainIgnoreWork, &drainIgnoreArgs{}, clientSession, MaxTime(10*time.Minute))

		settings := drainTestSettings()
		settings.DrainFinishTimeout = 100 * time.Millisecond
		settings.DrainCancelTimeout = 200 * time.Millisecond
		taskWorker := NewTaskWorker(ctx, settings)
		taskWorker.AddTargets(NewTaskTarget(drainIgnoreWork))
		go taskWorker.Run()

		waitForSignal(t, drainIgnoreStarted, 15*time.Second, "task start")

		drainStartTime := time.Now()
		taskWorker.Drain()
		drainElapsed := time.Since(drainStartTime)

		// bounded: gave up after ~finish+cancel, well before the task's 2s
		connect.AssertEqual(t, drainElapsed < 1500*time.Millisecond, true)
		// the straggler is still running and still holds its lease
		connect.AssertEqual(t, taskWorker.InflightCount(), 1)
		state := readPendingTaskState(ctx, taskId)
		connect.AssertEqual(t, state.exists, true)
		connect.AssertEqual(t, server.NowUtc().Before(state.releaseTime), true)

		// (in prod the process exits here and the lease gates the re-run; in
		// the test the root ctx stays alive, so the task eventually finishes
		// and finalizes — wait for it so the env tears down cleanly)
		endTime := time.Now().Add(30 * time.Second)
		for {
			if _, ok := GetFinishedTasks(ctx, taskId)[taskId]; ok {
				break
			}
			if endTime.Before(time.Now()) {
				t.Fatalf("straggler task never finalized")
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
}

// A production drain cancels the process-serving context immediately after
// Drain returns. A task that ignored the drain cancel can still unwind during
// the container grace window; its completion transaction must use the
// worker's detached, bounded finalize context rather than the canceled root.
func TestTaskWorkerGiveUpStillFinalizesAfterRootCancel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		pinRescheduleTimeouts(t)
		drainIgnoreStarted = make(chan struct{}, 1)

		rootCtx, rootCancel := context.WithCancel(context.Background())
		clientSession := session.Testing_CreateClientSession(context.Background(), nil)
		defer clientSession.Cancel()

		taskId := ScheduleTask(
			drainIgnoreWork,
			&drainIgnoreArgs{},
			clientSession,
			MaxTime(10*time.Minute),
		)

		settings := drainTestSettings()
		settings.DrainFinishTimeout = 50 * time.Millisecond
		settings.DrainCancelTimeout = 100 * time.Millisecond
		settings.FinalizeTimeout = 10 * time.Second
		taskWorker := NewTaskWorker(rootCtx, settings)
		taskWorker.AddTargets(NewTaskTarget(drainIgnoreWork))
		go taskWorker.Run()

		waitForSignal(t, drainIgnoreStarted, 15*time.Second, "task start")
		taskWorker.Drain()
		rootCancel()

		endTime := time.Now().Add(15 * time.Second)
		for time.Now().Before(endTime) {
			if _, ok := GetFinishedTasks(context.Background(), taskId)[taskId]; ok {
				connect.AssertEqual(t, readPendingTaskState(context.Background(), taskId).exists, false)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("straggler did not finalize after the serving root was canceled")
	})
}

// The CLI keeps the process alive for one final handback grace before it
// cancels the serving root. This is the production counterpart to the
// detached-context test above: a task that returns shortly after Drain's
// give-up is durably finalized before main can exit.
func TestTaskWorkerFinalHandbackGraceBeforeProcessExit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		pinRescheduleTimeouts(t)
		drainIgnoreStarted = make(chan struct{}, 1)

		rootCtx, rootCancel := context.WithCancel(context.Background())
		defer rootCancel()
		clientSession := session.Testing_CreateClientSession(context.Background(), nil)
		defer clientSession.Cancel()

		taskId := ScheduleTask(
			drainIgnoreWork,
			&drainIgnoreArgs{},
			clientSession,
			MaxTime(10*time.Minute),
		)

		settings := drainTestSettings()
		settings.DrainFinishTimeout = 50 * time.Millisecond
		settings.DrainCancelTimeout = 100 * time.Millisecond
		settings.FinalizeTimeout = 10 * time.Second
		taskWorker := NewTaskWorker(rootCtx, settings)
		taskWorker.AddTargets(NewTaskTarget(drainIgnoreWork))
		go taskWorker.Run()

		waitForSignal(t, drainIgnoreStarted, 15*time.Second, "task start")
		taskWorker.Drain()
		if !taskWorker.WaitFinalHandback() {
			t.Fatal("final handback grace ended before the task finalized")
		}
		rootCancel()

		if _, ok := GetFinishedTasks(context.Background(), taskId)[taskId]; !ok {
			t.Fatal("task was not finalized before process-serving cancellation")
		}
		connect.AssertEqual(t, readPendingTaskState(context.Background(), taskId).exists, false)
	})
}

// 5. the drain-window WaitGroup reuse race: main-style loops that re-enter
// Run must not panic (`sync: WaitGroup is reused before previous Wait has
// returned`) when Drain runs concurrently. Without the enterRun guard this
// hammering panics the test binary.
func TestTaskWorkerRunDrainRace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		for round := 0; round < 20; round += 1 {
			settings := drainTestSettings()
			settings.PollTimeout = 10 * time.Millisecond
			taskWorker := NewTaskWorker(ctx, settings)

			var loopWg sync.WaitGroup
			for i := 0; i < 8; i += 1 {
				loopWg.Add(1)
				go func() {
					defer loopWg.Done()
					for {
						taskWorker.Run()
						if taskWorker.Draining() {
							return
						}
						// mimic the main loop's re-entry cadence, compressed
						time.Sleep(time.Duration(mathrand.Intn(2000)) * time.Microsecond)
					}
				}()
			}

			time.Sleep(time.Duration(mathrand.Intn(5000)) * time.Microsecond)
			taskWorker.Drain()
			loopWg.Wait()
			taskWorker.Close()
		}
	})
}

// 6. version-skew retry: a claimed task with no registered target advances
// its error count (visibility) but retries on the clamped flat cadence
// (~2^targetNotFoundBackoffMaxExponent * RescheduleTimeout), never the
// exponential backoff that would push a brand-new chain out during a deploy
// overlap

type unregisteredWorkArgs struct{}
type unregisteredWorkResult struct{}

func unregisteredWork(
	args *unregisteredWorkArgs,
	clientSession *session.ClientSession,
) (*unregisteredWorkResult, error) {
	return &unregisteredWorkResult{}, nil
}

func TestTargetNotFoundFlatRetry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		pinRescheduleTimeouts(t)

		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		taskId := ScheduleTask(
			unregisteredWork,
			&unregisteredWorkArgs{},
			clientSession,
			RunOnce("target_not_found_flat_retry"),
		)

		// a worker WITHOUT the target registered (the other build generation)
		taskWorker := NewTaskWorkerWithDefaults(ctx)

		makeDue := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE pending_task SET run_at = $2, release_time = $2 WHERE task_id = $1`,
					taskId,
					server.NowUtc().Add(-5*time.Second),
				))
			})
		}

		// the retry delay stays clamped at every error count, where the
		// normal backoff would be minutes by round 6
		flatCeiling := time.Duration(1<<targetNotFoundBackoffMaxExponent)*RescheduleTimeout + RescheduleTimeout + 10*time.Second
		for round := 0; round < 6; round += 1 {
			makeDue()
			evalStart := server.NowUtc()
			_, rescheduledTaskIds, _, err := taskWorker.EvalTasks(10)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(rescheduledTaskIds), 1)

			state := readPendingTaskState(ctx, taskId)
			connect.AssertEqual(t, state.exists, true)
			connect.AssertEqual(t, state.errorCount, round+1)
			connect.AssertEqual(t, strings.Contains(state.rescheduleError, "Target not found"), true)
			delay := state.runAt.Sub(evalStart)
			if flatCeiling < delay {
				t.Fatalf("round %d: target-not-found retry delay %s exceeds the flat ceiling %s", round, delay, flatCeiling)
			}
		}
	})
}

// 7. operator recovery: ReleaseTask clears a stranded claim (claimable per
// run_at) and KickTasks pulls a chain's next run to now

type adminWorkArgs struct{}
type adminWorkResult struct{}

var adminWorkRan chan struct{}

func adminWork(
	args *adminWorkArgs,
	clientSession *session.ClientSession,
) (*adminWorkResult, error) {
	select {
	case adminWorkRan <- struct{}{}:
	default:
	}
	return &adminWorkResult{}, nil
}

func TestReleaseAndKickTask(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		pinRescheduleTimeouts(t)
		adminWorkRan = make(chan struct{}, 1)

		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()

		taskId := ScheduleTask(
			adminWork,
			&adminWorkArgs{},
			clientSession,
			RunOnce("release_kick_test"),
			RunAt(server.NowUtc().Add(1*time.Hour)),
			MaxTime(30*time.Minute),
		)

		// simulate a claim stranded by a killed worker: claimed now, lease
		// held for the full max time
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE pending_task SET claim_time = $2, release_time = $3 WHERE task_id = $1`,
				taskId,
				server.NowUtc(),
				server.NowUtc().Add(30*time.Minute),
			))
		})

		taskWorker := NewTaskWorkerWithDefaults(ctx)
		taskWorker.AddTargets(NewTaskTarget(adminWork))

		// stuck: not claimable while the lease holds
		finishedTaskIds, rescheduledTaskIds, _, err := taskWorker.EvalTasks(10)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(finishedTaskIds)+len(rescheduledTaskIds), 0)

		// release clears the lease, but the task still waits for its run_at
		connect.AssertEqual(t, ReleaseTask(ctx, taskId), true)
		finishedTaskIds, rescheduledTaskIds, _, err = taskWorker.EvalTasks(10)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(finishedTaskIds)+len(rescheduledTaskIds), 0)

		// kick pulls the run to now (the raw schedule key matches the stored
		// json-encoded form)
		connect.AssertEqual(t, KickTasks(ctx, "release_kick_test"), int64(1))
		connect.AssertEqual(t, KickTasks(ctx, "no_such_key"), int64(0))

		// now claimable and runs
		endTime := time.Now().Add(30 * time.Second)
		ran := false
		for !ran && time.Now().Before(endTime) {
			finishedTaskIds, _, _, err = taskWorker.EvalTasks(10)
			connect.AssertEqual(t, err, nil)
			for _, finishedTaskId := range finishedTaskIds {
				if finishedTaskId == taskId {
					ran = true
				}
			}
			if !ran {
				time.Sleep(200 * time.Millisecond)
			}
		}
		connect.AssertEqual(t, ran, true)
		waitForSignal(t, adminWorkRan, 1*time.Second, "kicked task run")

		// a released, unclaimed task is gone from pending after completion
		connect.AssertEqual(t, readPendingTaskState(ctx, taskId).exists, false)
		connect.AssertEqual(t, ReleaseTask(ctx, taskId), false)
	})
}
