package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

func legacySettlementTestIntent(t testing.TB, ctx context.Context) (netEscrowOrderingTestFixture, server.Id) {
	t.Helper()
	f := newNetEscrowOrderingTestFixture(t, ctx)
	e, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
	server.RunPosts(ctx, posts...)
	server.Raise(CloseContract(ctx, e.ContractId, f.sourceId, 11, false))
	server.Raise(CloseContract(ctx, e.ContractId, f.destinationId, 11, false))
	return f, e.ContractId
}

func requireLegacySettlementTestState(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, id server.Id, pending, terminal bool, credit, reserved ByteCount) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var gotPending, gotTerminal, settled bool
		var gotCredit ByteCount
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1),
          outcome IS NOT NULL,(SELECT settled FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$2),
          (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)
          FROM transfer_contract WHERE contract_id=$1`, id, f.balanceId).Scan(&gotPending, &gotTerminal, &settled, &gotCredit))
		if gotPending != pending || gotTerminal != terminal || settled != terminal || gotCredit != credit {
			t.Fatalf("legacy state pending=%t terminal=%t metadata=%t credit=%d", gotPending, gotTerminal, settled, gotCredit)
		}
	})
	if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != reserved {
		t.Fatalf("legacy reservation=%d want=%d", got, reserved)
	}
}

// Account totals are now projected separately from the financial commit. Tests
// that inspect the applied account explicitly run the real task target first;
// this helper leaves each applied pending row for the worker lifecycle controls.
func projectLegacyProviderTotalsForTest(t testing.TB, ctx context.Context) {
	t.Helper()
	target := task.NewTaskTarget(ApplyLegacyProviderTotals)
	taskIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT task_id FROM pending_task
            WHERE function_name=$1 AND NOT COALESCE((args_json::jsonb->>'applied')::boolean,false)
            ORDER BY task_id`, target.TargetFunctionName())
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var taskId server.Id
				server.Raise(rows.Scan(&taskId))
				taskIds = append(taskIds, taskId)
			}
		})
	})
	pending := task.GetTasks(ctx, taskIds...)
	for _, taskId := range taskIds {
		owner := pending[taskId]
		if owner == nil {
			t.Fatal("legacy provider projection lost its pending owner")
		}
		if _, _, err := target.RunSpecific(ctx, owner); err != nil {
			t.Fatalf("legacy provider total projection failed: %v", err)
		}
	}
}

// This remains assertion-only: lost acknowledgements and omitted posts must
// leave exact totals durable without executing their projection. One statement
// observes the account plus unapplied payloads consistently across an apply commit.
func requireLegacyProviderDurability(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, id server.Id, bytes int64, expectedRevenue ...int64) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var swept, provided, sweptRevenue, providedRevenue int64
		var pendingBytes, pendingRevenue int64
		server.Raise(conn.QueryRow(ctx, `WITH unapplied AS (
            SELECT allocation
            FROM pending_task
            CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
            WHERE function_name=$3 AND (args_json::jsonb->>'applied')::boolean=false
                AND (allocation->>'network_id')::uuid=$2
        ) SELECT COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep
            WHERE contract_id=$1 AND network_id=$2),0),COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$2),0),
            COALESCE((SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=$1 AND network_id=$2),0),
            COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$2),0),
            COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0),
            COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)`,
			id, f.destinationNetworkId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(
			&swept, &provided, &sweptRevenue, &providedRevenue, &pendingBytes, &pendingRevenue))
		revenue := int64(0)
		if len(expectedRevenue) > 0 {
			revenue = expectedRevenue[0]
		}
		if swept != bytes || provided+pendingBytes != bytes || sweptRevenue != revenue || providedRevenue+pendingRevenue != revenue {
			t.Fatalf("durable provider accounting swept=%d provided=%d pending=%d revenue=%d/%d/%d want=%d/%d", swept, provided, pendingBytes, sweptRevenue, providedRevenue, pendingRevenue, bytes, revenue)
		}
	})
	server.Redis(ctx, func(r server.RedisClient) {
		if value := r.Get(ctx, accountBalanceNetPayoutByteCountKey(f.destinationNetworkId)).Val(); value != "" && value != "0" {
			t.Fatal("worker duplicated its durable provider contribution in Redis")
		}
	})
}

