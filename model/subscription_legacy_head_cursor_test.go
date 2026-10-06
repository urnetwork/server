// A busy prefix cannot reset every bounded revisit to its oldest owner.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Two held old owners consume the entire two-slot head share. An independently
// funded third head must be reached on the next page, while a long forward
// remainder still exists. All grants and ids are synthetic native fixtures.
func TestLegacySettlementHeadCursorPassesPersistentBusyPrefix(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		head, firstHead, tail, tailIds := legacyHeadRevisitFixture(t, ctx, 24)
		second, posts := createNetEscrowOrderingTestContract(ctx, head, 100)
		server.RunPosts(ctx, posts...)
		secondHead := second.ContractId
		secondHead[15] = firstHead[15]
		oldest := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET contract_id=$2 WHERE contract_id=$1`, second.ContractId, secondHead))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET contract_id=$2 WHERE contract_id=$1`, second.ContractId, secondHead))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=next_attempt_time+interval '1 second'
				WHERE contract_id=ANY($1)`, tailIds))
		})
		server.Raise(CloseContract(ctx, secondHead, head.sourceId, 11, false))
		server.Raise(CloseContract(ctx, secondHead, head.destinationId, 11, false))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, secondHead, oldest.Add(time.Second)))
		})
		headConn := acquireContractLifecycleTestConnection(t, ctx)
		defer headConn.Release()
		headHeld, err := headConn.Begin(ctx)
		server.Raise(err)
		defer headHeld.Rollback(context.Background())
		server.RaisePgResult(headHeld.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, head.balanceId))
		tailConn := acquireContractLifecycleTestConnection(t, ctx)
		defer tailConn.Release()
		tailHeld, err := tailConn.Begin(ctx)
		server.Raise(err)
		defer tailHeld.Rollback(context.Background())
		server.RaisePgResult(tailHeld.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, tail.balanceId))
		shard := int(firstHead[15]) % LegacySettlementShardCount
		initial, err := FlushLegacySettlements(ctx, shard, nil, 4)
		if err != nil || initial.BusyOrGone != 4 || initial.Cursor == nil || initial.Cursor.ContractId != tailIds[1] {
			t.Fatal("initial pass did not retain the held prefix behind its forward cursor")
		}
		server.Raise(tailHeld.Rollback(ctx))
		cursor := initial.Cursor
		for turn := range 2 {
			before, _ := json.Marshal(cursor)
			page, err := FlushLegacySettlements(ctx, shard, cursor, 8)
			after, _ := json.Marshal(cursor)
			if err != nil || page.Visited != 8 || page.HeadVisited != 2 || page.Cursor == nil ||
				page.Cursor.ContractId != tailIds[7+6*turn] || !page.Cursor.PassEndTime.Equal(initial.Cursor.PassEndTime) ||
				!bytes.Equal(before, after) {
				t.Fatal("head revisit mutated or rewound the independent forward cursor")
			}
			if turn == 0 {
				if page.HeadBusyOrGone != 2 || page.HeadCompleted != 0 || page.HeadBusyGrantSetMismatch != 2 || page.HeadGrantWaitTimedOut != 1 {
					t.Fatal("initial head revisit did not observe its entire held prefix")
				}
			} else if page.HeadCompleted != 2 || page.HeadBusyOrGone != 0 || page.Completed != 8 || page.HeadGrantWaitCompleted != 1 ||
				page.Cursor.HeadAfter == nil || page.Cursor.HeadAfter.ContractId != tailIds[1] {
				t.Fatal("persistent busy prefix starved the independently funded next head")
			}
			// Reconstruct the next owner exclusively from serialized task state.
			raw, _ := json.Marshal(page.Cursor)
			cursor = nil
			server.Raise(json.Unmarshal(raw, &cursor))
		}
		for _, id := range tailIds[:2] {
			if close, terminal := GetContractClose(ctx, id); !terminal || close.Outcome != ContractOutcomeSettled {
				t.Fatal("released older owner waited for the whole forward pass")
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var retained bool
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*)=2 FROM legacy_settlement_intent WHERE contract_id=ANY($1) AND failure_code='none' AND next_attempt_time<$3) AND
				(SELECT count(*)=2 FROM transfer_contract WHERE contract_id=ANY($1) AND outcome IS NULL) AND
				(SELECT balance_byte_count=1000 FROM transfer_balance WHERE balance_id=$2)`,
				[]server.Id{firstHead, secondHead}, head.balanceId, oldest.Add(2*time.Second)).Scan(&retained))
			if !retained {
				t.Fatal("head scheduling changed a still-owned reservation or retry evidence")
			}
		})
		if Testing_NetEscrowByteCount(ctx, head.balanceId) != 200 {
			t.Fatal("head scheduling released the held prefix's reservation")
		}
		server.Raise(headHeld.Rollback(ctx))
		for range 8 {
			page, err := FlushLegacySettlements(ctx, shard, cursor, 8)
			if err != nil || page.Failed != 0 || page.BusyOrGone != 0 || page.Visited > 8 || page.HeadVisited > 2 {
				t.Fatal("released prefix failed to return through its bounded durable owner")
			}
			cursor = page.Cursor
			if cursor == nil && page.Visited == 0 {
				break
			}
		}
		projectLegacyProviderTotalsForTest(t, ctx)
		for _, cohort := range []struct {
			fixture netEscrowOrderingTestFixture
			ids     []server.Id
			initial ByteCount
		}{{head, []server.Id{firstHead, secondHead}, 1000}, {tail, tailIds, 1000000}} {
			server.Db(ctx, func(conn server.PgConn) {
				var terminal, pending int
				var credit, swept, provided ByteCount
				server.Raise(conn.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'),
					(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1)),
					(SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
					(SELECT coalesce(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
					(SELECT provided_byte_count FROM account_balance WHERE network_id=$3)`,
					cohort.ids, cohort.fixture.balanceId, cohort.fixture.destinationNetworkId).Scan(&terminal, &pending, &credit, &swept, &provided))
				if terminal != len(cohort.ids) || pending != 0 || credit != cohort.initial-11*int64(terminal) || swept != 11*int64(terminal) || provided != swept {
					t.Fatal("head cursor replay changed exact consumption or provider accounting")
				}
			})
			if Testing_NetEscrowByteCount(ctx, cohort.fixture.balanceId) != 0 {
				t.Fatal("completed head cycle retained a reservation")
			}
		}
		replayed, err := FlushLegacySettlements(ctx, shard, nil, 8)
		if err != nil || replayed.Visited != 0 {
			t.Fatal("head revisit repeated a completed financial owner")
		}
		clock, ok := GetClock(ctx)
		if !ok || clock.TotalTransferByteCount != "286" {
			t.Fatal("head cursor recovery omitted or repeated a finalized clock contribution")
		}
	})
}
