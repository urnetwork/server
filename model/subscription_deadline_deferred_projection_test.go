package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

type deadlineDeferredProjection struct {
	sweptBytes, accountBytes, pendingBytes       ByteCount
	sweptRevenue, accountRevenue, pendingRevenue NanoCents
	ownerId                                      server.Id
	owners                                       int
}

// Financial conservation includes the exact unapplied payload. It never treats
// a terminal outcome, a task count, or a best-effort post as earned revenue.
func readDeadlineDeferredProjection(t testing.TB, ctx context.Context, networkId, contractId server.Id) (state deadlineDeferredProjection) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
			COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1 AND network_id=$2),0),
			COALESCE((SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=$1 AND network_id=$2),0),
			COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$2),0),
			COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$2),0)`,
			contractId, networkId).Scan(&state.sweptBytes, &state.sweptRevenue, &state.accountBytes, &state.accountRevenue))
		rows, err := conn.Query(ctx, `SELECT task_id,args_json FROM pending_task WHERE function_name=$1 AND run_once_key=$2`,
			task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName(), task.RunOnce("legacy_provider_totals", contractId).String())
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var data string
				server.Raise(rows.Scan(&state.ownerId, &data))
				payload, err := decodeLegacyProviderTotals(data)
				if err != nil || payload.ContractId != contractId {
					t.Fatal("deadline projection lost immutable identity", err)
				}
				state.owners++
				for _, total := range payload.Totals {
					if !payload.Applied && total.NetworkId == networkId {
						state.pendingBytes += total.Bytes
						state.pendingRevenue += total.Revenue
					}
				}
			}
		})
	}, server.OptNoRetry())
	return
}

func newDeadlineDeferredFixture(t testing.TB, ctx context.Context, redis bool) (netEscrowOrderingTestFixture, server.Id) {
	t.Helper()
	f := newNetEscrowOrderingTestFixture(t, ctx)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
	})
	var id server.Id
	if redis {
		id = createRedisAdmissionTest(ctx, f, 100).ContractId
	} else {
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		id = contract.ContractId
	}
	server.Raise(CloseContract(ctx, id, f.sourceId, 17, true))
	return f, id
}

func holdDeadlineDeferredOwner(t testing.TB, ctx context.Context, keys []server.PgOwnershipKey) func() {
	t.Helper()
	ready, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		var failure error
		server.HandleError(func() {
			server.OwnedTx(ctx, keys, func(tx server.PgTx) {
				close(ready)
				select {
				case <-release:
				case <-ctx.Done():
					server.Raise(ctx.Err())
				}
			}, server.TxReadCommitted, server.OptNoRetry())
		}, func(err error) { failure = err })
		done <- failure
	}()
	joined := false
	var once sync.Once
	finish := func() {
		once.Do(func() { close(release) })
		if !joined {
			failure := <-done
			joined = true
			if failure != nil {
				t.Error("deadline holder did not join cleanly", failure)
			}
		}
	}
	t.Cleanup(finish)
	select {
	case <-ready:
	case err := <-done:
		joined = true
		finish()
		t.Fatal("deadline holder failed admission", err)
	case <-ctx.Done():
		finish()
		t.Fatal("deadline holder did not start", ctx.Err())
	}
	return finish
}

// No projection worker runs before these assertions. Both the direct legacy
// debit and the native journal are exact even while the provider is held.
func requireDeadlineDeferredCommit(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, id server.Id, redis bool) {
	t.Helper()
	projection := readDeadlineDeferredProjection(t, ctx, f.destinationNetworkId, id)
	if projection.sweptBytes != 17 || projection.sweptRevenue != 17 || projection.accountBytes != 0 || projection.accountRevenue != 0 ||
		projection.pendingBytes != 17 || projection.pendingRevenue != 17 || projection.owners != 1 {
		t.Fatal("deadline did not commit exact earnings and immutable provider debt", projection.sweptBytes, projection.sweptRevenue, projection.pendingBytes, projection.pendingRevenue, projection.owners)
	}
	server.Db(ctx, func(conn server.PgConn) {
		var terminal, settled bool
		var balance, payout, pending ByteCount
		var journals, intents int
		server.Raise(conn.QueryRow(ctx, `SELECT outcome='settled' AND NOT dispute,
			(SELECT settled FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2),
			(SELECT payout_byte_count FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2),
			(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
			(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=$1),
			COALESCE((SELECT debit_byte_count FROM transfer_debit_journal WHERE contract_id=$1 AND balance_id=$2 AND NOT applied),0),
			(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=$1)
			FROM transfer_contract WHERE contract_id=$1`, id, f.balanceId).Scan(&terminal, &settled, &payout, &balance, &journals, &pending, &intents))
		wantBalance, wantPending, wantJournals := ByteCount(983), ByteCount(0), 0
		if redis {
			wantBalance, wantPending, wantJournals = 1000, 17, 1
		}
		if !terminal || !settled || payout != 17 || intents != 0 || balance != wantBalance || pending != wantPending || journals != wantJournals || balance-pending != 983 {
			t.Fatal("deadline lost terminal/debit conservation", terminal, settled, payout, balance, pending, journals, intents)
		}
	}, server.OptNoRetry())
	if redis {
		if reserved := Testing_NetEscrowByteCount(ctx, f.balanceId); reserved != 17 {
			t.Fatal("native deadline released consumed reservation before debit", reserved)
		}
	}
}

func drainDeadlineDeferredCommit(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, id server.Id, redis bool) {
	t.Helper()
	state := readDeadlineDeferredProjection(t, ctx, f.destinationNetworkId, id)
	stored := task.GetTasks(ctx, state.ownerId)[state.ownerId]
	if stored == nil {
		t.Fatal("deadline projection lost durable owner")
	}
	target := task.NewTaskTarget(ApplyLegacyProviderTotals)
	for range 2 {
		_, _, err := target.RunSpecific(ctx, stored)
		server.Raise(err)
	}
	state = readDeadlineDeferredProjection(t, ctx, f.destinationNetworkId, id)
	if state.accountBytes != 17 || state.accountRevenue != 17 || state.pendingBytes != 0 || state.pendingRevenue != 0 || state.owners != 1 {
		t.Fatal("provider application or stale replay changed exact earnings")
	}
	providerTotalsBatchDue(ctx, []server.Id{state.ownerId})
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	defer worker.Close()
	worker.AddTargets(NewLegacyProviderTotalsTaskTarget())
	finished, retried, posts, err := worker.EvalTasks(1)
	if err != nil || len(finished) != 1 || finished[0] != state.ownerId || len(retried)+len(posts) != 0 {
		t.Fatal("provider owner did not finalize exactly", err)
	}
	applied, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
	want := 0
	if redis {
		want = 1
	}
	if err != nil || busy || applied != want || released != want {
		t.Fatal("debit worker did not consume exact journal", applied, released, busy, err)
	}
	if applied, released, busy, err = flushTransferDebitBalance(ctx, f.balanceId); err != nil || busy || applied != 0 || released != 0 {
		t.Fatal("debit replay repeated charge", err)
	}
	requireLegacySettlementTestState(t, ctx, f, id, false, true, 983, 0)
	state = readDeadlineDeferredProjection(t, ctx, f.destinationNetworkId, id)
	if state.accountBytes != 17 || state.accountRevenue != 17 || state.sweptBytes != 17 || state.sweptRevenue != 17 || state.owners != 0 {
		t.Fatal("projection completion lost funded earnings")
	}
	before := readRedisExpiryRepairTestState(ctx, id)
	result, err := ReconcileContractAtDeadline(ctx, id, server.NowUtc())
	if err != nil || result == nil || !result.AlreadyClosed || result.Charged != 0 || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
		t.Fatal("terminal replay changed finances", err)
	}
	if got := readDeadlineDeferredProjection(t, ctx, f.destinationNetworkId, id); got != state {
		t.Fatal("terminal replay republished provider allocation")
	}
}

func TestDeadlineDeferredProviderOwnerDoesNotBlockTerminalCommit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(WithProviderWorkSessionSource(t.Context(), nil), 45*time.Second)
		defer cancel()
		for _, redis := range []bool{false, true} {
			f, id := newDeadlineDeferredFixture(t, ctx, redis)
			held := accountBalanceOwnershipKeys([]server.Id{f.destinationNetworkId})
			release := holdDeadlineDeferredOwner(t, ctx, held)
			defer release()
			var refused, uncertain, reruns, accountAdmission atomic.Int64
			observed := server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
			observed = server.Testing_WithPgOwnershipObservation(observed, func(event server.PgOwnershipEvent) {
				if event.Kind == server.PgOwnershipRefused {
					refused.Add(1)
				}
				if event.Kind == server.PgOwnershipUncertain {
					uncertain.Add(1)
				}
				if event.Kind == server.PgOwnershipAdmitted && slices.Contains(event.Keys, held[0]) {
					accountAdmission.Add(1)
				}
			})
			result, err := ReconcileContractAtDeadline(observed, id, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
			if err != nil || result == nil || result.Charged != 17 || refused.Load()+uncertain.Load()+reruns.Load()+accountAdmission.Load() != 0 {
				t.Fatal("held provider blocked deadline settlement", redis, err)
			}
			requireDeadlineDeferredCommit(t, ctx, f, id, redis)
			release()
			drainDeadlineDeferredCommit(t, ctx, f, id, redis)
		}
	})
}

// A grant conflict is resolved by the existing pre-BEGIN owner. No contract or
// intent row is retained while waiting, and the same invocation completes once.
func TestDeadlineDeferredGrantAdmissionPrecedesBusinessRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(WithProviderWorkSessionSource(t.Context(), nil), 45*time.Second)
		defer cancel()
		f, id := newDeadlineDeferredFixture(t, ctx, true)
		held := transferBalanceOwnershipKeys([]server.Id{f.balanceId})
		release := holdDeadlineDeferredOwner(t, ctx, held)
		defer release()
		waiting := make(chan struct{})
		var once sync.Once
		var refused, uncertain, reruns atomic.Int64
		observed := server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		observed = server.Testing_WithPgOwnershipObservation(observed, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipWaiting && slices.Contains(event.Keys, held[0]) {
				once.Do(func() { close(waiting) })
			}
			if event.Kind == server.PgOwnershipRefused {
				refused.Add(1)
			}
			if event.Kind == server.PgOwnershipUncertain {
				uncertain.Add(1)
			}
		})
		type completion struct {
			result *ContractDeadlineReconciliation
			err    error
		}
		done := make(chan completion, 1)
		go func() { r, e := ReconcileContractAtDeadline(observed, id, server.NowUtc()); done <- completion{r, e} }()
		joined := false
		defer func() {
			release()
			if !joined {
				<-done
			}
		}()
		select {
		case <-waiting:
		case got := <-done:
			joined = true
			t.Fatal("held grant did not enter bounded pre-BEGIN wait", got.err)
		case <-ctx.Done():
			t.Fatal("deadline never reached grant admission", ctx.Err())
		}
		server.Tx(ctx, func(tx server.PgTx) {
			var terminal bool
			server.Raise(tx.QueryRow(ctx, `SELECT outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1 FOR UPDATE NOWAIT`, id).Scan(&terminal))
			if terminal {
				t.Fatal("waiting close already changed contract")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		release()
		got := <-done
		joined = true
		if got.err != nil || got.result == nil || got.result.Charged != 17 || refused.Load()+uncertain.Load()+reruns.Load() != 0 {
			t.Fatal("admitted close replayed or refused funded settlement", got.err)
		}
		requireDeadlineDeferredCommit(t, ctx, f, id, true)
		drainDeadlineDeferredCommit(t, ctx, f, id, true)
	})
}

// A failed immutable publication rolls back the outcome, journal, metadata and
// sweeps. A preexisting task never gets silently merged or replaced.
func TestDeadlineDeferredPublicationFailureRollsBackAllAccounting(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f, id := newDeadlineDeferredFixture(t, ctx, true)
		collision := providerTotalsTestTask(ctx, id, f.destinationNetworkId)
		priorTask := task.GetTasks(ctx, collision)[collision]
		prior, err := json.Marshal(priorTask)
		server.Raise(err)
		before := readRedisExpiryRepairTestState(ctx, id)
		result, err := ReconcileContractAtDeadline(ctx, id, server.NowUtc())
		if err == nil || result != nil || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
			t.Fatal("publication refusal committed deadline prefix", err)
		}
		after, err := json.Marshal(task.GetTasks(ctx, collision)[collision])
		server.Raise(err)
		if !bytes.Equal(prior, after) {
			t.Fatal("deadline replaced immutable provider allocation")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched bool
			server.Raise(conn.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
				AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
				AND (SELECT balance_byte_count=1000 FROM transfer_balance WHERE balance_id=$2)`, id, f.balanceId).Scan(&untouched))
			if !untouched {
				t.Fatal("failed publication lost debit or earnings rollback")
			}
		})
	})
}