func TestLegacySettlementRollbackAndLostAcknowledgement(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
		})
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		_, complete, busy, _, err := flushLegacySettlementInTx(ctx, tx, id)
		server.Raise(err)
		if !complete || busy {
			t.Fatal("rollback transaction never reached its outcome")
		}
		server.Raise(tx.Rollback(ctx))
		requireLegacyProviderDurability(t, ctx, f, id, 0)
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
		// Model a committed transaction whose caller never receives its reply
		// and never starts the post callbacks or provider projection. Sweeps,
		// exact queued totals, payer debit and the terminal outcome are durable.
		tx, err = conn.Begin(ctx)
		server.Raise(err)
		posts, complete, busy, _, err := flushLegacySettlementInTx(ctx, tx, id)
		server.Raise(err)
		if !complete || busy {
			t.Fatal("commit owner did not settle")
		}
		server.Raise(tx.Commit(ctx))
		requireLegacyProviderDurability(t, ctx, f, id, 11, 11)
		complete, busy, _, err = flushLegacySettlement(ctx, id)
		if err != nil || complete {
			t.Fatal("lost-ack replay repeated a financial transition", complete, busy, err)
		}
		ReconcileNetEscrowForNetwork(ctx, f.sourceNetworkId, true)
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
		server.RunPosts(ctx, posts...)
		requireLegacyProviderDurability(t, ctx, f, id, 11, 11)
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
	})
}

func TestLegacySettlementOldWriterAndWorkerCannotDoubleDebit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		old := func(id server.Id) {
			var posts []func() any
			server.Tx(ctx, func(tx server.PgTx) {
				var err error
				posts, _, err = settleEscrowInTx(ctx, tx, id, ContractOutcomeSettled)
				server.Raise(err)
			}, server.TxReadCommitted, server.OptNoRetry())
			server.RunPosts(ctx, posts...)
		}
		recovered := captureShardQueryPanic(func() { old(id) })
		err, _ := recovered.(error)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatal("old writer bypassed pending financial owner")
		}
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
		complete, busy, _, err := flushLegacySettlement(ctx, id)
		if err != nil || busy || !complete {
			t.Fatal("worker failed mixed-version handoff", complete, busy, err)
		}
		old(id)
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)

		// The old writer can still win BEFORE the intent exists. A later
		// foreground request observes its terminal outcome and queues nothing.
		other := newNetEscrowOrderingTestFixture(t, ctx)
		e, posts := createNetEscrowOrderingTestContract(ctx, other, 100)
		server.RunPosts(ctx, posts...)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
                VALUES($1,'source',11,now(),false),($1,'destination',11,now(),false)`, e.ContractId))
		})
		old(e.ContractId)
		server.Tx(ctx, func(tx server.PgTx) {
			_, _, err := settleEscrowForegroundInTx(ctx, tx, e.ContractId, ContractOutcomeSettled)
			server.Raise(err)
		})
		requireLegacySettlementTestState(t, ctx, other, e.ContractId, false, true, 989, 0)
	})
}

func TestLegacySettlementPendingProtectsOldAndCurrentRetention(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		// An old retention statement must roll back ALL dependent deletes if
		// its final contract delete encounters the pending-intent foreign key.
		recovered := captureShardQueryPanic(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `WITH deleted_close AS (DELETE FROM contract_close WHERE contract_id=$1),
                  deleted_escrow AS (DELETE FROM transfer_escrow WHERE contract_id=$1)
                  DELETE FROM transfer_contract WHERE contract_id=$1`, id))
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		err, _ := recovered.(error)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Fatalf("old retention bypassed pending contract: %v", recovered)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=clock_timestamp() AT TIME ZONE 'UTC'-interval '1 day' WHERE balance_id=$1`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time='2010-01-01',reap_time='2011-01-01' WHERE contract_id=$1`, id))
		})
		removeCompletedTransferBalanceBatch(ctx, []server.Id{f.balanceId}, server.NowUtc())
		removeDueContractBatches(ctx, server.NowUtc(), server.NowUtc().Add(-300*24*time.Hour), 128)
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
		complete, busy, _, err := flushLegacySettlement(ctx, id)
		if err != nil || busy || !complete {
			t.Fatal("retained intent lost financial inputs", complete, busy, err)
		}
		removeCompletedTransferBalanceBatch(ctx, []server.Id{f.balanceId}, server.NowUtc())
		server.Db(ctx, func(conn server.PgConn) {
			var found bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id=$1)`, f.balanceId).Scan(&found))
			if found {
				t.Fatal("completed legacy debit permanently pinned its expired balance")
			}
		})
	})
}

