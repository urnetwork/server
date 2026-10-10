// Function lanes retain ordinary capacity and the existing ownership rules.
// These controls use exact claim boundaries, not a throughput timing guess.
package task

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func taskFunctionTestTurn(t testing.TB, worker *TaskWorker, name string) {
	t.Helper()
	index := slices.Index(worker.claimFunctionNames, name)
	if index < 0 {
		t.Fatal("test lane was not registered")
	}
	worker.stateLock.Lock()
	worker.claimFunctionTurn = uint64(2 * index)
	worker.stateLock.Unlock()
}

func TestTaskClaimFunctionRotationPreservesOrdinaryAndAliases(t *testing.T) {
	settings := DefaultTaskWorkerSettings()
	settings.FairClaimFunctions = true
	worker := NewTaskWorker(t.Context(), settings)
	defer worker.Close()
	const alias = "fixture.example/tasks.RetainedFunction"
	worker.AddTargets(NewTaskTarget(claimProfileAllowed, alias), NewTaskTarget(claimProfileExcluded))
	poll := &taskClaimPoll{}
	if !slices.Contains(worker.claimFunctionNames, alias) {
		t.Fatal("a retained registered alias has no fairness lane")
	}
	seen := map[string]int{}
	for turn := range 4 * len(worker.claimFunctionNames) {
		name := worker.nextClaimFunction(taskClaimOptions{poll: poll})
		if turn%2 == 1 {
			if name != "" {
				t.Fatal("function rotation consumed a reserved ordinary turn")
			}
		} else {
			seen[name]++
		}
	}
	for _, name := range worker.claimFunctionNames {
		if seen[name] != 2 {
			t.Fatal("rotation skipped or duplicated a registered lane")
		}
	}
	if worker.nextClaimFunction(taskClaimOptions{}) != "" {
		t.Fatal("finite EvalTasks borrowed Run fairness state")
	}
	legacy := NewTaskWorkerWithDefaults(t.Context())
	defer legacy.Close()
	if legacy.nextClaimFunction(taskClaimOptions{poll: poll}) != "" {
		t.Fatal("generic default unexpectedly enabled indexed lanes")
	}
	first, second := &taskClaimPoll{isolatedFunction: alias}, &taskClaimPoll{}
	if worker.nextClaimFunction(taskClaimOptions{poll: first}) != alias || first.isolatedFunction != "" || second.isolatedFunction != "" {
		t.Fatal("isolated handoff escaped or was not consumed by its Run")
	}
	first.isolatedFunction = alias
	worker.setDraining()
	if worker.nextClaimFunction(taskClaimOptions{poll: first}) != "" {
		t.Fatal("isolated handoff bypassed drain")
	}
}

// Concurrent Run callers share rotation, while their cursor handoffs remain
// caller-owned. The exact aggregate proves ordinary capacity is not borrowed.
func TestTaskClaimFunctionConcurrentRotationAndSaturation(t *testing.T) {
	name := NewTaskTarget(claimProfileAllowed).TargetFunctionName()
	settings := DefaultTaskWorkerSettings()
	settings.FairClaimFunctions = true
	settings.TargetClaimLimits = map[string]int{name: 1}
	worker := NewTaskWorker(t.Context(), settings)
	defer worker.Close()
	worker.AddTargets(NewTaskTarget(claimProfileAllowed, "fixture.example/tasks.LimitedAlias"))
	results := make(chan string, 8*2*len(worker.claimFunctionNames))
	var joined sync.WaitGroup
	for range 8 {
		joined.Add(1)
		go func() {
			defer joined.Done()
			poll := &taskClaimPoll{}
			for range 2 * len(worker.claimFunctionNames) {
				results <- worker.nextClaimFunction(taskClaimOptions{poll: poll})
			}
		}()
	}
	joined.Wait()
	close(results)
	counts := map[string]int{}
	for name := range results {
		counts[name]++
	}
	if counts[""] != 8*len(worker.claimFunctionNames) {
		t.Fatal("concurrent rotation lost ordinary turns")
	}
	for _, name := range worker.claimFunctionNames {
		if counts[name] != 8 {
			t.Fatal("concurrent rotation lost a registered lane")
		}
	}
	reservation, admitted := worker.reserveTaskClaim(name)
	if !admitted || reservation == nil {
		t.Fatal("could not establish target saturation")
	}
	defer reservation.release()
	for _, alias := range []string{name, "fixture.example/tasks.LimitedAlias"} {
		taskFunctionTestTurn(t, worker, alias)
		if worker.nextClaimFunction(taskClaimOptions{poll: &taskClaimPoll{}}) != "" {
			t.Fatal("saturated canonical target or alias consumed a lane")
		}
	}
}

