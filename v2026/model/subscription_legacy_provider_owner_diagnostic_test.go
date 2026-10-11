// A downstream provider owner can extend an upstream grant's financial lock.
package model

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// The actual PostgreSQL blocking edge identifies the provider row owner before
// a second contract proves that its shared grant is still unavailable.
func TestLegacySettlementProviderTotalWaitRetainsGrant(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		f, firstId := legacySettlementTestIntent(t, ctx)
		other, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		secondId := other.ContractId
		secondId[15] = byte((int(firstId[15]) + 1) % LegacySettlementShardCount)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET contract_id=$2 WHERE contract_id=$1`, other.ContractId, secondId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET contract_id=$2 WHERE contract_id=$1`, other.ContractId, secondId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_balance(network_id,provided_byte_count,provided_net_revenue_nano_cents) VALUES($1,0,0) ON CONFLICT(network_id) DO NOTHING`, f.destinationNetworkId))
		})
		server.Raise(CloseContract(ctx, secondId, f.sourceId, 11, false))
		server.Raise(CloseContract(ctx, secondId, f.destinationId, 11, false))
		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		held, err := holder.Begin(ctx)
		server.Raise(err)
		var releaseOnce sync.Once
		releaseProvider := func() { releaseOnce.Do(func() { _ = held.Rollback(context.Background()) }) }
		defer releaseProvider()
		var holderPid int
		server.Raise(held.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
		server.RaisePgResult(held.Exec(ctx, `SELECT network_id FROM account_balance WHERE network_id=$1 FOR UPDATE`, f.destinationNetworkId))
		enteredProvider := make(chan struct{})
		ownerDone := make(chan any, 1)
		var ownerPid int
		var workers sync.WaitGroup
		workers.Add(1)
		go func() {
			defer workers.Done()
			ownerDone <- server.HandleError(func() {
				var ownerPosts []func() any
				server.Tx(ctx, func(tx server.PgTx) {
					// This synthetic holder is bounded independently of the
					// worker's production timeout; no elapsed latency is claimed.
					server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='3s'; SET LOCAL lock_timeout='2s'`))
					server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&ownerPid))
					wrapped := &legacyProviderOwnerDiagnosticTx{PgTx: tx, beforeProvider: func() { close(enteredProvider) }}
					var completed, busy bool
					var err error
					ownerPosts, completed, busy, _, err = flushLegacySettlementInTx(ctx, wrapped, firstId)
					server.Raise(err)
					if !completed || busy {
						server.Raise(fmt.Errorf("provider-wait owner did not settle"))
					}
				}, server.TxReadCommitted, server.OptNoRetry())
				server.RunPosts(ctx, ownerPosts...)
			})
		}()
		defer func() {
			releaseProvider()
			cancel()
			workers.Wait()
		}()
		select {
		case <-enteredProvider:
		case err := <-ownerDone:
			t.Fatal("owner ended before provider write", err)
		case <-ctx.Done():
			t.Fatal("owner did not reach provider write", ctx.Err())
		}
		poll := time.NewTicker(5 * time.Millisecond)
		defer poll.Stop()
		for {
			blocked := false
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT $2=ANY(pg_blocking_pids($1)) AND EXISTS(
                    SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND query LIKE '%INSERT INTO account_balance%')`, ownerPid, holderPid).Scan(&blocked))
			})
			if blocked {
				break
			}
			select {
			case err := <-ownerDone:
				t.Fatal("owner ended before actual provider blocking edge", err)
			case <-ctx.Done():
				t.Fatal("actual provider blocking edge never appeared", ctx.Err())
			case <-poll.C:
			}
		}
		completed, busy, gate, err := flushLegacySettlement(ctx, secondId)
		if err != nil || completed || !busy || gate != legacySettlementBusyGrantSet {
			t.Fatal("provider-blocked owner did not retain its shared grant", completed, busy, gate, err)
		}
		releaseProvider()
		if err := <-ownerDone; err != nil {
			t.Fatal("released provider owner did not commit", err)
		}
		completed, busy, gate, err = flushLegacySettlement(ctx, secondId)
		if err != nil || !completed || busy || gate != legacySettlementBusyNone {
			t.Fatal("released sibling did not complete", completed, busy, gate, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var terminal, metadata, pending int
			var credit, swept, provided, sweptRevenue, providedRevenue int64
			server.Raise(conn.QueryRow(ctx, `SELECT
                (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'),
                (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled AND payout_byte_count=11),
                (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1)),
                (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
                (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
                (SELECT provided_byte_count FROM account_balance WHERE network_id=$3),
                (SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
                (SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$3)`,
				[]server.Id{firstId, secondId}, f.balanceId, f.destinationNetworkId).Scan(&terminal, &metadata, &pending, &credit, &swept, &provided, &sweptRevenue, &providedRevenue))
			if terminal != 2 || metadata != 2 || pending != 0 || credit != 978 || swept != 22 || provided != 22 || sweptRevenue != 22 || providedRevenue != 22 {
				t.Fatal("provider-wait replay changed accounting", terminal, metadata, pending, credit, swept, provided, sweptRevenue, providedRevenue)
			}
		})
		requireLegacyOwnedMetadataRedis(t, ctx, f.balanceId, 0)
		t.Log("actual provider-row blocking edge retained the financial grant; sibling classified grant-set busy, then both settled exactly after release; synthetic control does not identify a Main owner")
	})
}

// The signal is before the real SQL call; PostgreSQL blocking evidence, not
// this signal alone, establishes that the owner actually waits downstream.
type legacyProviderOwnerDiagnosticTx struct {
	server.PgTx
	beforeProvider func()
}

// Preserve the actual provider statement and all financial transaction state.
func (self *legacyProviderOwnerDiagnosticTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if self.beforeProvider != nil && strings.Contains(sql, "INSERT INTO account_balance") {
		hook := self.beforeProvider
		self.beforeProvider = nil
		hook()
	}
	return self.PgTx.Exec(ctx, sql, args...)
}
