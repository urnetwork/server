// A bounded head retry shortens old-owner latency without rewinding the traversal.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The tail uses an independent grant so the head's granted lock cannot block it.
func legacyHeadRevisitFixture(t testing.TB, ctx context.Context, count int) (netEscrowOrderingTestFixture, server.Id, netEscrowOrderingTestFixture, []server.Id) {
	t.Helper()
	head, headId := legacySettlementTestIntent(t, ctx)
	tail := newNetEscrowOrderingTestFixture(t, ctx)
	oldest := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=1000000,balance_byte_count=1000000 WHERE balance_id=$1`, tail.balanceId))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, headId, oldest))
	})
	tailIds := make([]server.Id, count)
	for index := range tailIds {
		escrow, posts := createNetEscrowOrderingTestContract(ctx, tail, 100)
		server.RunPosts(ctx, posts...)
		id := escrow.ContractId
		id[15] = headId[15]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, id))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, id))
		})
		server.Raise(CloseContract(ctx, id, tail.sourceId, 11, false))
		server.Raise(CloseContract(ctx, id, tail.destinationId, 11, false))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, id, oldest.Add(time.Duration(index+1)*time.Second)))
		})
		tailIds[index] = id
	}
	return head, headId, tail, tailIds
}

// A fixed initial cohort bigger than two full pages must not delay a released
// oldest owner until that cohort is exhausted. The next page also advances tail.
func TestLegacySettlementHeadRevisitBeforeLargeFinitePassEnds(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		head, headId, tail, tailIds := legacyHeadRevisitFixture(t, ctx, 128)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, head.balanceId))
		shard := int(headId[15]) % LegacySettlementShardCount
		first, err := FlushLegacySettlements(ctx, shard, nil, 64)
		if err != nil || first.Visited != 64 || first.Completed != 63 || first.BusyOrGone != 1 || first.Cursor == nil || first.Cursor.ContractId != tailIds[62] {
			t.Fatalf("initial pass did not force a skipped head and long remainder: %+v, %v", first, err)
		}
		server.Raise(held.Rollback(ctx))
		second, err := FlushLegacySettlements(ctx, shard, first.Cursor, 64)
		if err != nil || second.Visited != 64 || second.Completed != 64 || second.Failed != 0 || second.Cursor == nil || !second.Cursor.PassEndTime.Equal(first.Cursor.PassEndTime) {
			t.Fatalf("continued page lost its bounded forward pass: %+v, %v", second, err)
		}
		requireLegacySettlementTestState(t, ctx, head, headId, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, head, headId, 11)
		if second.Cursor.ContractId != tailIds[125] || second.HeadVisited != 1 || second.HeadCompleted != 1 || second.HeadBusyOrGone != 0 || second.HeadFailed != 0 {
			t.Fatal("head retry reset or skipped the forward cursor")
		}
		last, err := FlushLegacySettlements(ctx, shard, second.Cursor, 64)
		if err != nil || last.Completed != 2 || last.Cursor != nil {
			t.Fatalf("remaining tail did not finish exactly once: %+v, %v", last, err)
		}
		projectLegacyProviderTotalsForTest(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var pending, terminal int
			var credit, swept, provided ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT
                (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1)),
                (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'),
                (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
                (SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
                (SELECT provided_byte_count FROM account_balance WHERE network_id=$3)`, tailIds, tail.balanceId, tail.destinationNetworkId).Scan(&pending, &terminal, &credit, &swept, &provided))
			if pending != 0 || terminal != len(tailIds) || credit != 1000000-11*int64(len(tailIds)) || swept != 11*int64(len(tailIds)) || provided != swept {
				t.Fatal("head retry changed tail financial conservation", pending, terminal, credit, swept, provided)
			}
		})
		if Testing_NetEscrowByteCount(ctx, tail.balanceId) != 0 {
			t.Fatal("tail retained completed reservations")
		}
		if replay, err := FlushLegacySettlements(ctx, shard, nil, 64); err != nil || replay.Visited != 0 {
			t.Fatalf("head replay repeated settlement: %+v, %v", replay, err)
		}
	})
}

