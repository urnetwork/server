// One physical Run slot owns a bounded real provider transaction and handback.
package model

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

func providerRunCohortSibling(_ *struct{}, _ *session.ClientSession) (*struct{}, error) {
	return &struct{}{}, nil
}

type providerRunCohortHeldTarget struct {
	task.Target
	entered chan struct{}
	release <-chan struct{}
}

func (self *providerRunCohortHeldTarget) Run(ctx context.Context, _ *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	close(self.entered)
	select {
	case <-self.release:
		return &struct{}{}, func(server.PgTx) ([]server.PostFunction, error) { return nil, nil }, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

type providerRunCohortWitness struct {
	applied int
	owned   bool
	err     error
}

// A real account-write boundary establishes the cohort size while an ordinary
// sibling retains one of the unchanged four slots. No timing absence or helper
// transaction manufactures the accounting or completion proof.
func runProviderSlotCohort(t *testing.T, count int) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		providerTotalsBatchWriteCounter(t, ctx)
		networkId := server.NewId()
		ids := make([]server.Id, 0, count)
		for range count {
			ids = append(ids, providerTotalsTestTask(ctx, server.NewId(), networkId))
		}
		originals := task.GetTasks(ctx, ids...)
		keys := map[server.PgOwnershipKey]bool{}
		for _, queued := range originals {
			keys[task.PendingTaskOwnershipKey(queued.TaskId, &queued.RunOnceKey)] = true
			keys[server.NewPgOwnershipKey("finished_task/task_id", queued.TaskId)] = true
		}
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		past := server.NowUtc().Truncate(time.Second).Add(-time.Hour)
		siblingId := task.ScheduleTask(providerRunCohortSibling, &struct{}{}, owner, task.RunAt(past.Add(-time.Hour)))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE task_id=ANY($1)`, ids, past))
		})
		providerRelease, siblingRelease := make(chan struct{}), make(chan struct{})
		var providerOnce, siblingOnce sync.Once
		witnesses := make(chan providerRunCohortWitness, count)
		first := true
		workerCtx := context.WithValue(ctx, accountBalanceWriteTestKey{}, func(runCtx context.Context, tx server.PgTx, observedNetwork server.Id) {
			if observedNetwork != networkId {
				panic("provider cohort wrote an unrelated account")
			}
			witness := providerRunCohortWitness{}
			ownedKeys := accountBalanceOwnershipKeys([]server.Id{networkId})
			rows, err := tx.Query(runCtx, `SELECT task_id FROM pending_task WHERE task_id=ANY($1) AND (args_json::jsonb->>'applied')::boolean ORDER BY task_id`, ids)
			if err == nil {
				for rows.Next() {
					var id server.Id
					if err = rows.Scan(&id); err != nil {
						break
					}
					witness.applied++
					ownedKeys = append(ownedKeys, task.PendingTaskOwnershipKey(id, &originals[id].RunOnceKey))
				}
				rows.Close()
				if err == nil {
					err = rows.Err()
				}
			}
			witness.err, witness.owned = err, server.TxOwnsKeys(tx, ownedKeys)
			witnesses <- witness
			if first {
				first = false
				select {
				case <-providerRelease:
				case <-runCtx.Done():
					panic(runCtx.Err())
				}
			}
		})
		completed := make(chan struct{})
		var completionLock sync.Mutex
		completedKeys := map[server.PgOwnershipKey]bool{}
		completionSizes := []int{}
		workerCtx = server.Testing_WithPgOwnershipObservation(workerCtx, func(event server.PgOwnershipEvent) {
			if event.Kind != server.PgOwnershipReleased || event.TransactionScoped || len(event.Keys) == 0 {
				return
			}
			for _, key := range event.Keys {
				if !keys[key] {
					return
				}
			}
			completionLock.Lock()
			defer completionLock.Unlock()
			completionSizes = append(completionSizes, len(event.Keys)/2)
			for _, key := range event.Keys {
				completedKeys[key] = true
			}
			if len(completedKeys) == 2*count {
				select {
				case <-completed:
				default:
					close(completed)
				}
			}
		})
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		if settings.BatchSize != 4 || settings.PollTimeout != 5*time.Second {
			t.Fatal("cohort fixture changed the production slot or polling profile")
		}
		worker := task.NewTaskWorker(workerCtx, settings)
		sibling := &providerRunCohortHeldTarget{Target: task.NewTaskTarget(providerRunCohortSibling), entered: make(chan struct{}), release: siblingRelease}
		worker.AddTargets(NewLegacyProviderTotalsTaskTarget(), sibling)
		done := make(chan error, 1)
		go func() {
			var runErr error
			server.HandleError(worker.Run, func(err error) { runErr = err })
			done <- runErr
		}()
		joined := false
		defer func() {
			providerOnce.Do(func() { close(providerRelease) })
			siblingOnce.Do(func() { close(siblingRelease) })
			worker.Drain()
			worker.WaitFinalHandback()
			worker.Close()
			if !joined {
				select {
				case <-done:
				case <-ctx.Done():
					t.Error("provider cohort failed to join cleanup")
				}
			}
		}()
		select {
		case <-sibling.entered:
		case <-ctx.Done():
			t.Fatal("ordinary sibling did not own its slot", ctx.Err())
		}
		var witness providerRunCohortWitness
		select {
		case witness = <-witnesses:
		case <-ctx.Done():
			t.Fatal("actual provider accounting did not reach its barrier", ctx.Err())
		}
		if witness.err != nil || witness.applied != 64 || !witness.owned || worker.InflightCount() != 2 {
			t.Fatalf("one provider slot did not own64 markers and all queue keys: applied=%d owned=%t inflight=%d err=%v", witness.applied, witness.owned, worker.InflightCount(), witness.err)
		}
		providerOnce.Do(func() { close(providerRelease) })
		select {
		case <-completed:
		case err := <-done:
			joined = true
			t.Fatal("provider Run stopped before every complete handback", err)
		case <-ctx.Done():
			t.Fatal("provider cohort handbacks did not complete", ctx.Err())
		}
		siblingOnce.Do(func() { close(siblingRelease) })
		worker.Drain()
		if !worker.WaitFinalHandback() {
			t.Fatal("provider cohort lost final handback")
		}
		select {
		case err := <-done:
			joined = true
			if err != nil {
				t.Fatal("provider cohort Run failed", err)
			}
		case <-ctx.Done():
			t.Fatal("provider cohort Run did not join", ctx.Err())
		}
		if worker.InflightCount() != 0 || worker.DrainCanceledCount() != 0 || len(task.GetTasks(ctx, ids...)) != 0 || len(task.GetFinishedTasks(ctx, siblingId)) != 1 {
			t.Fatal("provider cohort left unjoined work, cancellation or pending owners")
		}
		finished := task.GetFinishedTasks(ctx, ids...)
		if len(finished) != count {
			t.Fatal("provider cohort lost immutable contract identities")
		}
		for _, id := range ids {
			payload, err := decodeLegacyProviderTotals(finished[id].ArgsJson)
			original, originalErr := decodeLegacyProviderTotals(originals[id].ArgsJson)
			if err != nil || originalErr != nil || !payload.Applied || payload.ContractId != original.ContractId ||
				payload.Private != original.Private || payload.Version != original.Version || !slices.Equal(payload.Totals, original.Totals) ||
				finished[id].RunOnceKey != originals[id].RunOnceKey || !finished[id].PostCompleted || finished[id].RescheduleError != "" ||
				finished[id].RunStartTime.IsZero() || finished[id].RunEndTime.Before(finished[id].RunStartTime) {
				t.Fatal("provider completion changed an allocation identity or replay marker", errors.Join(err, originalErr))
			}
		}
		completionLock.Lock()
		sizes := append([]int(nil), completionSizes...)
		completionLock.Unlock()
		if len(sizes) != (count+63)/64 || sizes[0] != 64 || (count == 65 && sizes[1] != 1) {
			t.Fatal("provider completion did not retain bounded64+1 ownership", sizes)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var provided, revenue int64
			var writes int
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count,provided_net_revenue_nano_cents,
				(SELECT count(*) FROM test_provider_total_write WHERE network_id=$1)
				FROM account_balance WHERE network_id=$1`, networkId).Scan(&provided, &revenue, &writes))
			if provided != int64(17*count) || revenue != int64(29*count) || writes != (count+63)/64 {
				t.Fatalf("provider cohort lost exact accounting or repeated its account write: bytes=%d revenue=%d writes=%d", provided, revenue, writes)
			}
		})
	})
}

func TestLegacyProviderRunCohortOwns64InOneSlot(t *testing.T) {
	runProviderSlotCohort(t, 64)
}

func TestLegacyProviderRunCohortBounds65As64AndOne(t *testing.T) {
	runProviderSlotCohort(t, 65)
}
