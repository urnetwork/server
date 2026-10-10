// Same-payer deadline rows of one expiry page take turns instead of refusing each other.
package model

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Two rows of one payer passed explicit deadlines and start together. Deadline
// reconciliation try-locks their shared grant. The first admitted row is held
// inside its transaction until the second either waits for its in-page turn or
// is refused; only the turn lets both rows close in the same page.
func TestForceClosePageGivesSamePayerDeadlineRowsTurns(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		ids := []server.Id{createRedisAdmissionTest(ctx, f, 100).ContractId, createRedisAdmissionTest(ctx, f, 200).ContractId}
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=$3 WHERE contract_id=ANY($1)`,
				ids, now.Add(-2*time.Hour), now.Add(-time.Hour)))
		}, server.TxReadCommitted, server.OptNoRetry())
		grant := server.NewPgOwnershipKey("transfer_balance", f.balanceId)
		other := make(chan struct{})
		var otherOnce, holdOnce sync.Once
		var refused atomic.Int32
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if !slices.Contains(event.Keys, grant) {
				return
			}
			switch event.Kind {
			case server.PgOwnershipRefused:
				refused.Add(1)
				otherOnce.Do(func() { close(other) })
			case server.PgOwnershipAdmitted:
				holdOnce.Do(func() {
					select {
					case <-other:
					case <-ctx.Done():
					}
				})
			}
		})
		callCtx := context.WithValue(observed, forceCloseRowTurnWaitKey{}, func(server.Id) {
			otherOnce.Do(func() { close(other) })
		})
		count, cursor, _, err := forceCloseOpenContractIdsPage(callCtx, now, 10, 2, 0, 0, nil)
		if err != nil || refused.Load() != 0 || count != 2 || cursor != nil {
			t.Fatal("same-payer deadline rows refused each other inside one page", count, refused.Load(), err)
		}
		for _, id := range ids {
			if close, terminal := GetContractClose(ctx, id); !terminal || close.Outcome != ContractOutcomeSettled {
				t.Fatal("a same-payer deadline row was left open by its own sibling", id)
			}
		}
	})
}