// Every continued two-slot page advances one forward row even while the same
// old owner remains locked. An old serialized cursor gains and keeps its cutoff.
func TestLegacySettlementBusyHeadKeepsOldCursorForwardProgress(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		head, headId, tail, tailIds := legacyHeadRevisitFixture(t, ctx, 4)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, head.balanceId))
		shard := int(headId[15]) % LegacySettlementShardCount
		first, err := FlushLegacySettlements(ctx, shard, nil, 1)
		if err != nil || first.BusyOrGone != 1 || first.Cursor == nil {
			t.Fatalf("first owner was not held: %+v, %v", first, err)
		}
		data, err := json.Marshal(map[string]any{"next_attempt_time": first.Cursor.NextAttemptTime, "contract_id": first.Cursor.ContractId})
		server.Raise(err)
		cursor := &LegacySettlementCursor{}
		server.Raise(json.Unmarshal(data, cursor))
		var cutoff time.Time
		for index := range 3 {
			page, err := FlushLegacySettlements(ctx, shard, cursor, 2)
			if err != nil || page.Visited != 2 || page.Completed != 1 || page.BusyOrGone != 1 || page.Failed != 0 || page.HeadVisited != 1 || page.HeadBusyOrGone != 1 || page.HeadCompleted != 0 || page.HeadFailed != 0 || page.Cursor == nil || page.Cursor.ContractId != tailIds[index] || page.Cursor.PassEndTime.IsZero() {
				t.Fatalf("held head monopolized the page or rewound its cursor: %+v, %v", page, err)
			}
			if index > 0 && !cutoff.Equal(page.Cursor.PassEndTime) {
				t.Fatal("head retry replaced the fixed cutoff")
			}
			cutoff = page.Cursor.PassEndTime
			cursor = page.Cursor
			requireLegacySettlementTestState(t, ctx, tail, tailIds[index], false, true, 1000000-11*int64(index+1), 100*int64(3-index))
		}
		requireLegacySettlementTestState(t, ctx, head, headId, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, head, headId, 0)
		server.Raise(held.Rollback(ctx))
		last, err := FlushLegacySettlements(ctx, shard, cursor, 2)
		if err != nil || last.Completed != 2 || last.HeadCompleted != 1 || last.Cursor == nil || last.Cursor.ContractId != tailIds[3] {
			t.Fatalf("released head and final tail did not both finish: %+v, %v", last, err)
		}
		requireLegacySettlementTestState(t, ctx, head, headId, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, head, headId, 11)
		requireLegacySettlementTestState(t, ctx, tail, tailIds[3], false, true, 999956, 0)
	})
}