func TestLegacySettlementDisputeRejectionStaysReservedAndDeferred(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		e, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		server.Raise(CloseContract(ctx, e.ContractId, f.sourceId, 0, false))
		server.Raise(CloseContract(ctx, e.ContractId, f.destinationId, 4*AcceptableTransfersByteDifference, false))
		aged := server.NowUtc().Add(-2 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, e.ContractId, aged))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, e.ContractId, aged))
		})
		closed, err := ForceCloseOpenContractIds(ctx, aged.Add(time.Hour), 10, 1, 0, 0)
		if err != nil || closed != 0 {
			t.Fatal("queued legacy dispute was counted as a verified close", closed, err)
		}
		requireLegacySettlementTestState(t, ctx, f, e.ContractId, true, false, 1000, 100)
		result, err := FlushLegacySettlements(ctx, int(e.ContractId[15])%LegacySettlementShardCount, nil, 64)
		if err != nil || result.Failed != 1 || result.Completed != 0 {
			t.Fatal("accounting rejection was not retained as a failed intent", result, err)
		}
		requireLegacySettlementTestState(t, ctx, f, e.ContractId, true, false, 1000, 100)
		server.Db(ctx, func(conn server.PgConn) {
			var dispute bool
			var code string
			var next time.Time
			server.Raise(conn.QueryRow(ctx, `SELECT c.dispute,i.failure_code,i.next_attempt_time FROM legacy_settlement_intent i JOIN transfer_contract c USING(contract_id) WHERE contract_id=$1`, e.ContractId).Scan(&dispute, &code, &next))
			if !dispute || code != "accounting" || next.Before(server.NowUtc().Add(14*time.Minute)) {
				t.Fatal("failed worker cleared dispute or enabled a hot retry", dispute, code)
			}
		})
		result, err = FlushLegacySettlements(ctx, int(e.ContractId[15])%LegacySettlementShardCount, nil, 64)
		if err != nil || result.Visited != 0 {
			t.Fatal("unchanged rejected accounting was blindly retried", result, err)
		}
	})
}

func TestLegacySettlementBusyCursorAndConcurrentWorkers(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		first, firstId := legacySettlementTestIntent(t, ctx)
		second := newNetEscrowOrderingTestFixture(t, ctx)
		e, posts := createNetEscrowOrderingTestContract(ctx, second, 100)
		server.RunPosts(ctx, posts...)
		// Put independent synthetic contracts in the same partition before any
		// close/intent exists, so the cursor boundary is deterministic.
		secondId := e.ContractId
		secondId[15] = firstId[15]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET contract_id=$2 WHERE contract_id=$1`, e.ContractId, secondId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET contract_id=$2 WHERE contract_id=$1`, e.ContractId, secondId))
		})
		server.Raise(CloseContract(ctx, secondId, second.sourceId, 11, false))
		server.Raise(CloseContract(ctx, secondId, second.destinationId, 11, false))
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, first.balanceId))
		shard := int(firstId[15]) % 16
		result, err := FlushLegacySettlements(ctx, shard, nil, 1)
		if err != nil || result.BusyOrGone != 1 || result.Cursor == nil || result.Cursor.ContractId != firstId || !result.More {
			t.Fatal("busy owner did not advance bounded cursor", result, err)
		}
		next, err := FlushLegacySettlements(ctx, shard, result.Cursor, 1)
		if err != nil || next.Completed != 1 || next.Cursor == nil || next.Cursor.ContractId != secondId {
			t.Fatal("busy payer starved later payer", next, err)
		}
		wrapped, err := FlushLegacySettlements(ctx, shard, next.Cursor, 1)
		if err != nil || wrapped.Cursor != nil {
			t.Fatal("completed page did not wrap", wrapped, err)
		}
		server.Raise(held.Rollback(ctx))
		// Two actual independent transactions compete for the same intent.
		type completion struct {
			complete, busy bool
			err            error
		}
		start := make(chan struct{})
		done := make(chan completion, 2)
		for range 2 {
			go func() { <-start; a, b, _, e := flushLegacySettlement(ctx, firstId); done <- completion{a, b, e} }()
		}
		close(start)
		completed := 0
		for range 2 {
			r := <-done
			if r.err != nil {
				t.Fatal(r.err)
			}
			if r.complete {
				completed++
			}
		}
		if completed != 1 {
			t.Fatal("competing workers did not have one financial owner", completed)
		}
		requireLegacySettlementTestState(t, ctx, first, firstId, false, true, 989, 0)
		requireLegacySettlementTestState(t, ctx, second, secondId, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, first, firstId, 11)
		requireLegacyProviderDurability(t, ctx, second, secondId, 11)
	})
}