// Commit the exact body and deliberately lose every optional post and its
// caller acknowledgement. The replay fence, debit and earnings are all SQL
// authority; the original full Redis reservation remains conservative until
// the debit worker releases it after applying the recorded consumption.
func TestDeadlineDeferredLostPostsKeepExactDebitAndEarnings(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f, id := newDeadlineDeferredFixture(t, ctx, true)
		server.Tx(ctx, func(tx server.PgTx) {
			result, posts := reconcileContractAtDeadlineInTx(ctx, tx, id, server.NowUtc())
			if result == nil || result.Charged != 17 || result.Outcome != ContractOutcomeSettled || len(posts) == 0 {
				t.Fatal("lost-post fixture did not commit the real settlement")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		projection := readDeadlineDeferredProjection(t, ctx, f.destinationNetworkId, id)
		if projection.sweptBytes != 17 || projection.sweptRevenue != 17 || projection.accountBytes != 0 || projection.pendingBytes != 17 || projection.pendingRevenue != 17 || projection.owners != 1 {
			t.Fatal("lost posts lost durable earnings")
		}
		if reserved := Testing_NetEscrowByteCount(ctx, f.balanceId); reserved != 100 {
			t.Fatal("lost-post fixture released its reservation without a debit", reserved)
		}
		before := readRedisExpiryRepairTestState(ctx, id)
		result, err := ReconcileContractAtDeadline(ctx, id, server.NowUtc())
		if err != nil || result == nil || !result.AlreadyClosed || result.Charged != 0 || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
			t.Fatal("lost acknowledgement replay repeated settlement", err)
		}
		if after := readDeadlineDeferredProjection(t, ctx, f.destinationNetworkId, id); after != projection {
			t.Fatal("lost acknowledgement changed immutable earnings")
		}
		drainDeadlineDeferredCommit(t, ctx, f, id, true)
	})
}

