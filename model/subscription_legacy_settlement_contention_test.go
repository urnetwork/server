package model

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Public bilateral closure must finish while a different owner holds every
// shared financial row. The barrier is an actual granted PostgreSQL lock, not
// a sleep or a sampled absence of waits. Each caller owns a different contract.
func TestLegacySettlementCompletesWithSharedFinancialRowsHeld(t *testing.T) {
	legacySettlementHeldRowsControl(t, 64, 0)
}
func TestLegacySettlementLargeNMixedSamePayer(t *testing.T) {
	legacySettlementHeldRowsControl(t, 512, 64)
}
func legacySettlementHeldRowsControl(t *testing.T, count, markedCount int) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=1000000,balance_byte_count=1000000 WHERE balance_id=$1`, f.balanceId))

		})
		ids := make([]server.Id, count)
		for index := range ids {
			clientId := server.NewId()
			Testing_CreateDevice(ctx, f.sourceNetworkId, server.NewId(), clientId, "synthetic-settlement", "synthetic")
			owned := f
			owned.sourceId = clientId
			e, posts := createNetEscrowOrderingTestContract(ctx, owned, 100)
			server.RunPosts(ctx, posts...)
			ids[index] = e.ContractId
			server.Raise(CloseContract(ctx, e.ContractId, clientId, 11, false))
		}
		markedIds := make([]server.Id, markedCount)
		for index := range markedIds {
			clientId := server.NewId()
			Testing_CreateDevice(ctx, f.sourceNetworkId, server.NewId(), clientId, "synthetic-mixed", "synthetic")
			owned := f
			owned.sourceId = clientId
			e := createRedisAdmissionTest(ctx, owned, 100)
			markedIds[index] = e.ContractId
			server.Raise(CloseContract(ctx, e.ContractId, clientId, 11, false))
		}

		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		for _, table := range []string{"transfer_balance", "transfer_balance_net_escrow_revision", "transfer_balance_net_escrow_snapshot"} {
			server.RaisePgResult(held.Exec(ctx, fmt.Sprintf("SELECT balance_id FROM %s WHERE balance_id=$1 FOR UPDATE", table), f.balanceId))
		}
		start := make(chan struct{})
		errs := make(chan any, count+markedCount)
		var group sync.WaitGroup
		begin := time.Now()
		for _, id := range append(append([]server.Id{}, ids...), markedIds...) {
			group.Add(1)
			go func() {
				defer group.Done()
				<-start
				bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				err := server.HandleError(func() { server.Raise(CloseContract(bounded, id, f.destinationId, 11, false)) })
				errs <- err
			}()
		}
		close(start)
		group.Wait()
		close(errs)
		failed := 0
		for err := range errs {
			if err != nil {
				failed++
			}
		}
		if failed != 0 {
			t.Fatalf("public settlement queued behind held shared financial rows: %d/%d", failed, count)
		}
		var terminal, pending int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'`, ids).Scan(&terminal))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1)`, ids).Scan(&pending))
		})
		if terminal != 0 || pending != count || Testing_NetEscrowByteCount(ctx, f.balanceId) != 100*int64(count)+11*int64(markedCount) {
			t.Fatalf("acknowledgement must retain reservation and durable intent: terminal=%d pending=%d", terminal, pending)
		}
		busy := 0
		for shard := range LegacySettlementShardCount {
			result, err := FlushLegacySettlements(ctx, shard, nil, 64)
			if err != nil || result.Failed != 0 || result.Completed != 0 {
				t.Fatal("held grant did not defer bounded worker", result, err)
			}
			busy += result.BusyOrGone
		}
		if busy != count {
			t.Fatalf("worker did not skip every held grant: %d/%d", busy, count)
		}
		if markedCount > 0 {
			n, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
			if err != nil || !busy || n != 0 || released != 0 {
				t.Fatal("current worker bypassed held balance")
			}
		}
		server.Raise(held.Rollback(ctx))
		if markedCount > 0 {
			n, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
			if err != nil || busy || n != markedCount || released != markedCount {
				t.Fatal("legacy backlog starved current debit", n, released, busy, err)
			}
		}
		completed := 0
		for shard := range LegacySettlementShardCount {
			result, err := FlushLegacySettlements(ctx, shard, nil, 64)
			if err != nil || result.Failed != 0 || result.BusyOrGone != 0 {
				t.Fatal("released grant failed to drain", result, err)
			}
			completed += result.Completed
		}
		var credit ByteCount
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'`, ids).Scan(&terminal))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1)`, ids).Scan(&pending))
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.balanceId).Scan(&credit))
		})
		if completed != count || terminal != count || pending != 0 || credit != 1000000-11*int64(count+markedCount) || Testing_NetEscrowByteCount(ctx, f.balanceId) != 0 {
			t.Fatalf("worker accounting mismatch: completed=%d terminal=%d pending=%d credit=%d", completed, terminal, pending, credit)
		}
		projectLegacyProviderTotalsForTest(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var swept, provided int64
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE network_id=$1),
                (SELECT provided_byte_count FROM account_balance WHERE network_id=$1)`, f.destinationNetworkId).Scan(&swept, &provided))
			if swept != 11*int64(count+markedCount) || provided != 11*int64(count) {
				t.Fatal("mixed provider financial conservation failed", swept, provided)
			}
		})
		t.Logf("legacy public settlements=%d current=%d elapsed=%s held shared rows=3", count, markedCount, time.Since(begin))
	})
}
