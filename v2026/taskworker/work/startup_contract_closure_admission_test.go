// A busy child completion owner must not prevent independent later pages from
// publishing. The real worker retains failed scanner custody until every group
// can commit, with the original startup deadline and ordinary RunOnce policy.
package work

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Hold the same actual PostgreSQL keys used by child completion, without
// replacing the scanner, queue writes, task retry or task completion handlers.
func TestStartupContractClosureBusyChildOwnersKeepLaterPagesMoving(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		baseCtx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 2*time.Minute)
		defer cancel()
		networkId, sourceId, destinationId := newStartupClosureFreeClients(baseCtx)
		ids := newStartupClosureScanContracts(baseCtx, networkId, sourceId, destinationId, 2049)
		childKeys := make(map[server.PgOwnershipKey]bool, len(ids))
		for _, id := range ids {
			childKeys[task.RunOnceOwnershipKey(task.RunOnce("close_scheduled_contract", id))] = true
		}
		heldIndexes := []int{17, 1297}
		heldKeys := make([]server.PgOwnershipKey, len(heldIndexes))
		for index, heldIndex := range heldIndexes {
			heldKeys[index] = task.RunOnceOwnershipKey(task.RunOnce("close_scheduled_contract", ids[heldIndex]))
		}
		var refusals, releasedGroups atomic.Int64
		ctx := server.Testing_WithPgOwnershipObservation(baseCtx, func(event server.PgOwnershipEvent) {
			if startupCheckpointObservedChildren(event, childKeys) == 0 {
				return
			}
			if event.Kind == server.PgOwnershipWaiting || event.Kind == server.PgOwnershipRefused {
				refusals.Add(1)
			}
			if event.Kind == server.PgOwnershipReleased {
				// Small deterministic publication jitter perturbs pacing only;
				// actual admission and worker return establish all ordering.
				count := releasedGroups.Add(1)
				timer := time.NewTimer(time.Duration(1+count%3) * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-baseCtx.Done():
				}
			}
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) { ScheduleOpenContractClosuresOnStartup(owner, tx) })
		key := task.RunOnce("schedule_open_contract_closures_on_startup").String()
		initial := readExpiryRecoveryQueue(t, baseCtx)[key]
		var scanArgs ScheduleOpenContractClosuresArgs
		server.Raise(json.Unmarshal([]byte(initial.args), &scanArgs))
		if scanArgs.PageSize != 1024 || task.GetTasks(baseCtx, initial.id)[initial.id].RunMaxTimeSeconds != 120 {
			t.Fatal("fixture did not use the production startup page size and task budget")
		}
		deadline := scanArgs.StartedAt.Add(model.DefaultContractExpiration)
		earlier := scanArgs.StartedAt.Add(20 * time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			for _, index := range heldIndexes {
				task.ScheduleTaskInTx(tx, CloseScheduledContract, &CloseScheduledContractArgs{
					Private: true, ScheduledContractClose: ScheduledContractClose{ContractId: ids[index], Deadline: earlier},
				}, owner, task.RunOnce("close_scheduled_contract", ids[index]), task.RunAt(earlier), task.MaxTime(30*time.Second))
			}
		})
		before := readExpiryRecoveryQueue(t, baseCtx)
		ready, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		done := make(chan error, 1)
		joined := false
		go func() {
			var holderErr error
			server.HandleError(func() {
				server.OwnedTx(baseCtx, heldKeys, func(server.PgTx) {
					close(ready)
					select {
					case <-release:
					case <-baseCtx.Done():
						server.Raise(baseCtx.Err())
					}
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { holderErr = err })
			done <- holderErr
		}()
		defer func() {
			resume()
			if !joined {
				select {
				case err := <-done:
					joined = true
					if err != nil {
						t.Error("child owner fixture failed", err)
					}
				case <-time.After(10 * time.Second):
					cancel()
					t.Error("child owner fixture did not join")
				}
			}
		}()
		select {
		case <-ready:
		case err := <-done:
			joined = true
			t.Fatal("child owner fixture did not admit", err)
		case <-baseCtx.Done():
			t.Fatal("child owner fixture admission expired", baseCtx.Err())
		}
		worker := startupClosureWorker(ctx, NewStartupContractClosureTaskTarget())
		defer worker.Close()
		for attempt := range 2 {
			makeCloseRetryTaskDue(baseCtx, initial.id)
			finished, retried, posts, err := worker.EvalTasks(1)
			if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != initial.id {
				t.Fatal("busy child publication lost scanner retry custody", attempt, finished, retried, posts, err)
			}
			queued := readExpiryRecoveryQueue(t, baseCtx)
			current, found := queued[key]
			if !found || current.id != initial.id || startupCheckpointOriginalArgs(t, current.args) != initial.args {
				t.Fatal("busy retry replaced the scanner or its fixed startup timestamp", attempt)
			}
			var progressArgs ScheduleOpenContractClosuresArgs
			if json.Unmarshal([]byte(current.args), &progressArgs) != nil || progressArgs.Progress == nil ||
				progressArgs.Progress.After != (server.Id{}) || progressArgs.Progress.RetryFrom != nil {
				t.Fatal("busy EOF did not retain its exact earliest unpaid head group")
			}
			mismatches := 0
			for index, id := range ids {
				childKey := task.RunOnce("close_scheduled_contract", id).String()
				row, found := queued[childKey]
				busyGroup := index < 256 || 1280 <= index && index < 1536
				wanted := !busyGroup || slices.Contains(heldIndexes, index)
				if found != wanted {
					mismatches++
					continue
				}
				if !found {
					continue
				}
				if prior, existed := before[childKey]; existed {
					if row.id != prior.id || row.args != prior.args || !row.runAt.Equal(prior.runAt) {
						t.Fatal("busy pass changed an existing child or its earlier wake", attempt, index)
					}
				} else {
					var args CloseScheduledContractArgs
					if json.Unmarshal([]byte(row.args), &args) != nil || args.ContractId != id || !args.Deadline.Equal(deadline) || !row.runAt.Equal(deadline) {
						t.Fatal("later page lost its contract identity or startup deadline", attempt, index)
					}
				}
			}
			if mismatches != 0 {
				t.Errorf("STARTUP_BUSY_OWNER_TAIL_STARVED attempt=%d child_presence_mismatches=%d", attempt+1, mismatches)
			}
			if refusals.Load() == 0 {
				t.Fatal("no actual PostgreSQL refusal witnessed the held child owner")
			}
			before = queued
		}
		resume()
		select {
		case err := <-done:
			joined = true
			if err != nil {
				t.Fatal("child owner release failed", err)
			}
		case <-baseCtx.Done():
			t.Fatal("child owner release expired", baseCtx.Err())
		}
		evalStartupClosureTask(t, ctx, worker, initial.id)
		complete := readExpiryRecoveryQueue(t, baseCtx)
		if len(complete) != len(ids) {
			t.Fatal("owner release did not finish the same whole-pass scanner", len(complete))
		}
		for index, id := range ids {
			childKey := task.RunOnce("close_scheduled_contract", id).String()
			row, found := complete[childKey]
			var args CloseScheduledContractArgs
			wantedDeadline := deadline
			if slices.Contains(heldIndexes, index) {
				wantedDeadline = earlier
			}
			if !found || json.Unmarshal([]byte(row.args), &args) != nil || !args.Private || args.ContractId != id ||
				!args.Deadline.Equal(wantedDeadline) || !row.runAt.Equal(wantedDeadline) {
				t.Fatal("full retry omitted a busy child or changed its retained deadline", index)
			}
			if prior, existed := before[childKey]; existed && (row.id != prior.id || row.args != prior.args || !row.runAt.Equal(prior.runAt)) {
				t.Fatal("full retry replaced a previously committed child", index)
			}
		}
		server.Db(baseCtx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(baseCtx, `SELECT count(*)=$2 AND bool_and(outcome IS NULL AND close_time IS NULL
				AND expiration_time IS NULL AND transfer_byte_count=100)
				FROM transfer_contract WHERE contract_id=ANY($1::uuid[])`, ids, len(ids)).Scan(&exact))
			if !exact {
				t.Fatal("scanner publication changed contract accounting or lifetime state")
			}
		})
	})
}