func TestLegacySettlementCancellationAndConflictingIntent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		var conflict error
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, id))
			conflict = queueLegacySettlementInTx(ctx, tx, id, ContractOutcomeDisputeResolvedToDestination, false)
		})
		if conflict == nil {
			t.Fatal("accepted intent was overwritten")
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, _, _, err := flushLegacySettlement(canceled, id)
		if err == nil {
			t.Fatal("canceled worker acknowledged financial work")
		}
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
		server.Db(ctx, func(conn server.PgConn) {
			var outcome string
			server.Raise(conn.QueryRow(ctx, `SELECT outcome FROM legacy_settlement_intent WHERE contract_id=$1`, id).Scan(&outcome))
			if outcome != "settled" {
				t.Fatal("conflicting intent changed accepted outcome")
			}
		})
		completed, busy, _, err := flushLegacySettlement(ctx, id)
		if err != nil || busy || !completed {
			t.Fatal("healthy successor could not recover cancellation", completed, busy, err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
	})
}

func TestLegacySettlementPartialGrantLockAndMissingUnusedGrant(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		second, missing := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents)
     VALUES($1,$2,now()-interval '1 hour',now()+interval '30 minutes',1000,1000,4000)`, second, f.sourceNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,100),($1,$3,4096)`, id, second, missing))
		})
		ReconcileNetEscrowForNetwork(ctx, f.sourceNetworkId, true)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.balanceId))
		completed, busy, _, err := flushLegacySettlement(ctx, id)
		if err != nil || completed || !busy {
			t.Fatal("partial grant ownership did not defer without mutation", completed, busy, err)
		}
		requireLegacyProviderDurability(t, ctx, f, id, 0)
		server.Raise(held.Rollback(ctx))
		completed, busy, _, err = flushLegacySettlement(ctx, id)
		if err != nil || !completed || busy {
			t.Fatal("missing unused grant changed original funding policy", completed, busy, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var firstCredit, secondCredit, spent int64
			var pending bool
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1),
     (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
     (SELECT payout_byte_count FROM transfer_escrow WHERE contract_id=$3 AND balance_id=$2),
     EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$3)`, f.balanceId, second, id).Scan(&firstCredit, &secondCredit, &spent, &pending))
			if firstCredit != 1000 || secondCredit != 989 || spent != 11 || pending {
				t.Fatal("expiry allocation or financial completion changed", firstCredit, secondCredit, spent, pending)
			}
		})
		requireLegacyProviderDurability(t, ctx, f, id, 11, 22)
	})
}

func TestLegacySettlementShardHardDeleteRetainsIntent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		peer := newEscrowSelectionTestClients(t, ctx)
		f := netEscrowOrderingTestFixture{sourceNetworkId: owner.NetworkId, sourceId: owner.ClientId, destinationNetworkId: peer.providerNetworkId, destinationId: peer.providerId, balanceId: owner.BalanceId}
		c, posts := createNetEscrowOrderingTestContract(ctx, f, 4096)
		server.RunPosts(ctx, posts...)
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Raise(CloseContract(ctx, c.ContractId, owner.ClientId, 1024, false))
		server.Raise(CloseContract(ctx, c.ContractId, peer.providerId, 1024, false))
		deleted, err := ReapProberShard(ctx, owner.Key)
		if err != nil || deleted {
			t.Fatal("shard deletion bypassed pending legacy reservation", deleted, err)
		}
		completed, busy, _, err := flushLegacySettlement(ctx, c.ContractId)
		if err != nil || busy || !completed {
			t.Fatal("draining shard lost settlement authority", completed, busy, err)
		}
		deleted, err = ReapProberShard(ctx, owner.Key)
		if err != nil || !deleted {
			t.Fatal("completed legacy intent permanently pinned shard", deleted, err)
		}
	})
}