// All 65 candidates in a selected function are owned. The same call must then
// admit an older ordinary row without changing any refused row's metadata.
func TestTaskClaimFunctionBusyWindowFallsBackToOrdinary(t *testing.T) {
	for _, queueOwnership := range []bool{false, true} {
		runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
			owner := session.NewLocalClientSession(ctx, "", nil)
			defer owner.Cancel()
			settings := DefaultTaskWorkerSettings()
			settings.FairClaimFunctions = true
			worker := NewTaskWorker(ctx, settings)
			defer worker.Close()
			worker.AddTargets(NewTaskTarget(claimProfileAllowed), NewTaskTarget(claimProfileExcluded))
			ordinary := ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, owner, RunAt(server.NowUtc().Add(-2*time.Hour)))
			ids := make([]server.Id, 0, 65)
			keys := make([]server.PgOwnershipKey, 0, 65)
			for range 65 {
				id := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
				ids = append(ids, id)
				keys = append(keys, PendingTaskOwnershipKey(id, nil))
			}
			before := GetTasks(ctx, ids...)
			entered, release := make(chan struct{}), make(chan struct{})
			ownerDone := make(chan error, 1)
			var releaseOnce sync.Once
			go func() {
				var result error
				server.HandleError(func() {
					if queueOwnership {
						server.OwnedTx(ctx, keys, func(server.PgTx) {
							close(entered)
							taskQueueWait(ctx, release)
						}, server.TxReadCommitted, server.OptNoRetry())
					} else {
						conn, err := server.AcquireMaintenanceDbConn(ctx)
						server.Raise(err)
						defer conn.Release()
						defer func() { server.RaisePgResult(conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock_all()`)) }()
						for _, id := range ids {
							server.RaisePgResult(conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, taskAdvisoryLockKey(id)))
						}
						close(entered)
						taskQueueWait(ctx, release)
					}
				}, func(err error) { result = err })
				ownerDone <- result
			}()
			joined := false
			defer func() {
				releaseOnce.Do(func() { close(release) })
				if !joined {
					<-ownerDone
				}
			}()
			taskQueueWait(ctx, entered)
			refused := 0
			worker.claimQueueAdmission = func(id server.Id, admitted bool) {
				if id != ordinary && !admitted {
					refused++
				}
			}
			taskFunctionTestTurn(t, worker, NewTaskTarget(claimProfileAllowed).TargetFunctionName())
			claimed, guard, isolated, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{poll: &taskClaimPoll{}})
			if guard != nil {
				defer guard.release()
			}
			if err != nil || isolated || guard == nil || len(claimed) != 1 || claimed[ordinary] == nil || queueOwnership && refused != 65 {
				t.Fatal("owned function window hid ordinary fallback", queueOwnership, refused, len(claimed), err)
			}
			if !reflect.DeepEqual(before, GetTasks(ctx, ids...)) {
				t.Fatal("function refusal changed a durable lease or queue identity")
			}
			releaseOnce.Do(func() { close(release) })
			ownerErr := <-ownerDone
			joined = true
			if ownerErr != nil {
				t.Fatal(ownerErr)
			}
		})
	}
}

// Normalized retained aliases participate in the lane; future/leased rows and
// unregistered older work remain untouched by the scoped worker.
func TestTaskClaimFunctionAliasAndFutureEligibility(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		settings := DefaultTaskWorkerSettings()
		settings.FairClaimFunctions = true
		settings.ClaimRegisteredTargetsOnly = true
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		target := NewTaskTarget(claimProfileAllowed)
		worker.AddTargets(target)
		now := server.NowUtc().Truncate(time.Second)
		retained := []server.Id{}
		for range 70 {
			retained = append(retained, ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, owner, RunAt(now.Add(-2*time.Hour))))
		}
		future := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(now.Add(time.Hour)))
		leased := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(now.Add(-time.Hour)))
		wanted := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(now.Add(-time.Hour)))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET release_time=$2 WHERE task_id=$1`, leased, now.Add(time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET function_name=$2 WHERE task_id=$1`, wanted,
				strings.Replace(target.TargetFunctionName(), "/server/", "/server/v37/", 1)))
		})
		retained = append(retained, future, leased)
		before := GetTasks(ctx, retained...)
		taskFunctionTestTurn(t, worker, target.TargetFunctionName())
		claimed, guard, _, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{poll: &taskClaimPoll{}})
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(claimed) != 1 || claimed[wanted] == nil || !reflect.DeepEqual(before, GetTasks(ctx, retained...)) {
			t.Fatal("function lane changed alias dispatch, profile or eligibility", len(claimed), err)
		}
	})
}

// Actual plans must read only the selected function's due prefix, despite a
// much larger, older ordinary queue. Both normal cache policies are exercised.
func TestTaskClaimFunctionIndexBoundsUnrelatedBacklog(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		settings := DefaultTaskWorkerSettings()
		settings.FairClaimFunctions = true
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		lane := NewTaskTarget(claimProfileAllowed).TargetFunctionName()
		bulk := NewTaskTarget(claimProfileExcluded).TargetFunctionName()
		worker.AddTargets(NewTaskTarget(claimProfileAllowed), NewTaskTarget(claimProfileExcluded))
		const bulkCount, laneCount, limit = 32768, 96, 68
		past := server.NowUtc().Add(-48 * time.Hour).Truncate(time.Second)
		future := server.NowUtc().Add(time.Hour)
		rows := make([][]any, 0, bulkCount+3*laneCount)
		for index := range bulkCount + 3*laneCount {
			var id server.Id
			binary.BigEndian.PutUint64(id[8:], uint64(index+1))
			name, runAt, releaseAt := bulk, past, past
			if index >= bulkCount {
				name = lane
				if index%2 == 0 {
					name = strings.Replace(lane, "/server/", "/server/v37/", 1)
				}
				runAt = past.Add(time.Hour)
				if index >= bulkCount+2*laneCount {
					releaseAt = future
				} else if index >= bulkCount+laneCount {
					runAt = future
				}
			}
			rows = append(rows, []any{id, name, `{}`, runAt, DefaultPriority, int(DefaultMaxTime / time.Second), past, releaseAt})
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := tx.CopyFrom(ctx, pgx.Identifier{"pending_task"},
				[]string{"task_id", "function_name", "args_json", "run_at", "run_priority", "run_max_time_seconds", "claim_time", "release_time"}, pgx.CopyFromRows(rows))
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE pending_task`))
		})
		query, args := worker.taskFunctionCandidatesQuery(server.NowUtc().Unix()/BlockSizeSeconds, limit, true, lane)
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			server.MaintenanceDb(ctx, func(conn server.PgConn) {
				tx, err := conn.Begin(ctx)
				server.Raise(err)
				defer tx.Rollback(ctx)
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = `+mode))
				var raw []byte
				server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) `+query, args...).Scan(&raw))
				var plans []struct {
					Plan taskClaimQueryTestPlan `json:"Plan"`
				}
				server.Raise(json.Unmarshal(raw, &plans))
				if len(plans) != 1 || plans[0].Plan.ActualRows != limit {
					t.Fatal("function plan did not return its complete bounded window", mode, string(raw))
				}
				visits, indexed := float64(0), false
				var walk func(taskClaimQueryTestPlan)
				walk = func(plan taskClaimQueryTestPlan) {
					if plan.RelationName == "pending_task" {
						visits += (plan.ActualRows + plan.FilteredRows) * plan.ActualLoops
						indexed = indexed || plan.IndexName == "pending_task_function_poll_order"
					}
					if plan.NodeType == "Sort" || plan.NodeType == "Incremental Sort" || plan.NodeType == "Materialize" {
						t.Error("function lane materialized queue rows", mode, string(raw))
					}
					for _, child := range plan.Plans {
						walk(child)
					}
				}
				walk(plans[0].Plan)
				t.Logf("function lane mode=%s rows_visited=%.0f plan=%s", mode, visits, raw)
				if !indexed || visits != limit {
					t.Fatal("function lane read unrelated, future or leased backlog", mode, visits, indexed)
				}
			})
		}
	})
}