// A due accounting refusal at the head preserves the forward cursor and retry
// delay; it cannot rewind the pass or undo the healthy forward commit before it.
func TestLegacySettlementHeadFailurePreservesForwardCursor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		head, headId, tail, tailIds := legacyHeadRevisitFixture(t, ctx, 2)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, head.balanceId))
		shard := int(headId[15]) % LegacySettlementShardCount
		first, err := FlushLegacySettlements(ctx, shard, nil, 1)
		if err != nil || first.BusyOrGone != 1 || first.Cursor == nil {
			t.Fatalf("first owner was not held: %+v, %v", first, err)
		}
		server.Raise(held.Rollback(ctx))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=101 WHERE contract_id=$1`, headId))
		})
		page, err := FlushLegacySettlements(ctx, shard, first.Cursor, 2)
		if err != nil || page.Visited != 2 || page.Completed != 1 || page.Failed != 1 || page.HeadVisited != 1 || page.HeadFailed != 1 || page.HeadCompleted != 0 || page.HeadBusyOrGone != 0 || page.Cursor == nil || page.Cursor.ContractId != tailIds[0] || !page.Cursor.PassEndTime.Equal(first.Cursor.PassEndTime) {
			t.Fatalf("head refusal lost the forward commit/cursor: %+v, %v", page, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var next time.Time
			var code string
			server.Raise(conn.QueryRow(ctx, `SELECT next_attempt_time,failure_code FROM legacy_settlement_intent WHERE contract_id=$1`, headId).Scan(&next, &code))
			if code != "accounting" || next.Before(server.NowUtc().Add(14*time.Minute)) {
				t.Fatal("head refusal lost its finite accounting delay", code)
			}
		})
		last, err := FlushLegacySettlements(ctx, shard, page.Cursor, 2)
		if err != nil || last.Completed != 1 || last.HeadVisited != 0 || last.Cursor != nil {
			t.Fatalf("deferred head blocked the remainder or retried early: %+v, %v", last, err)
		}
		requireLegacySettlementTestState(t, ctx, head, headId, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, head, headId, 0)
		requireLegacySettlementTestState(t, ctx, tail, tailIds[1], false, true, 999978, 0)
	})
}

// Cancellation during the head's actual financial transaction preserves the
// preceding forward commit and cursor, while rollback keeps the head reserved.
func TestLegacySettlementCanceledHeadPreservesForwardCursor(t *testing.T) {
	testLegacySettlementCanceledHeadPreservesForwardCursor(t, false)
}

// Cancellation after an empty resumed segment must retain the same financial
// boundary when this page wraps to an earlier held head.
func TestLegacySettlementCanceledWrappedHeadPreservesForwardCursor(t *testing.T) {
	testLegacySettlementCanceledHeadPreservesForwardCursor(t, true)
}

func testLegacySettlementCanceledHeadPreservesForwardCursor(t *testing.T, wrap bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		head, headId, tail, tailIds := legacyHeadRevisitFixture(t, ctx, 2)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, head.balanceId))
		shard := int(headId[15]) % LegacySettlementShardCount
		first, err := FlushLegacySettlements(ctx, shard, nil, 1)
		if err != nil || first.BusyOrGone != 1 || first.Cursor == nil {
			t.Fatalf("first owner was not held: %+v, %v", first, err)
		}
		if wrap {
			first.Cursor.HeadAfter = &LegacySettlementPosition{NextAttemptTime: first.Cursor.NextAttemptTime, ContractId: first.Cursor.ContractId}
		}
		server.Raise(held.Rollback(ctx))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
                CREATE FUNCTION synthetic_legacy_head_cancel() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN
                    IF NEW.contract_id = TG_ARGV[0]::uuid THEN
                        PERFORM set_config('lock_timeout','0',true);
                        PERFORM pg_advisory_xact_lock(731029);
                    END IF;
                    RETURN NEW;
                END $$;
                CREATE TRIGGER synthetic_legacy_head_cancel
                BEFORE UPDATE OF outcome ON transfer_contract
                FOR EACH ROW WHEN (OLD.outcome IS NULL AND NEW.outcome IS NOT NULL)
                EXECUTE FUNCTION synthetic_legacy_head_cancel('%s');`, headId)))
		})
		held, err = conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT pg_advisory_xact_lock(731029)`))
		blocker := contractLifecycleTestBackendPid(t, ctx, held)
		parent, cancelParent := context.WithCancel(ctx)
		defer cancelParent()
		type pageResult struct {
			page LegacySettlementFlushResult
			err  error
		}
		done := make(chan pageResult, 1)
		go func() {
			page, err := FlushLegacySettlements(parent, shard, first.Cursor, 2)
			done <- pageResult{page: page, err: err}
		}()
		requireContractLifecycleBlockedBy(t, ctx, held, blocker)
		cancelParent()
		var canceled pageResult
		select {
		case canceled = <-done:
		case <-ctx.Done():
			t.Fatal("canceled head did not join", ctx.Err())
		}
		server.Raise(held.Rollback(ctx))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_legacy_head_cancel ON transfer_contract; DROP FUNCTION synthetic_legacy_head_cancel();`))
		})
		if canceled.err == nil || canceled.page.Completed != 1 || canceled.page.HeadVisited != 0 || canceled.page.Cursor == nil || canceled.page.Cursor.ContractId != tailIds[0] || !canceled.page.Cursor.PassEndTime.Equal(first.Cursor.PassEndTime) {
			t.Fatalf("canceled head lost the prior forward cursor: %+v, %v", canceled.page, canceled.err)
		}
		requireLegacySettlementTestState(t, ctx, head, headId, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, head, headId, 0)
		requireLegacySettlementTestState(t, ctx, tail, tailIds[0], false, true, 999989, 100)
		resumed, err := FlushLegacySettlements(ctx, shard, canceled.page.Cursor, 2)
		if err != nil || resumed.Completed != 2 || resumed.HeadCompleted != 1 {
			t.Fatalf("canceled head did not resume exactly once: %+v, %v", resumed, err)
		}
		requireLegacySettlementTestState(t, ctx, head, headId, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, head, headId, 11)
		requireLegacySettlementTestState(t, ctx, tail, tailIds[1], false, true, 999978, 0)
	})
}
