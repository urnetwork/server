package model

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Public bilateral closure must finish while a different owner holds every
// shared financial row. The barrier is an actual granted PostgreSQL lock, not
// a sleep or a sampled absence of waits. Each caller owns a different contract.
func TestRedisSettlementCompletesWithSharedFinancialRowsHeld(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=1000000,balance_byte_count=1000000 WHERE balance_id=$1`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance_net_escrow_revision(balance_id,revision) VALUES($1,1)`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance_net_escrow_snapshot(balance_id,revision,reserved_byte_count) VALUES($1,1,0)`, f.balanceId))
		})
		const count = 64
		ids := make([]server.Id, count)
		for index := range ids {
			clientId := server.NewId()
			Testing_CreateDevice(ctx, f.sourceNetworkId, server.NewId(), clientId, "synthetic-settlement", "synthetic")
			e, err := CreateTransferEscrow(ctx, f.sourceNetworkId, clientId, f.destinationNetworkId, f.destinationId, 100)
			server.Raise(err)
			ids[index] = e.ContractId
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
		errs := make(chan any, count)
		var group sync.WaitGroup
		begin := time.Now()
		for _, id := range ids {
			group.Add(1)
			go func() {
				defer group.Done()
				<-start
				bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
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
		var terminal int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'`, ids).Scan(&terminal))
		})
		if terminal != count {
			t.Fatalf("completed calls without durable outcomes: %d/%d", terminal, count)
		}
		t.Logf("current public settlements=%d elapsed=%s held shared rows=3", count, time.Since(begin))
	})
}
