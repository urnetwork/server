// A stopped legacy visit preserves the previous committed cursor and every
// original failure before attempting another write with its expired owner.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Real selection and settlement surround the interrupted second visit. The
// resumed public worker must settle the remaining intent exactly once.
func TestLegacySettlementPageDeadlinePreservesJoinedFailureAndCommittedCursor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		physical := &pgconn.PgError{Code: "40001", Message: "synthetic independent transaction failure"}
		for _, c := range []struct {
			name         string
			failure      error
			cancelParent bool
			yield        bool
		}{
			{name: "owned_joined_stop", failure: errors.Join(server.DbContextDoneError, context.Canceled, context.Canceled), yield: true},
			{name: "owned_joined_hard_stop", failure: errors.Join(server.DbContextDoneError, physical, context.Canceled)},
			{name: "owned_hard_stop", failure: physical},
			{name: "parent_joined_stop", failure: errors.Join(server.DbContextDoneError, context.Canceled), cancelParent: true},
		} {
			f, firstId := legacySettlementTestIntent(t, ctx)
			escrow, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
			server.RunPosts(ctx, posts...)
			secondId := escrow.ContractId
			secondId[15] = firstId[15]
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, secondId))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, secondId))
			})
			server.Raise(CloseContract(ctx, secondId, f.sourceId, 11, false))
			server.Raise(CloseContract(ctx, secondId, f.destinationId, 11, false))
			oldest := server.NowUtc().Add(-time.Hour)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, firstId, oldest))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, secondId, oldest.Add(time.Millisecond)))
			})
			// Setup-only identity rewrites must not add deferred cold projection
			// repair to this financial cancellation and committed-cursor control.
			refreshNetEscrow(ctx, []server.Id{f.balanceId})
			owner, cancelParent := context.WithCancel(ctx)
			bounded, expire := context.WithCancelCause(owner)
			calls := 0
			shard := int(firstId[15]) % LegacySettlementShardCount
			result, err := flushLegacySettlementsPage(owner, bounded, shard, nil, 64,
				func(pageCtx context.Context, id server.Id, wait *legacySettlementGrantWait) (bool, bool, legacySettlementBusyGate, error) {
					calls++
					if calls == 1 {
						if id != firstId {
							t.Fatal("legacy page selected a different first intent")
						}
						return flushLegacySettlementWithGrantWait(pageCtx, id, wait)
					}
					if calls != 2 || id != secondId {
						t.Fatal("legacy page changed the interrupted visit")
					}
					// Financial ownership has advanced; the page's optional mirror
					// batch is deliberately still pending at this next-visit seam.
					requireLegacySettlementTestState(t, ctx, f, firstId, false, true, 989, 200)
					requireLegacySettlementTestState(t, ctx, f, secondId, true, false, 989, 200)
					expire(errLegacySettlementPageBudget)
					if c.cancelParent {
						cancelParent()
					}
					return false, false, legacySettlementBusyNone, c.failure
				})
			expire(nil)
			cancelParent()
			if c.yield {
				if err != nil {
					t.Fatalf("%s lost completed legacy prefix: %v", c.name, err)
				}
			} else if err != c.failure {
				t.Fatalf("%s replaced the original legacy failure: got %v want %v", c.name, err, c.failure)
			}
			if calls != 2 || result.Visited != 1 || result.Completed != 1 || result.Failed != 0 ||
				result.BusyOrGone != 0 || result.More != c.yield || result.Cursor == nil || result.Cursor.ContractId != firstId {
				t.Fatalf("%s advanced the interrupted legacy cursor: %+v calls=%d", c.name, result, calls)
			}
			requireLegacySettlementTestState(t, ctx, f, firstId, false, true, 989, 100)
			requireLegacySettlementTestState(t, ctx, f, secondId, true, false, 989, 100)
			resumed, err := FlushLegacySettlements(ctx, shard, result.Cursor, 64)
			if err != nil || resumed.Visited != 1 || resumed.Completed != 1 || resumed.Failed != 0 || resumed.Cursor != nil || resumed.More {
				t.Fatalf("%s did not resume the untouched legacy intent: %+v %v", c.name, resumed, err)
			}
			requireLegacySettlementTestState(t, ctx, f, firstId, false, true, 978, 0)
			requireLegacySettlementTestState(t, ctx, f, secondId, false, true, 978, 0)
			if replay, err := FlushLegacySettlements(ctx, shard, nil, 64); err != nil || replay.Visited != 0 || replay.Completed != 0 {
				t.Fatalf("%s replay changed completed legacy accounting: %+v %v", c.name, replay, err)
			}
		}
	})
}