// Preflight is never financial authority. Add a new reservation after admission
// and require one explicit scope failure without acquiring another business key.
func TestDeadlineDeferredChangedScopeRefusesWithoutReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f, id := newDeadlineDeferredFixture(t, ctx, true)
		additional := server.NewId()
		var once sync.Once
		var reruns atomic.Int64
		observed := server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		observed = server.Testing_WithPgOwnershipObservation(observed, func(event server.PgOwnershipEvent) {
			if event.Kind != server.PgOwnershipAdmitted || event.TransactionScoped {
				return
			}
			once.Do(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count,redis_reserved) VALUES($1,$2,1,true)`, id, additional))
				}, server.TxReadCommitted, server.OptNoRetry())
			})
		})
		result, err := ReconcileContractAtDeadline(observed, id, server.NowUtc())
		if result != nil || err == nil || err.Error() != "deadline financial ownership changed before transaction" || errors.Is(err, context.DeadlineExceeded) || reruns.Load() != 0 {
			t.Fatal("changed scope escaped exact admitted set", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND (SELECT balance_byte_count=1000 FROM transfer_balance WHERE balance_id=$2)
				AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
				AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
				FROM transfer_contract WHERE contract_id=$1`, id, f.balanceId).Scan(&untouched))
			if !untouched {
				t.Fatal("scope refusal changed financial state")
			}
		})
	})
}
