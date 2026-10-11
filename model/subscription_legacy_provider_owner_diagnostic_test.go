// A held provider row cannot extend an upstream grant's financial lock.
package model

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// A held provider row must not hold up the upstream grant. The financial owner
// queues provider totals instead of writing them, so it settles while the row
// is held and a second contract on the shared grant settles right after it.
// The projection writes the totals exactly once after the row is released.
func TestLegacySettlementProviderTotalWaitReleasesGrant(t *testing.T) {
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
		// Synthetic ID rewrites invalidate the cache. Keep this fixture warm.
		refreshNetEscrow(ctx, []server.Id{f.balanceId})
		stopHooks := startLegacyStatementHooks(t, ctx)
		defer stopHooks()
		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		held, err := holder.Begin(ctx)
		server.Raise(err)
		var releaseOnce sync.Once
		releaseProvider := func() { releaseOnce.Do(func() { _ = held.Rollback(context.Background()) }) }
		defer releaseProvider()
		server.RaisePgResult(held.Exec(ctx, `SELECT network_id FROM account_balance WHERE network_id=$1 FOR UPDATE`, f.destinationNetworkId))
		// Control: the provider row is really held against any writer.
		probe := acquireContractLifecycleTestConnection(t, ctx)
		_, err = probe.Exec(ctx, `SELECT network_id FROM account_balance WHERE network_id=$1 FOR UPDATE NOWAIT`, f.destinationNetworkId)
		probe.Release()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatal("provider row was not held", err)
		}
		provider := &legacyProviderStatementWatch{}
		var ownerPosts []func() any
		server.Tx(ctx, func(tx server.PgTx) {
			// A wait on the held row fails here instead of hanging the test.
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='3s'; SET LOCAL lock_timeout='2s'`))
			var completed, busy bool
			var err error
			hooked, unhook := withLegacyStatementHooks(ctx, tx, provider.hook())
			defer unhook()
			ownerPosts, completed, busy, _, err = flushLegacySettlementInTx(hooked, tx, firstId)
			if err != nil || !completed || busy {
				t.Fatal("financial owner waited on the held provider row", completed, busy, err)
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if len(provider.statements) != 0 {
			t.Fatal("financial owner wrote the provider row inside its grant transaction", provider.statements)
		}
		server.RunPosts(ctx, ownerPosts...)
		completed, busy, gate, err := flushLegacySettlement(ctx, secondId)
		if err != nil || !completed || busy || gate != legacySettlementBusyNone {
			t.Fatal("held provider row retained the shared grant", completed, busy, gate, err)
		}
		// Both totals wait in their projections, not in the financial owners.
		server.Db(ctx, func(conn server.PgConn) {
			var provided, pendingBytes, pendingRevenue int64
			server.Raise(conn.QueryRow(ctx, `WITH unapplied AS (
                SELECT allocation FROM pending_task
                CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
                WHERE function_name=$2 AND (args_json::jsonb->>'applied')::boolean=false
                    AND (allocation->>'network_id')::uuid=$1
            ) SELECT COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$1),0),
                COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0),
                COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)`,
				f.destinationNetworkId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(&provided, &pendingBytes, &pendingRevenue))
			if provided != 0 || pendingBytes != 22 || pendingRevenue != 22 {
				t.Fatal("provider totals did not wait in their projections", provided, pendingBytes, pendingRevenue)
			}
		})
		releaseProvider()
		projectLegacyProviderTotalsForTest(t, ctx)
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
				t.Fatal("provider projection changed accounting", terminal, metadata, pending, credit, swept, provided, sweptRevenue, providedRevenue)
			}
		})
		requireLegacyOwnedMetadataRedis(t, ctx, f.balanceId, 0)
	})
}

// Records each statement of the hooked transaction that touches a provider
// account row.
type legacyProviderStatementWatch struct {
	statements []string
}

// Watches statements and every statement queued in a batch.
func (self *legacyProviderStatementWatch) hook() *legacyStatementHook {
	return &legacyStatementHook{before: func(_ context.Context, sql string, queued []string) {
		if queued == nil {
			queued = []string{sql}
		}
		for _, statement := range queued {
			if strings.Contains(statement, "account_balance") {
				self.statements = append(self.statements, strings.Join(strings.Fields(statement), " "))
			}
		}
	}}
}