// Ordinary executions retain their guard while a later isolated lane is
// deferred. The exact lane must survive this Run's next initial claim.
type taskFunctionIsolationTarget struct {
	Target
	heldId    server.Id
	firstId   server.Id
	isolation bool
	started   chan struct{}
	release   <-chan struct{}
	tailCalls *atomic.Int32
}

func (self *taskFunctionIsolationTarget) Run(ctx context.Context, queued *Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	if self.isolation || queued.TaskId == self.heldId {
		close(self.started)
		select {
		case <-self.release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	} else if queued.TaskId != self.firstId {
		self.tailCalls.Add(1)
	}
	return self.Target.Run(ctx, queued)
}

func TestTaskClaimFunctionRunCarriesIsolationPastBulk(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		settings := DefaultTaskWorkerSettings()
		settings.BatchSize = 2
		settings.FairClaimFunctions = true
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		past := server.NowUtc().Add(-3 * time.Hour)
		heldId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(past))
		firstId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(past.Add(time.Second)))
		tailIds := []server.Id{}
		for range 80 {
			tailIds = append(tailIds, ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(past.Add(time.Hour))))
		}
		isolatedId := ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, owner, RunAt(past.Add(2*time.Hour)), Priority(DefaultPriority+10))
		before := GetTasks(ctx, tailIds...)
		heldStarted, heldRelease := make(chan struct{}), make(chan struct{})
		isolatedStarted, isolatedRelease := make(chan struct{}), make(chan struct{})
		var tailCalls atomic.Int32
		worker.AddTargets(
			&taskFunctionIsolationTarget{Target: NewTaskTarget(claimProfileAllowed), heldId: heldId, firstId: firstId, started: heldStarted, release: heldRelease, tailCalls: &tailCalls},
			&taskFunctionIsolationTarget{Target: NewTaskTarget(claimProfileExcluded), isolation: true, started: isolatedStarted, release: isolatedRelease, tailCalls: &tailCalls},
		)
		worker.claimFunctionTurn = 1 // First initial claim uses the ordinary queue.
		var initial sync.Once
		worker.claimAfterCommit = func(*taskClaimGuard) {
			initial.Do(func() { taskFunctionTestTurn(t, worker, NewTaskTarget(claimProfileExcluded).TargetFunctionName()) })
		}
		var isolatedLocks atomic.Int32
		worker.claimCandidateLocked = func(id server.Id) {
			if id == isolatedId {
				isolatedLocks.Add(1)
			}
		}
		deferred := make(chan struct{})
		var deferredOnce sync.Once
		worker.claimBeforeCommit = func(*taskClaimGuard) error {
			if isolatedLocks.Load() == 1 {
				deferredOnce.Do(func() {
					worker.stateLock.Lock()
					worker.claimFunctionTurn = 1 // A lost handoff would choose the old global prefix.
					worker.stateLock.Unlock()
					close(deferred)
				})
			}
			return nil
		}
		done := make(chan any, 1)
		go func() { done <- server.HandleError(worker.Run) }()
		var heldOnce, isolatedOnce sync.Once
		joined := false
		defer func() {
			worker.runCancel()
			heldOnce.Do(func() { close(heldRelease) })
			isolatedOnce.Do(func() { close(isolatedRelease) })
			if !joined {
				<-done
			}
		}()
		taskQueueWait(ctx, heldStarted)
		taskQueueWait(ctx, deferred)
		pending := GetTasks(ctx, isolatedId)[isolatedId]
		if pending == nil || pending.ClaimGeneration != 0 || !pending.ClaimTime.IsZero() || tailCalls.Load() != 0 {
			t.Fatal("refill claimed isolated work before its sibling joined")
		}
		select {
		case <-isolatedStarted:
			t.Fatal("isolated body overlapped its ordinary sibling")
		default:
		}
		heldOnce.Do(func() { close(heldRelease) })
		taskQueueWait(ctx, isolatedStarted)
		if isolatedLocks.Load() != 2 || tailCalls.Load() != 0 || len(GetFinishedTasks(ctx, heldId, firstId)) != 2 || !reflect.DeepEqual(before, GetTasks(ctx, tailIds...)) {
			t.Fatal("next Run claim lost the isolated lane behind ordinary bulk")
		}
		worker.runCancel()
		isolatedOnce.Do(func() { close(isolatedRelease) })
		runErr := <-done
		joined = true
		if runErr != nil || worker.InflightCount() != 0 || len(GetFinishedTasks(ctx, isolatedId)) != 1 || len(GetTasks(ctx, isolatedId)) != 0 {
			t.Fatal("isolated handoff did not finish and release its real Run owner", runErr)
		}
	})
}

