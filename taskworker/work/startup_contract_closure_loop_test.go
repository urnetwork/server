package work

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

func TestStartupContractClosureRejectsInvalidArguments(t *testing.T) {
	started := time.Date(2026, time.October, 9, 0, 0, 0, 0, time.UTC)
	for _, args := range []*ScheduleOpenContractClosuresArgs{
		nil,
		{},
		{PageSize: -1, StartedAt: started},
		{PageSize: startupContractClosureMaxPageSize + 1, StartedAt: started},
	} {
		result, err := ScheduleOpenContractClosures(args, nil)
		if err == nil || result != nil {
			t.Fatal("invalid startup scan arguments reached database work")
		}
	}
}

// A retained old-key task has no PageSize and may carry the old After cursor.
// The new startup key is independent; both actual executions scan from the head
// and publish the same child identities without postponing existing wakes.
func TestStartupContractClosureLegacyArgsAndNewKeyCoexist(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 2*time.Minute)
		defer cancel()
		networkId, sourceId, destinationId := newStartupClosureFreeClients(ctx)
		ids := newStartupClosureScanContracts(ctx, networkId, sourceId, destinationId, 1025)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		started := server.NowUtc().Truncate(time.Microsecond).Add(-10 * time.Minute)
		legacyArgs, err := json.Marshal(map[string]any{"started_at": started, "after": ids[len(ids)-1]})
		server.Raise(err)
		oldKey := task.RunOnce("schedule_open_contract_closures").String()
		newKey := task.RunOnce("schedule_open_contract_closures_on_startup").String()
		var oldId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			oldId = task.ScheduleTaskInTx(tx, ScheduleOpenContractClosures, &ScheduleOpenContractClosuresArgs{StartedAt: started}, owner,
				task.RunOnce("schedule_open_contract_closures"), task.RunAt(time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)), task.MaxTime(30*time.Second))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json=$2 WHERE task_id=$1`, oldId, string(legacyArgs)))
			ScheduleOpenContractClosuresOnStartup(owner, tx)
		})
		queued := readExpiryRecoveryQueue(t, ctx)
		old, oldFound := queued[oldKey]
		current, newFound := queued[newKey]
		var args ScheduleOpenContractClosuresArgs
		if !oldFound || !newFound || len(queued) != 2 || old.id != oldId || old.args != string(legacyArgs) || current.id == old.id ||
			json.Unmarshal([]byte(current.args), &args) != nil || args.PageSize != 1024 || args.StartedAt.IsZero() {
			t.Fatal("new startup key replaced retained work or did not create its independent loop")
		}
		worker := startupClosureWorker(ctx, NewStartupContractClosureTaskTarget())
		defer worker.Close()
		evalStartupClosureTask(t, ctx, worker, oldId)
		first := readExpiryRecoveryQueue(t, ctx)
		if _, found := first[oldKey]; found || len(first) != len(ids)+1 || first[newKey].id != current.id {
			t.Fatal("legacy After or zero PageSize skipped the head or changed the independent startup")
		}
		for _, id := range ids {
			row, found := first[task.RunOnce("close_scheduled_contract", id).String()]
			var child CloseScheduledContractArgs
			if !found || json.Unmarshal([]byte(row.args), &child) != nil || child.ContractId != id ||
				!child.Deadline.Equal(started.Add(model.DefaultContractExpiration)) || !row.runAt.Equal(child.Deadline) {
				t.Fatal("legacy argument compatibility omitted a child or changed its original lifetime")
			}
		}
		evalStartupClosureTask(t, ctx, worker, current.id)
		second := readExpiryRecoveryQueue(t, ctx)
		if len(second) != len(ids) {
			t.Fatal("new startup did not finish one complete pass", len(second))
		}
		for _, id := range ids {
			key := task.RunOnce("close_scheduled_contract", id).String()
			before, after := first[key], second[key]
			if before.id != after.id || before.args != after.args || !before.runAt.Equal(after.runAt) {
				t.Fatal("independent startup key duplicated a child or postponed its earlier wake")
			}
		}
	})
}

// Publish another ordinary startup request while the actual worker is paused
// after committing page one. The running task keeps its claim and arguments,
// finishes every later page, and then the existing RunOnce policy owns one rerun.
func TestStartupContractClosureActiveWakeKeepsFullPass(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		baseCtx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 2*time.Minute)
		defer cancel()
		networkId, sourceId, destinationId := newStartupClosureFreeClients(baseCtx)
		ids := newStartupClosureScanContracts(baseCtx, networkId, sourceId, destinationId, 2049)
		childKeys := map[server.PgOwnershipKey]bool{}
		for _, id := range ids {
			childKeys[task.RunOnceOwnershipKey(task.RunOnce("close_scheduled_contract", id))] = true
		}
		ready, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		defer resume()
		var observationLock sync.Mutex
		published, paused := 0, false
		ctx := server.Testing_WithPgOwnershipObservation(baseCtx, func(event server.PgOwnershipEvent) {
			if event.Kind != server.PgOwnershipReleased || len(event.Keys) == 0 {
				return
			}
			for _, key := range event.Keys {
				if !childKeys[key] {
					return
				}
			}
			observationLock.Lock()
			published += len(event.Keys)
			pause := published == 1024 && !paused
			if pause {
				paused = true
			}
			observationLock.Unlock()
			if pause {
				close(ready)
				select {
				case <-release:
				case <-baseCtx.Done():
				}
			}
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) { ScheduleOpenContractClosuresOnStartup(owner, tx) })
		key := task.RunOnce("schedule_open_contract_closures_on_startup").String()
		initial := readExpiryRecoveryQueue(t, ctx)[key]
		worker := startupClosureWorker(ctx, NewStartupContractClosureTaskTarget())
		defer worker.Close()
		makeCloseRetryTaskDue(ctx, initial.id)
		type evaluation struct {
			finished, retried, posts []server.Id
			err                      error
		}
		done := make(chan evaluation, 1)
		joined := false
		go func() {
			var result evaluation
			server.HandleError(func() {
				result.finished, result.retried, result.posts, result.err = worker.EvalTasks(1)
			}, func(err error) { result.err = err })
			done <- result
		}()
		defer func() {
			resume()
			if !joined {
				cancel()
				select {
				case <-done:
					joined = true
				case <-time.After(10 * time.Second):
					t.Error("active startup worker did not join after fixture cancellation")
				}
			}
		}()
		select {
		case <-ready:
		case result := <-done:
			joined = true
			t.Fatal("startup returned before committing its first page", result.err)
		case <-baseCtx.Done():
			t.Fatal("startup did not reach its first-page barrier", baseCtx.Err())
		}
		prefix := readExpiryRecoveryQueue(t, baseCtx)
		running := prefix[key]
		if running.id != initial.id || running.args != initial.args || len(prefix) != 1025 || running.claim != running.generation ||
			!running.releaseTime.After(server.NowUtc()) {
			t.Fatal("first-page barrier did not retain the actual live scanner claim")
		}
		for index, id := range ids {
			_, found := prefix[task.RunOnce("close_scheduled_contract", id).String()]
			if found != (index < 1024) {
				t.Fatal("first page did not publish its exact ordered prefix", index)
			}
		}
		// The producer is independent of the executing worker's context and
		// uses the real startup entry while that worker still owns its claim.
		producer := session.NewLocalClientSession(baseCtx, "", nil)
		defer producer.Cancel()
		server.Tx(baseCtx, func(tx server.PgTx) { ScheduleOpenContractClosuresOnStartup(producer, tx) })
		woken := readExpiryRecoveryQueue(t, baseCtx)[key]
		if woken.id != running.id || woken.args != running.args || woken.claim != running.claim ||
			woken.generation != running.generation+1 || woken.wakeAt == nil || woken.releaseTime.Before(running.releaseTime) {
			t.Fatal("concurrent startup wake replaced active arguments, identity or claim")
		}
		resume()
		var result evaluation
		select {
		case result = <-done:
			joined = true
		case <-baseCtx.Done():
			t.Fatal("active startup wake prevented full-pass completion", baseCtx.Err())
		}
		if result.err != nil || len(result.finished) != 1 || result.finished[0] != initial.id || len(result.retried)+len(result.posts) != 0 {
			t.Fatal("active wake canceled or retried the existing full pass", result.err)
		}
		complete := readExpiryRecoveryQueue(t, baseCtx)
		successor, found := complete[key]
		if !found || successor.id == initial.id || successor.args != initial.args || successor.claim != 0 ||
			!successor.runAt.Equal(*woken.wakeAt) || len(complete) != len(ids)+1 {
			t.Fatal("ordinary RunOnce completion lost its one requested successor or a later page")
		}
		for _, id := range ids {
			if _, found := complete[task.RunOnce("close_scheduled_contract", id).String()]; !found {
				t.Fatal("concurrent startup request stranded a later contract")
			}
		}
		evalStartupClosureTask(t, ctx, worker, successor.id)
		replayed := readExpiryRecoveryQueue(t, baseCtx)
		if len(replayed) != len(ids) {
			t.Fatal("one startup wake created an endless page/successor chain", len(replayed))
		}
		for _, id := range ids {
			key := task.RunOnce("close_scheduled_contract", id).String()
			before, after := complete[key], replayed[key]
			if before.id != after.id || before.args != after.args || !before.runAt.Equal(after.runAt) {
				t.Fatal("requested rescan replaced a committed child or postponed its wake")
			}
		}
	})
}

// A short page is not EOF until the next read is empty. A public contract
// committed during that page's publication must enter this same complete pass.
func TestStartupContractClosureFindsAppendAfterShortPage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		baseCtx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 2*time.Minute)
		defer cancel()
		networkId, sourceId, destinationId := newStartupClosureFreeClients(baseCtx)
		ids := newStartupClosureScanContracts(baseCtx, networkId, sourceId, destinationId, 3)
		keys := map[server.PgOwnershipKey]bool{}
		for _, id := range ids {
			keys[task.RunOnceOwnershipKey(task.RunOnce("close_scheduled_contract", id))] = true
		}
		var stateLock sync.Mutex
		appended := false
		var addedId server.Id
		var addedDeadline time.Time
		var appendErr error
		ctx := server.Testing_WithPgOwnershipObservation(baseCtx, func(event server.PgOwnershipEvent) {
			if event.Kind != server.PgOwnershipReleased || len(event.Keys) != len(ids) {
				return
			}
			for _, key := range event.Keys {
				if !keys[key] {
					return
				}
			}
			stateLock.Lock()
			create := !appended
			appended = true
			stateLock.Unlock()
			if !create {
				return
			}
			var id server.Id
			var deadline time.Time
			var err error
			server.HandleError(func() {
				id, deadline, err = model.CreateContractNoEscrowWithExpiration(baseCtx, networkId, sourceId, networkId, destinationId, 100, true)
				server.Raise(err)
			}, func(cause error) { err = cause })
			stateLock.Lock()
			addedId, addedDeadline, appendErr = id, deadline, err
			stateLock.Unlock()
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) { ScheduleOpenContractClosuresOnStartup(owner, tx) })
		key := task.RunOnce("schedule_open_contract_closures_on_startup").String()
		initial := readExpiryRecoveryQueue(t, ctx)[key]
		worker := startupClosureWorker(ctx, NewStartupContractClosureTaskTarget())
		defer worker.Close()
		evalStartupClosureTask(t, ctx, worker, initial.id)
		stateLock.Lock()
		created, id, deadline, err := appended, addedId, addedDeadline, appendErr
		stateLock.Unlock()
		if !created || err != nil || id.Cmp(ids[len(ids)-1]) <= 0 || deadline.IsZero() {
			t.Fatal("short-page fixture did not commit a new greater public contract", created, err)
		}
		queue := readExpiryRecoveryQueue(t, ctx)
		child, found := queue[task.RunOnce("close_scheduled_contract", id).String()]
		var args CloseScheduledContractArgs
		if !found || len(queue) != len(ids)+1 || json.Unmarshal([]byte(child.args), &args) != nil ||
			args.ContractId != id || !args.Deadline.Equal(deadline) || !child.runAt.Equal(deadline) {
			t.Fatal("scanner stopped on a short page before observing the committed greater contract")
		}
		if _, present := queue[key]; present {
			t.Fatal("empty-page EOF left an unnecessary continuation")
		}
	})
}
