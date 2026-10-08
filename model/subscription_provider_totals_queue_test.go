// Provider credits and durable markers enter through the same queue ownership as
// publication and finalization. These controls hold real PostgreSQL row writers.
package model

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// The target adapter runs the actual provider function with its durable identity.
// A later registered worker completes every original pending owner and its posts.
func providerQueueTestTarget(queued map[server.Id]*task.Task, taskIds []server.Id) task.Target {
	target := NewLegacyProviderTotalsTaskTarget().(*legacyProviderTotalsTaskTarget)
	ordered := make([]*task.Task, 0, len(taskIds))
	for _, id := range taskIds {
		ordered = append(ordered, queued[id])
	}
	return target.PrepareTaskBatch(ordered)
}

func providerQueueTestFinalize(t testing.TB, ctx context.Context, original map[server.Id]*task.Task) {
	t.Helper()
	ids := make([]server.Id, 0, len(original))
	for id := range original {
		ids = append(ids, id)
	}
	providerTotalsBatchDue(ctx, ids)
	worker := accountOwnershipTestWorker(ctx)
	defer worker.Close()
	finished, retried, posts, err := worker.EvalTasks(len(ids))
	if err != nil || len(finished) != len(ids) || len(retried)+len(posts) != 0 {
		t.Fatal("provider queue owners did not durably finalize", err)
	}
	accountOwnershipRequireFinished(t, ctx, original)
}