// A selected function cannot borrow a live business group's capacity. Its
// refusal leaves the existing group session intact and uses ordinary capacity.
func TestTaskClaimFunctionGroupRefusalKeepsOtherProgress(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		group := server.NewId()
		first := NewTaskWorkerWithDefaults(ctx)
		defer first.Close()
		target := &taskClaimGroupTestTarget{Target: NewTaskTarget(taskClaimGroupTestCall)}
		first.AddTargets(target)
		heldId := ScheduleTask(taskClaimGroupTestCall, &taskClaimGroupTestArgs{GroupIds: []server.Id{group}, MaxTasks: 1}, owner, RunAt(server.NowUtc().Add(-3*time.Hour)))
		held, heldGuard, err := first.takeTasks(1)
		if heldGuard != nil {
			defer heldGuard.release()
		}
		if err != nil || len(held) != 1 || held[heldId] == nil {
			t.Fatal("could not establish the actual business group owner", err)
		}
		ordinary := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(server.NowUtc().Add(-2*time.Hour)))
		blocked := ScheduleTask(taskClaimGroupTestCall, &taskClaimGroupTestArgs{GroupIds: []server.Id{group}, MaxTasks: 1}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
		before := GetTasks(ctx, heldId, blocked)
		settings := DefaultTaskWorkerSettings()
		settings.FairClaimFunctions = true
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(target, NewTaskTarget(claimProfileAllowed))
		taskFunctionTestTurn(t, worker, target.TargetFunctionName())
		claimed, guard, _, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{poll: &taskClaimPoll{}})
		if guard != nil {
			defer guard.release()
		}
		if err != nil || len(claimed) != 1 || claimed[ordinary] == nil || !reflect.DeepEqual(before, GetTasks(ctx, heldId, blocked)) {
			t.Fatal("function lane crossed group ownership or lost ordinary progress", len(claimed), err)
		}
		probe, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer probe.Release()
		taskClaimGroupRequireKeys(t, ctx, probe, []taskClaimGroupKey{taskClaimGroupLockKey(target.TargetFunctionName(), group)}, true)
	})
}

