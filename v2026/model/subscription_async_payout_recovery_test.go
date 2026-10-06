// Settlement crash boundaries retain earned payouts without serializing payer
// grants or provider totals. Every barrier owns a real transaction or callback.
package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

// Make one byte of eligible service worth one nano-cent, with synthetic owners.
func asyncPayoutRecoveryFixture(t testing.TB, ctx context.Context) netEscrowOrderingTestFixture {
	t.Helper()
	f := newNetEscrowOrderingTestFixture(t, ctx)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
	})
	return f
}

// Reports commit separately, exactly as the foreground close owner expects.
func asyncPayoutRecoveryReports(ctx context.Context, contractId server.Id, amount ByteCount) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=true WHERE contract_id=$1`, contractId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
			VALUES($1,'source',$2,clock_timestamp() AT TIME ZONE 'UTC',false),($1,'destination',$2,clock_timestamp() AT TIME ZONE 'UTC',false)`, contractId, amount))
	})
}

// Inspect durable financial ownership without running or reconstructing posts.
func asyncPayoutRecoveryState(t testing.TB, ctx context.Context, contractId server.Id) (terminal bool, journals, sweeps, owners int, earned ByteCount, revenue NanoCents) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1),
			(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=$1),
			(SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=$1),
			(SELECT count(*) FROM pending_task WHERE function_name=$2 AND args_json::jsonb->>'contract_id'=$1::text),
			COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1),0),
			COALESCE((SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=$1),0)`, contractId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).
			Scan(&terminal, &journals, &sweeps, &owners, &earned, &revenue))
	})
	return
}

// The durable task's row is the restart authority; captured callbacks are absent.
func asyncPayoutRecoveryProject(t testing.TB, ctx context.Context, contractId server.Id) server.Id {
	t.Helper()
	var taskId server.Id
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE function_name=$2 AND args_json::jsonb->>'contract_id'=$1::text`, contractId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(&taskId))
	})
	for range 2 {
		server.Tx(ctx, func(tx server.PgTx) { server.Raise(applyLegacyProviderTotalsInTx(ctx, tx, taskId)) })
	}
	return taskId
}