// Queue admission precedes the provider's first pending-row lock, including all
// 64 members of a prepared batch. A real unrelated provider progresses while held.
func providerQueueTestHeldWriter(t *testing.T, count int) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		providerTotalsBatchWriteCounter(t, ctx)
		networkId := server.NewId()
		ids := make([]server.Id, 0, count)
		for range count {
			ids = append(ids, providerTotalsTestTask(ctx, server.NewId(), networkId))
		}
		original := task.GetTasks(ctx, ids...)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2 WHERE task_id=ANY($1)`,
				ids, server.NowUtc().Add(time.Hour)))
		}, server.TxReadCommitted, server.OptNoRetry())
		heldId := ids[len(ids)-1]
		rawKey := original[heldId].RunOnceKey
		key := task.PendingTaskOwnershipKey(heldId, &rawKey)
		entered, releaseOwner := make(chan uint32, 1), make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseOwner) }) }
		holder := startAccountOwnershipTest(func() {
			server.OwnedTx(ctx, []server.PgOwnershipKey{key}, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=run_at WHERE task_id=$1`, heldId))
				var pid uint32
				server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
				entered <- pid
				select {
				case <-releaseOwner:
				case <-ctx.Done():
					server.Raise(ctx.Err())
				}
			}, server.TxReadCommitted)
		})
		defer func() {
			release()
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			holder.join(t, cleanup)
		}()
		var heldPid uint32
		select {
		case heldPid = <-entered:
		case <-holder.done:
			t.Fatal("queue holder missed its actual pending write", holder.err)
		case <-ctx.Done():
			t.Fatal("queue holder did not arrive", ctx.Err())
		}
		var reruns atomic.Int32
		var premature atomic.Int32
		var held atomic.Bool
		held.Store(true)
		waiting := make(chan struct{}, 1)
		observed := server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		observed = server.Testing_WithPgOwnershipObservation(observed, func(event server.PgOwnershipEvent) {
			if !slices.Contains(event.Keys, key) {
				return
			}
			if event.Kind == server.PgOwnershipWaiting {
				select {
				case waiting <- struct{}{}:
				default:
				}
			}
			if event.Kind == server.PgOwnershipAdmitted && held.Load() {
				premature.Add(1)
			}
		})
		runCtx, cancelRun := context.WithCancel(observed)
		target := providerQueueTestTarget(original, ids)
		run := startAccountOwnershipTest(func() {
			result, post, err := target.Run(runCtx, original[ids[0]])
			server.Raise(err)
			if result == nil || post == nil {
				panic(errors.New("provider queue execution lost its result or handback"))
			}
			posts, err := post(nil)
			server.Raise(err)
			if len(posts) != 0 {
				panic(errors.New("provider queue execution invented an external post"))
			}
		})
		defer func() {
			release()
			cancelRun()
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			run.join(t, cleanup)
		}()
		waited := false
		for !waited {
			select {
			case <-waiting:
				waited = true
			case <-run.done:
				var pgErr *pgconn.PgError
				if errors.As(run.err, &pgErr) && pgErr.Code == "55P03" {
					t.Fatal("provider entered a held pending-row business lock before queue admission")
				}
				t.Fatal("provider ended before positive queue refusal", run.err)
			case <-holder.done:
				t.Fatal("actual pending writer ended before queue refusal", holder.err)
			case <-ctx.Done():
				t.Fatal("provider never acknowledged queue ownership refusal", ctx.Err())
			case <-time.After(time.Millisecond):
				server.Db(ctx, func(conn server.PgConn) {
					var blocked bool
					server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
                        WHERE datname=current_database() AND $1::integer=ANY(pg_blocking_pids(pid)))`, heldPid).Scan(&blocked))
					if blocked {
						t.Fatal("provider entered a held pending-row business lock before queue admission")
					}
				}, server.OptNoRetry())
			}
		}
		independentId := providerTotalsTestTask(ctx, server.NewId(), server.NewId())
		independent := task.GetTasks(ctx, independentId)
		providerQueueTestFinalize(t, ctx, independent)
		server.Db(ctx, func(conn server.PgConn) {
			var stillHeld bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
                WHERE pid=$1 AND state='idle in transaction' AND backend_xid IS NOT NULL)`, heldPid).Scan(&stillHeld))
			if !stillHeld || premature.Load() != 0 {
				t.Fatal("provider entered before the actual pending writer retired")
			}
		}, server.OptNoRetry())
		held.Store(false)
		release()
		server.Raise(holder.join(t, ctx))
		server.Raise(run.join(t, ctx))
		if reruns.Load() != 0 {
			t.Fatal("provider queue admission replayed a business transaction")
		}
		providerQueueTestFinalize(t, ctx, original)
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count=$2 AND provided_net_revenue_nano_cents=$3
                AND (SELECT count(*) FROM test_provider_total_write WHERE network_id=$1)=1
                FROM account_balance WHERE network_id=$1`, networkId, int64(17*count), int64(29*count)).Scan(&exact))
			if !exact {
				t.Fatal("queue admission or replay changed exact provider accounting")
			}
		}, server.OptNoRetry())
	})
}

func TestLegacyProviderTotalsSingletonQueueOwnerPrecedesPendingLock(t *testing.T) {
	providerQueueTestHeldWriter(t, 1)
}

func TestLegacyProviderTotalsMaximumBatchQueueOwnersPrecedePendingLocks(t *testing.T) {
	providerQueueTestHeldWriter(t, legacyProviderTotalsBatchLimit)
}

// The retained raw queue string is authoritative. An adversarial change between
// admission and BEGIN must roll back without a credit or marker, then recover.
func providerQueueTestChangedIdentity(t *testing.T, count int) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId := server.NewId()
		ids := make([]server.Id, 0, count)
		for range count {
			ids = append(ids, providerTotalsTestTask(ctx, server.NewId(), networkId))
		}
		original := task.GetTasks(ctx, ids...)
		changedId := ids[len(ids)-1]
		rawKey := original[changedId].RunOnceKey
		key := task.PendingTaskOwnershipKey(changedId, &rawKey)
		var changedRawKey *string
		if count > 1 {
			value := task.RunOnce("synthetic-provider-queue-change", server.NewId()).String()
			changedRawKey = &value
		}
		var changed atomic.Bool
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind != server.PgOwnershipAdmitted || !slices.Contains(event.Keys, key) || !changed.CompareAndSwap(false, true) {
				return
			}
			// Deliberately violate producer immutability outside the protocol.
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_once_key=$2 WHERE task_id=$1`, changedId, changedRawKey))
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		target := providerQueueTestTarget(original, ids)
		_, _, err := target.Run(observed, original[ids[0]])
		var phase *legacyProviderTotalsPhaseError
		if !changed.Load() || !errors.As(err, &phase) || phase.phase != legacyProviderTotalsPendingRead {
			t.Fatal("changed durable queue identity authorized provider accounting", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched bool
			server.Raise(conn.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM account_balance WHERE network_id=$1)
                AND (SELECT count(*) FROM pending_task WHERE task_id=ANY($2)
                    AND NOT (args_json::jsonb->>'applied')::boolean)=$3`, networkId, ids, count).Scan(&untouched))
			if !untouched {
				t.Fatal("changed queue membership leaked an account write or applied marker")
			}
		}, server.OptNoRetry())
		providerQueueTestFinalize(t, ctx, original)
		finished := task.GetFinishedTasks(ctx, changedId)[changedId]
		wantRawKey := ""
		if changedRawKey != nil {
			wantRawKey = *changedRawKey
		}
		if finished == nil || finished.RunOnceKey != wantRawKey {
			t.Fatal("queue recovery did not retain its actual null or stored identity")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count=$2 AND provided_net_revenue_nano_cents=$3
                FROM account_balance WHERE network_id=$1`, networkId, int64(17*count), int64(29*count)).Scan(&exact))
			if !exact {
				t.Fatal("fresh queue admission lost exact recovery accounting")
			}
		}, server.OptNoRetry())
	})
}