// A sparse ordinary function lane must fill its unused physical slot from the
// global queue before arming an idle poll, while retaining the first guard.
func TestTaskClaimFunctionSparseLaneFillsOrdinaryCapacity(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		settings := DefaultTaskWorkerSettings()
		settings.FairClaimFunctions = true
		settings.BatchSize = 2
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		laneId := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(server.NowUtc().Add(-time.Hour)))
		ordinaryId := ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, owner, RunAt(server.NowUtc().Add(-2*time.Hour)))
		laneStarted, ordinaryStarted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		worker.AddTargets(
			&taskFunctionIsolationTarget{Target: NewTaskTarget(claimProfileAllowed), heldId: laneId, started: laneStarted, release: release, tailCalls: &calls},
			&taskFunctionIsolationTarget{Target: NewTaskTarget(claimProfileExcluded), heldId: ordinaryId, started: ordinaryStarted, release: release, tailCalls: &calls},
		)
		taskFunctionTestTurn(t, worker, NewTaskTarget(claimProfileAllowed).TargetFunctionName())
		initialCount := make(chan int, 1)
		var first sync.Once
		worker.claimAfterCommit = func(guard *taskClaimGuard) { first.Do(func() { initialCount <- len(guard.taskIds) }) }
		worker.pollAfter = func(time.Duration) <-chan time.Time {
			panic("sparse function lane polled before filling ordinary capacity")
		}
		done := make(chan any, 1)
		go func() { done <- server.HandleError(worker.Run) }()
		var releaseOnce sync.Once
		joined := false
		defer func() {
			worker.runCancel()
			releaseOnce.Do(func() { close(release) })
			if !joined {
				<-done
			}
		}()
		taskQueueWait(ctx, laneStarted)
		taskQueueWait(ctx, ordinaryStarted)
		if <-initialCount != 1 || worker.InflightCount() != 2 {
			t.Fatal("sparse lane did not retain its owner while filling the other slot")
		}
		worker.runCancel()
		releaseOnce.Do(func() { close(release) })
		runErr := <-done
		joined = true
		if runErr != nil || worker.InflightCount() != 0 || len(GetFinishedTasks(ctx, laneId, ordinaryId)) != 2 || len(GetTasks(ctx, laneId, ordinaryId)) != 0 {
			t.Fatal("sparse lane did not join and finalize both ordinary owners", runErr)
		}
	})
}
