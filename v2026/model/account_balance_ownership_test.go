// Account ownership of the provider totals entry points. The paid-completion
// writers that shared it ended with USDC provider payouts (owner decisions
// 2026-11-12 and 2026-11-13).
package model

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

type accountOwnershipTestRun struct {
	done chan struct{}
	err  error
}

func startAccountOwnershipTest(run func()) *accountOwnershipTestRun {
	result := &accountOwnershipTestRun{done: make(chan struct{})}
	go func() {
		defer close(result.done)
		server.HandleError(run, func(err error) { result.err = err })
	}()
	return result
}

func (self *accountOwnershipTestRun) join(t testing.TB, ctx context.Context) error {
	t.Helper()
	select {
	case <-self.done:
		return self.err
	case <-ctx.Done():
		t.Fatal("account ownership participant did not join", ctx.Err())
		return ctx.Err()
	}
}

func accountOwnershipTestWorker(ctx context.Context) *task.TaskWorker {
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	worker.AddTargets(NewLegacyProviderTotalsTaskTarget())
	return worker
}

func evalAccountOwnershipTestWorker(worker *task.TaskWorker, expected server.Id) {
	finished, retried, posts, err := worker.EvalTasks(1)
	server.Raise(err)
	if len(finished) != 1 || finished[0] != expected || len(retried)+len(posts) != 0 {
		panic(errors.New("registered provider did not finalize its exact owner once"))
	}
}

func accountOwnershipRequireFinished(t testing.TB, ctx context.Context, original map[server.Id]*task.Task) {
	t.Helper()
	ids := make([]server.Id, 0, len(original))
	for id := range original {
		ids = append(ids, id)
	}
	if len(task.GetTasks(ctx, ids...)) != 0 {
		t.Fatal("account ownership left a durable provider pending")
	}
	finished := task.GetFinishedTasks(ctx, ids...)
	if len(finished) != len(ids) {
		t.Fatal("account ownership lost a finished provider identity")
	}
	for id, before := range original {
		after := finished[id]
		if after == nil || !after.PostCompleted || after.ResultJson != "{}" || after.RunStartTime.IsZero() || after.RunEndTime.IsZero() || after.RunEndTime.Before(after.RunStartTime) {
			t.Fatal("account ownership lost exact durable handback")
		}
		want, err := decodeLegacyProviderTotals(before.ArgsJson)
		server.Raise(err)
		got, err := decodeLegacyProviderTotals(after.ArgsJson)
		server.Raise(err)
		want.Applied = true
		if !reflect.DeepEqual(want, got) {
			t.Fatal("account ownership changed immutable provider allocation")
		}
	}
}

// A durable network change between the unlocked admission read and BEGIN must
// refuse every credit. Exact amounts remain authoritative only after row lock.
func TestAccountBalanceOwnerRejectsChangedDurableProviderMembership(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		networkId, changedNetworkId := server.NewId(), server.NewId()
		id := providerTotalsTestTask(ctx, server.NewId(), networkId)
		providerTotalsBatchDue(ctx, []server.Id{id})
		var mutated atomic.Bool
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind != server.PgOwnershipAdmitted || !mutated.CompareAndSwap(false, true) {
				return
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json=jsonb_set(args_json::jsonb,'{totals,0,network_id}',to_jsonb($2::uuid))::text WHERE task_id=$1`, id, changedNetworkId))
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		worker := accountOwnershipTestWorker(observed)
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished)+len(posts) != 0 || len(retried) != 1 || retried[0] != id || !mutated.Load() {
			t.Fatal("changed membership did not retain its exact durable owner", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched bool
			server.Raise(conn.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM account_balance WHERE network_id=ANY($1))
              AND NOT (SELECT (args_json::jsonb->>'applied')::boolean FROM pending_task WHERE task_id=$2)`, []server.Id{networkId, changedNetworkId}, id).Scan(&untouched))
			if !untouched {
				t.Fatal("stale admitted keys authorized a different account write")
			}
		}, server.OptNoRetry())
	})
}

// The accounting commit is real and only its caller's reply is lost. The exact
// applied marker remains the authority for a subsequent registered task owner.
func TestAccountBalanceOwnerLostReplyRetainsExactProviderReplay(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		providerTotalsBatchWriteCounter(t, ctx)
		networkId := server.NewId()
		id := providerTotalsTestTask(ctx, server.NewId(), networkId)
		original := task.GetTasks(ctx, id)
		calls := 0
		err := runLegacyProviderTotalsTxWithOwner(ctx, func(tx server.PgTx) error {
			calls++
			return applyLegacyProviderTotalsWithOwnershipInTx(ctx, tx, id, []server.Id{networkId})
		}, func(ctx context.Context, callback func(server.PgTx), options ...any) {
			server.OwnedTx(ctx, accountBalanceOwnershipKeys([]server.Id{networkId}), callback, options...)
			panic(io.ErrUnexpectedEOF)
		})
		if !errors.Is(err, io.ErrUnexpectedEOF) || calls != 1 {
			t.Fatal("unknown caller reply repeated or hid provider accounting", err, calls)
		}
		var phase *legacyProviderTotalsPhaseError
		if !errors.As(err, &phase) || phase.phase != legacyProviderTotalsCommit {
			t.Fatal("lost reply changed commit-phase diagnostics", err)
		}
		requireProviderTotalsTestState(t, ctx, id, networkId, true, 17, 29)
		providerTotalsBatchDue(ctx, []server.Id{id})
		worker := accountOwnershipTestWorker(ctx)
		defer worker.Close()
		evalAccountOwnershipTestWorker(worker, id)
		accountOwnershipRequireFinished(t, ctx, original)
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count=17 AND provided_net_revenue_nano_cents=29
              AND (SELECT count(*) FROM test_provider_total_write WHERE network_id=$1)=1 FROM account_balance WHERE network_id=$1`, networkId).Scan(&exact))
			if !exact {
				t.Fatal("recovery repeated an acknowledged provider credit")
			}
		}, server.OptNoRetry())
	})
}
