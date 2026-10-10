// The real max-time adapter interrupts only after an observed committed page.
// Five bounded attempts must cover one logical4103-row pass without extending
// its120s budget or replacing its pending identity or original lifetime.
package work

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

func TestStartupContractClosureBudgetResumesCommittedPass(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		baseCtx, cancel := context.WithTimeout(model.WithProviderWorkSessionSource(t.Context(), nil), 2*time.Minute)
		defer cancel()
		network, source, destination := newStartupClosureFreeClients(baseCtx)
		ids := newStartupClosureScanContracts(baseCtx, network, source, destination, 4103)
		started := server.NowUtc().Truncate(time.Microsecond).Add(-3 * time.Hour)
		explicit := started.Add(17 * time.Minute)
		server.Tx(baseCtx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(baseCtx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=ANY($1::uuid[])`,
				[]server.Id{ids[500], ids[2500]}, explicit))
		}, server.OptNoRetry())
		parentKey := task.RunOnce("schedule_open_contract_closures_on_startup")
		parentOwner := task.RunOnceOwnershipKey(parentKey)
		childKeys := map[server.PgOwnershipKey]bool{}
		for _, id := range ids {
			childKeys[task.RunOnceOwnershipKey(task.RunOnce("close_scheduled_contract", id))] = true
		}
		var timer chan time.Time
		bodyContexts := make(chan context.Context, 1)
		budgets := make(chan time.Duration, 8)
		published := 0
		ctx := task.Testing_WithTaskRunAfter(baseCtx, func(bodyCtx context.Context, budget time.Duration) <-chan time.Time {
			bodyContexts <- bodyCtx
			budgets <- budget
			return timer
		})
		ctx = server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind != server.PgOwnershipReleased {
				return
			}
			count := 0
			for _, key := range event.Keys {
				if childKeys[key] {
					count++
				} else if key != parentOwner {
					return
				}
			}
			if count == 0 {
				return
			}
			published += count
			if published == 1024 {
				// Released follows COMMIT and physical cleanup. The adapter's
				// own timer cancels its actual body before this callback joins.
				bodyCtx := <-bodyContexts
				close(timer)
				select {
				case <-bodyCtx.Done():
				case <-baseCtx.Done():
					server.Raise(baseCtx.Err())
				}
			}
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		var parent server.Id
		server.Tx(baseCtx, func(tx server.PgTx) {
			parent = task.ScheduleTaskInTx(tx, ScheduleOpenContractClosures,
				&ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: started}, owner,
				parentKey, task.RunAt(started), task.MaxTime(120*time.Second))
			server.RaisePgResult(tx.Exec(baseCtx, `UPDATE pending_task SET reschedule_error_count=19 WHERE task_id=$1`, parent))
		}, server.OptNoRetry())
		original := task.GetTasks(baseCtx, parent)[parent].ArgsJson
		worker := startupClosureWorker(ctx, NewStartupContractClosureTaskTarget())
		defer worker.Close()
		var retries []*task.Task
		finishedCount := 0
		for attempt := range 5 {
			timer = make(chan time.Time)
			published = 0
			makeCloseRetryTaskDue(baseCtx, parent)
			finished, retried, posts, err := worker.EvalTasks(1)
			if err != nil || len(posts) != 0 {
				t.Fatal("bounded scanner attempt lost ordinary handback", err)
			}
			if budget := <-budgets; budget != 120*time.Second {
				t.Fatal("scanner budget changed", budget)
			}
			if len(finished) == 1 && finished[0] == parent {
				finishedCount++
				if attempt != 4 || len(retried) != 0 {
					t.Fatal("scanner finished before all five ordered portions")
				}
				<-bodyContexts
				break
			}
			if len(finished) != 0 || len(retried) != 1 || retried[0] != parent || published != 1024 {
				t.Fatal("scanner did not retain one actual max-time interruption after its committed page", attempt)
			}
			current := task.GetTasks(baseCtx, parent)[parent]
			if current == nil || !strings.Contains(current.RescheduleError, "Timeout") ||
				!strings.Contains(current.RescheduleError, "context canceled") {
				t.Fatal("actual max-time cause or same pending owner lost")
			}
			retries = append(retries, current)
		}
		queued := readExpiryRecoveryQueue(t, baseCtx)
		if finishedCount != 1 || len(queued) != len(ids) {
			t.Fatalf("STARTUP_BUDGET_PREFIX_REPLAY: bounded claims omitted late contracts; finished=%d children=%d want=%d", finishedCount, len(queued)-1, len(ids))
		}
		for index, id := range ids {
			row, exists := queued[task.RunOnce("close_scheduled_contract", id).String()]
			var child CloseScheduledContractArgs
			deadline := started.Add(model.DefaultContractExpiration)
			if index == 500 || index == 2500 {
				deadline = explicit
			}
			if !exists || json.Unmarshal([]byte(row.args), &child) != nil || child.ContractId != id ||
				!child.Deadline.Equal(deadline) || !row.runAt.Equal(deadline) {
				t.Fatal("logical pass omitted a tail child or changed its original deadline", index)
			}
		}
		for index, retried := range retries {
			if retried.RescheduleErrorCount != 20+index || retried.RunAt.Sub(retried.ReleaseTime) < task.RescheduleTimeout ||
				retried.RunAt.Sub(retried.ReleaseTime) > task.RescheduleTimeout+time.Second {
				t.Fatal("acknowledged progress retained exponential deferral or cleared failure history", index)
			}
			var saved struct {
				StartedAt time.Time `json:"started_at"`
			}
			if json.Unmarshal([]byte(retried.ArgsJson), &saved) != nil || !saved.StartedAt.Equal(started) {
				t.Fatal("resumed claim changed the saved lifetime")
			}
		}
		complete := task.GetFinishedTasks(baseCtx, parent)[parent]
		if complete == nil || complete.ArgsJson != original || complete.ResultJson != "{}" {
			t.Fatal("EOF did not restore original arguments and empty result in normal completion")
		}
		server.Db(baseCtx, func(conn server.PgConn) {
			var untouched bool
			server.Raise(conn.QueryRow(baseCtx, `SELECT count(*)=4103 AND bool_and(outcome IS NULL AND close_time IS NULL AND transfer_byte_count=100)
                FROM transfer_contract WHERE contract_id=ANY($1::uuid[])`, ids).Scan(&untouched))
			if !untouched {
				t.Fatal("scanner publication changed contract financial state")
			}
		})
	})
}
