package model

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

// Settlement writes no provider row (BILLING-SPLIT.md): while another
// transaction holds the provider network's account_balance row, the entire
// shared-grant cohort settles through the foreground owner, queues no provider
// totals and leaves the held row untouched. (Until 2026-11-11 settlement queued one provider total
// per contract, and this test followed those rows through their blocked
// projection while the cohort settled.)
func TestLegacyProviderTotalsHeldProviderDoesNotRetainGrant(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		const count = 512
		f, firstId := legacySettlementTestOpenFinals(t, ctx)
		ids := []server.Id{firstId}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=1000000,balance_byte_count=1000000,net_revenue_nano_cents=2000000 WHERE balance_id=$1`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_balance(network_id) VALUES($1)`, f.destinationNetworkId))
		})
		for index := 1; index < count; index++ {
			e, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
			server.RunPosts(ctx, posts...)
			id := e.ContractId
			// Both finals commit; the destination's genuine report owner omits
			// its settlement continuation, which runs below with the row held.
			server.Raise(CloseContract(ctx, id, f.sourceId, 11, false))
			server.Tx(ctx, func(tx server.PgTx) {
				applied, _, err := applyContractCloseReportInTx(ctx, tx, id, f.destinationId, 11, false, nil)
				server.Raise(err)
				if !applied {
					t.Fatal("destination final report did not commit")
				}
			}, server.TxReadCommitted)
			ids = append(ids, id)
		}
		// Keep the cache explicitly cold so foreground financial posts cannot
		// stand in for the independently queued historical mirror consumer.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
		})
		requireLegacyOwnedMetadataRedis(t, ctx, f.balanceId, 100*count)
		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		held, err := holder.Begin(ctx)
		server.Raise(err)
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { _ = held.Rollback(context.Background()) }) }
		defer release()
		server.RaisePgResult(held.Exec(ctx, `SELECT network_id FROM account_balance WHERE network_id=$1 FOR UPDATE`, f.destinationNetworkId))
		for _, id := range ids {
			if err := SettleEscrow(ctx, id, ContractOutcomeSettled); err != nil {
				t.Fatal("settlement waited on or wrote the held provider row", err)
			}
		}
		// The held transaction must still exist throughout the complete cohort;
		// an expired connection is not proof of independence from its row lock.
		server.RaisePgResult(held.Exec(ctx, `SELECT 1`))
		assertFinancial := func() {
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT
                    (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled')=$4 AND
                    (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled AND settle_time IS NOT NULL AND payout_byte_count=11)=$4 AND
                    (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=1000000-11*$4 AND
                    (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=11*$4 AND
                    (SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=11*$4 AND
                    (SELECT provided_byte_count=0 AND provided_net_revenue_nano_cents=0 FROM account_balance WHERE network_id=$3) AND
                    NOT EXISTS(SELECT 1 FROM pending_task WHERE function_name=$5)`,
					ids, f.balanceId, f.destinationNetworkId, count, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(&exact))
				if !exact {
					t.Fatal("financial conservation failed or settlement published provider totals")
				}
			})
		}
		assertFinancial()
		release()
		mirrorOwner := legacyMirrorTestOwner(t, ctx, f.balanceId)
		requireLegacyOwnedMetadataRedis(t, ctx, f.balanceId, 100*count)
		finalizeProviderTotalsTestMirror(t, ctx, mirrorOwner)
		requireLegacyOwnedMetadataRedis(t, ctx, f.balanceId, 0)
		assertFinancial()
	})
}

// The signal covers either transport but is only a submission observation.
// The fixture separately requires the exact live PostgreSQL blocker edge.
type providerTotalContentionTx struct {
	server.PgTx
	beforeProvider func()
}

// Signal once for a matching provider statement before delegating unchanged.
func (self *providerTotalContentionTx) observeProvider(sql string) {
	if self.beforeProvider != nil && strings.Contains(sql, "INSERT INTO account_balance") {
		before := self.beforeProvider
		self.beforeProvider = nil
		before()
	}
}

// Ordinary controls retain the same observation before their direct write.
func (self *providerTotalContentionTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	self.observeProvider(sql)
	return self.PgTx.Exec(ctx, sql, args...)
}

// Pipelining keeps the provider write in the batch; an Exec-only wrapper cannot
// see it. Inspect supplied statements without consuming or changing any reply.
func (self *providerTotalContentionTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	for _, query := range batch.QueuedQueries {
		self.observeProvider(query.SQL)
	}
	return self.PgTx.SendBatch(ctx, batch)
}