func TestLegacyProviderTotalsSingletonRejectsChangedStoredQueueIdentity(t *testing.T) {
	providerQueueTestChangedIdentity(t, 1)
}

func TestLegacyProviderTotalsBatchRejectsChangedStoredQueueIdentity(t *testing.T) {
	providerQueueTestChangedIdentity(t, 2)
}

// Missing publication ownership is a programming error before queue SQL.
// The adapter makes no late acquisition and never silently omits required work.
func TestLegacyProviderTotalsPublicationRequiresPredeclaredQueueOwner(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		contractId, networkId := server.NewId(), server.NewId()
		var admitted atomic.Int32
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipAdmitted || event.Kind == server.PgOwnershipWaiting || event.Kind == server.PgOwnershipRefused {
				admitted.Add(1)
			}
		})
		err := server.HandleError(func() {
			server.Tx(observed, func(tx server.PgTx) {
				queueLegacyProviderTotalsInTx(observed, tx, contractId, map[server.Id]*contractPayout{
					networkId: {payoutByteCount: 17, payout: 29},
				})
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		if err == nil || admitted.Load() != 0 {
			t.Fatal("unowned provider publication acquired late ownership or reached queue SQL", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched bool
			server.Raise(conn.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM pending_task)
                AND NOT EXISTS(SELECT 1 FROM account_balance)`).Scan(&untouched))
			if !untouched {
				t.Fatal("unowned publication left durable financial or task work")
			}
		}, server.OptNoRetry())
		id := providerTotalsTestTask(ctx, contractId, networkId)
		providerQueueTestFinalize(t, ctx, task.GetTasks(ctx, id))
	})
}

// Completion dispatch consults the registered target, not the prepared adapter.
// Its queue owner is automatic and no provider account post is declared.
func TestLegacyProviderTotalsFactoryDeclaresQueueOwnedCompletion(t *testing.T) {
	target := NewLegacyProviderTotalsTaskTarget()
	declaration, ok := target.(interface {
		TaskCompletionOwnershipKeys(*task.Task, string) ([]server.PgOwnershipKey, error)
	})
	if !ok {
		t.Fatal("provider finalization omitted its queue ownership declaration")
	}
	keys, err := declaration.TaskCompletionOwnershipKeys(&task.Task{TaskId: server.NewId()}, "{}")
	batching, batches := target.(interface{ TaskCompletionBatchEnabled() bool })
	if err != nil || len(keys) != 0 || !batches || !batching.TaskCompletionBatchEnabled() {
		t.Fatal("provider completion changed no-post batching or invented extra keys", err)
	}
}
