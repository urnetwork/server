// A deadline close writes the contract row once: the retirement columns and
// the terminal outcome share one UPDATE.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The close retired the contract with its own UPDATE (dispute cleared, usage
// marked unverified, expiration capped at the retirement clock) and then
// claimed the outcome with a second UPDATE of the same row in the same
// transaction. By its deadline a contract's heap page is usually full, so the
// retirement version moved page and wrote an entry into every transfer_contract
// index, only to be replaced by the outcome version before commit. The close
// must write one row version that carries both, on both admission paths.
func TestDeadlineCloseWritesOneContractRowVersion(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		deadline := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
		for _, redis := range []bool{false, true} {
			_, contractId := newDeadlineDeferredFixture(t, ctx, redis)
			// an open dispute, verified usage and a later stored expiration are
			// all retired by the close
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=true,usage_unverified=false,expiration_time=$2 WHERE contract_id=$1`,
					contractId, deadline.Add(time.Hour)))
			})

			var rowVersionCount int64
			var posts []server.PostFunction
			server.Tx(ctx, func(tx server.PgTx) {
				contractUpdateCount := func() (updateCount int64) {
					server.Raise(tx.QueryRow(ctx, `SELECT n_tup_upd FROM pg_stat_xact_user_tables WHERE relid='transfer_contract'::regclass`).Scan(&updateCount))
					return
				}
				before := contractUpdateCount()
				var result *ContractDeadlineReconciliation
				result, posts = reconcileContractAtDeadlineInTx(ctx, tx, contractId, deadline)
				if result == nil || result.Outcome != ContractOutcomeSettled || result.AlreadyClosed || result.Missing {
					t.Fatal("deadline close did not settle the open contract", redis, result)
				}
				rowVersionCount = contractUpdateCount() - before
			}, server.TxReadCommitted, server.OptNoRetry())
			server.RunPosts(ctx, posts...)
			if rowVersionCount != 1 {
				t.Fatal("deadline close wrote more than one contract row version", redis, rowVersionCount)
			}

			server.Db(ctx, func(conn server.PgConn) {
				var dispute, usageUnverified, closed bool
				var outcome string
				var expirationTime time.Time
				server.Raise(conn.QueryRow(ctx, `SELECT dispute,usage_unverified,expiration_time,outcome,close_time IS NOT NULL
					FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&dispute, &usageUnverified, &expirationTime, &outcome, &closed))
				if dispute || !usageUnverified || !expirationTime.Equal(deadline) || outcome != ContractOutcomeSettled || !closed {
					t.Fatal("deadline close lost its retirement or outcome", redis, dispute, usageUnverified, expirationTime, outcome, closed)
				}
			})
		}
	})
}
