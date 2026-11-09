//go:build sidecar_pending

package model

// QUOTA-M2-02: once MG2 is linked, every reader that keeps a balance because
// transfer_debit_journal holds a row of it reads service_debit_journal too, so the
// journal's delete guard never fails a batch. Under -tags sidecar_pending, through
// .stack/server-test.sh.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func testingInsertServiceDebit(ctx context.Context, balanceId server.Id) server.Id {
	usageId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO service_debit_journal (usage_id, balance_id, meter, debit_byte_count, batch_id)
				VALUES ($1, $2, 'evs', 512, $3)
			`,
			usageId,
			balanceId,
			server.NewId(),
		))
	})
	return usageId
}

func testingBalanceExists(ctx context.Context, balanceId server.Id) (exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id = $1)`, balanceId).Scan(&exists))
	})
	return
}

// Retention deletes an expired balance with no rows and keeps one with a service debit
// row, in one batch, rather than the guard failing the batch; once the row goes, the
// next pass deletes it.
func TestBalanceRetentionKeepsABalanceWithServiceDebits(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if !server.SidecarMg2Linked() {
			t.Fatal("a pending build does not link MG2")
		}
		networkId := server.NewId()
		now := server.NowUtc()
		for i := 0; i < 2; i += 1 {
			connect.AssertEqual(t, AddBasicTransferBalance(ctx, networkId, ByteCount(1024), now.Add(-30*24*time.Hour), now.Add(-8*24*time.Hour)), nil)
		}
		balanceIds := []server.Id{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT balance_id FROM transfer_balance WHERE network_id = $1 ORDER BY balance_id`, networkId)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var balanceId server.Id
					server.Raise(result.Scan(&balanceId))
					balanceIds = append(balanceIds, balanceId)
				}
			})
		})
		connect.AssertEqual(t, len(balanceIds), 2)
		kept, gone := balanceIds[0], balanceIds[1]
		usageId := testingInsertServiceDebit(ctx, kept)

		removeCompletedTransferBalanceBatches(ctx, server.NowUtc().Add(-7*24*time.Hour))
		connect.AssertEqual(t, testingBalanceExists(ctx, kept), true)
		connect.AssertEqual(t, testingBalanceExists(ctx, gone), false)

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM service_debit_journal WHERE balance_id = $1 AND usage_id = $2`, kept, usageId))
		})
		removeCompletedTransferBalanceBatches(ctx, server.NowUtc().Add(-7*24*time.Hour))
		connect.AssertEqual(t, testingBalanceExists(ctx, kept), false)
	})
}

// A redeemed balance with a service debit row is used: the move refuses it, as it does
// one with a transfer debit row.
func TestRedeemedBalanceMoveRefusesServiceDebits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newRedeemedBalanceMoveFixture(t, ctx, true, true)
		valid, err := PlanRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, valid.State, RedeemedBalanceMoveStateMove)

		testingInsertServiceDebit(ctx, f.balanceId)
		plan, err := PlanRedeemedBalanceMove(ctx, f.balanceId, f.targetNetworkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, plan.DebitJournalCount, int64(1))
		refused := false
		for _, refusal := range plan.Refusals {
			refused = refused || strings.Contains(refusal, "the balance is used")
		}
		if !refused {
			t.Fatalf("refusals = %v", plan.Refusals)
		}
	})
}