// Commit is the explicit crash barrier: no callback runs before durable restart.
func TestRedisSettlementCrashRetainsEarnedPayoutAndProjection(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		f := asyncPayoutRecoveryFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 64)
		_ = createRedisAdmissionTest(ctx, f, 37)
		asyncPayoutRecoveryReports(ctx, contract.ContractId, 11)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		defer tx.Rollback(context.Background())
		posts, closed, err := settleEscrowForegroundInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
		server.Raise(err)
		if !closed || len(posts) == 0 {
			t.Fatal("foreground owner did not reach the callback boundary")
		}
		terminal, journals, sweeps, owners, _, _ := asyncPayoutRecoveryState(t, ctx, contract.ContractId)
		if terminal || journals+sweeps+owners != 0 {
			t.Fatal("uncommitted settlement escaped the transaction")
		}
		server.Raise(tx.Commit(ctx))
		// Simulate process loss after commit by handing no callback to recovery.
		terminal, journals, sweeps, owners, earned, revenue := asyncPayoutRecoveryState(t, ctx, contract.ContractId)
		if !terminal || journals != 1 || sweeps != 1 || owners != 1 || earned != 11 || revenue != 11 {
			t.Fatalf("committed settlement lost payout ownership before callbacks: terminal=%t journals=%d sweeps=%d owners=%d earned=%d revenue=%d", terminal, journals, sweeps, owners, earned, revenue)
		}
		credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 1000 || pending != 1 || applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 101 {
			t.Fatal("foreground settlement consumed the grant or released its reservation")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			retryPosts, again, err := settleEscrowForegroundInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
			server.Raise(err)
			if again || len(retryPosts) != 0 {
				t.Fatal("lost commit reply allocated a second settlement")
			}
		})
		taskId := asyncPayoutRecoveryProject(t, ctx, contract.ContractId)
		requireProviderTotalsTestState(t, ctx, taskId, f.destinationNetworkId, true, 11, 11)
		for range 2 {
			_, _, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
			server.Raise(err)
			if busy {
				t.Fatal("uncontended durable debit owner refused")
			}
		}
		credit, pending, applied = asyncDebitTestState(t, ctx, f.balanceId)
		var settled bool
		var consumption ByteCount
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT settled,payout_byte_count FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2`, contract.ContractId, f.balanceId).Scan(&settled, &consumption))
		})
		if credit != 989 || pending+applied != 0 || !settled || consumption != 11 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 37 {
			t.Fatal("restart lost consumption, metadata, or the healthy reservation")
		}
		// A delayed callback from another admitted invocation is also harmless.
		server.RunPosts(ctx, posts...)
		server.RunPosts(ctx, posts...)
		requireProviderTotalsTestState(t, ctx, taskId, f.destinationNetworkId, true, 11, 11)
		terminal, journals, sweeps, owners, earned, revenue = asyncPayoutRecoveryState(t, ctx, contract.ContractId)
		if !terminal || journals != 0 || sweeps != 1 || owners != 1 || earned != 11 || revenue != 11 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 37 {
			t.Fatal("late callback replay changed durable allocation or the neighbor")
		}
	})
}

// Rollback must discard every required financial owner, then admit one retry.
func TestRedisSettlementRollbackKeepsPayoutAndProjectionAtomic(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		f := asyncPayoutRecoveryFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 64)
		asyncPayoutRecoveryReports(ctx, contract.ContractId, 11)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		_, closed, err := settleEscrowForegroundInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
		server.Raise(err)
		if !closed {
			t.Fatal("rollback never reached the financial owner")
		}
		server.Raise(tx.Rollback(ctx))
		terminal, journals, sweeps, owners, earned, revenue := asyncPayoutRecoveryState(t, ctx, contract.ContractId)
		credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if terminal || journals+sweeps+owners != 0 || earned != 0 || revenue != 0 || credit != 1000 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 64 {
			t.Fatal("rollback leaked financial state")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, closed, err = settleEscrowForegroundInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
			server.Raise(err)
			if !closed {
				t.Fatal("rolled-back settlement lost retry authority")
			}
		})
		terminal, journals, sweeps, owners, earned, revenue = asyncPayoutRecoveryState(t, ctx, contract.ContractId)
		if !terminal || journals != 1 || sweeps != 1 || owners != 1 || earned != 11 || revenue != 11 {
			t.Fatal("retry did not commit one exact allocation")
		}
	})
}

// An actual over-capacity report retains all liability while a neighbor settles.
func TestRedisSettlementInsufficientEscrowRetainsLiabilityAndNeighbor(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		f := asyncPayoutRecoveryFixture(t, ctx)
		held := createRedisAdmissionTest(ctx, f, 64)
		neighbor := createRedisAdmissionTest(ctx, f, 37)
		asyncPayoutRecoveryReports(ctx, held.ContractId, 80)
		asyncPayoutRecoveryReports(ctx, neighbor.ContractId, 11)
		reports := legacyDrainTestReports(ctx, held.ContractId)
		server.Tx(ctx, func(tx server.PgTx) {
			posts, closed, err := settleEscrowForegroundInTx(ctx, tx, held.ContractId, ContractOutcomeSettled)
			if !errors.Is(err, errContractInsufficientEscrow) || closed || len(posts) != 0 {
				t.Fatal("underfunded usage was converted into settlement")
			}
		})
		terminal, journals, sweeps, owners, _, _ := asyncPayoutRecoveryState(t, ctx, held.ContractId)
		if terminal || journals+sweeps+owners != 0 || string(reports) != string(legacyDrainTestReports(ctx, held.ContractId)) {
			t.Fatal("refusal lost original reports or financial liability")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, closed, err := settleEscrowForegroundInTx(ctx, tx, neighbor.ContractId, ContractOutcomeSettled)
			server.Raise(err)
			if !closed {
				t.Fatal("accounting hold blocked a healthy neighbor")
			}
		})
		taskId := asyncPayoutRecoveryProject(t, ctx, neighbor.ContractId)
		requireProviderTotalsTestState(t, ctx, taskId, f.destinationNetworkId, true, 11, 11)
		_, _, _, err := flushTransferDebitBalance(ctx, f.balanceId)
		server.Raise(err)
		credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 989 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 64 {
			t.Fatal("healthy recovery consumed or released the accounting hold")
		}
	})
}

// Two close transactions reach their pre-commit barriers while another owner
// holds all shared payer and provider rows. No timing-based absence is evidence.
func TestRedisSettlementPayoutOwnersDoNotLockSharedFinancialRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		f := asyncPayoutRecoveryFixture(t, ctx)
		first := createRedisAdmissionTest(ctx, f, 64)
		second := createRedisAdmissionTest(ctx, f, 37)
		asyncPayoutRecoveryReports(ctx, first.ContractId, 11)
		asyncPayoutRecoveryReports(ctx, second.ContractId, 17)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance_net_escrow_revision(balance_id,revision) VALUES($1,1) ON CONFLICT DO NOTHING`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance_net_escrow_snapshot(balance_id,revision,reserved_byte_count) VALUES($1,1,0) ON CONFLICT DO NOTHING`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_balance(network_id) VALUES($1) ON CONFLICT DO NOTHING`, f.destinationNetworkId))
		})
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		for _, table := range []string{"transfer_balance", "transfer_balance_net_escrow_revision", "transfer_balance_net_escrow_snapshot"} {
			server.RaisePgResult(held.Exec(ctx, fmt.Sprintf("SELECT balance_id FROM %s WHERE balance_id=$1 FOR UPDATE", table), f.balanceId))
		}
		server.RaisePgResult(held.Exec(ctx, `SELECT network_id FROM account_balance WHERE network_id=$1 FOR UPDATE`, f.destinationNetworkId))
		ready := make(chan struct{}, 2)
		release := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		done := make(chan any, 2)
		var workers sync.WaitGroup
		defer func() { cancel(); unblock(); workers.Wait() }()
		for _, id := range []server.Id{first.ContractId, second.ContractId} {
			workers.Add(1)
			go func() {
				defer workers.Done()
				done <- server.HandleError(func() {
					server.Tx(ctx, func(tx server.PgTx) {
						_, closed, err := settleEscrowForegroundInTx(ctx, tx, id, ContractOutcomeSettled)
						server.Raise(err)
						if !closed {
							server.Raise(errors.New("concurrent close did not own outcome"))
						}
						ready <- struct{}{}
						select {
						case <-release:
						case <-ctx.Done():
							server.Raise(ctx.Err())
						}
					}, server.TxReadCommitted, server.OptNoRetry())
				})
			}()
		}
		for range 2 {
			select {
			case <-ready:
			case err := <-done:
				t.Fatalf("close failed before the owned commit barrier: %v", err)
			case <-ctx.Done():
				t.Fatal("close could not reach the owned commit barrier", ctx.Err())
			}
		}
		unblock()
		for range 2 {
			if err := <-done; err != nil {
				t.Fatal("concurrent financial commit failed", err)
			}
		}
		server.Raise(held.Rollback(ctx))
		for _, id := range []server.Id{first.ContractId, second.ContractId} {
			terminal, journals, sweeps, owners, _, _ := asyncPayoutRecoveryState(t, ctx, id)
			if !terminal || journals != 1 || sweeps != 1 || owners != 1 {
				t.Fatal("concurrent close lost its independent durable owners")
			}
			asyncPayoutRecoveryProject(t, ctx, id)
		}
		_, _, _, err = flushTransferDebitBalance(ctx, f.balanceId)
		server.Raise(err)
		credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 972 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 0 {
			t.Fatal("concurrent close or replay changed payer conservation")
		}
	})
}
