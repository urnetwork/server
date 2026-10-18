// Head allocation stays distinct and bounded while old owners and tail both progress.
package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A released old prefix must use its configured share while forward work remains.
func TestLegacySettlementHeadAllocationRevisitsReleasedPrefix(t *testing.T) {
	legacySettlementHeadAllocationControl(t, false)
}

// A second distinct head is reached only after more forward commits. Canceling
// its financial transaction retains that forward cursor and all unpaid escrow.
func TestLegacySettlementHeadAllocationCancellationRetainsInterleavedProgress(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		head, headId, tail, tailIds := legacyHeadRevisitFixture(t, ctx, 8)
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
		shard := int(headId[15]) % LegacySettlementShardCount
		first, err := FlushLegacySettlements(ctx, shard, nil, 4)
		if err != nil || first.BusyOrGone != 4 || first.Cursor == nil || first.Cursor.ContractId != tailIds[2] {
			t.Fatalf("first page did not establish the busy prefix: %+v, %v", first, err)
		}
		server.Raise(tailHeld.Rollback(ctx))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
                CREATE FUNCTION synthetic_legacy_head_allocation_cancel() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN
                    IF NEW.contract_id = TG_ARGV[0]::uuid THEN
                        PERFORM set_config('lock_timeout','0',true);
                        PERFORM pg_advisory_xact_lock(731039);
                    END IF;
                    RETURN NEW;
                END $$;
                CREATE TRIGGER synthetic_legacy_head_allocation_cancel
                BEFORE UPDATE OF outcome ON transfer_contract
                FOR EACH ROW WHEN (OLD.outcome IS NULL AND NEW.outcome IS NOT NULL)
                EXECUTE FUNCTION synthetic_legacy_head_allocation_cancel('%s');`, tailIds[0])))
		})
		barrier, err := tailConn.Begin(ctx)
		server.Raise(err)
		defer barrier.Rollback(context.Background())
		server.RaisePgResult(barrier.Exec(ctx, `SELECT pg_advisory_xact_lock(731039)`))
		blocker := contractLifecycleTestBackendPid(t, ctx, barrier)
		parent, cancelParent := context.WithCancel(ctx)
		defer cancelParent()
		type pageResult struct {
			page LegacySettlementFlushResult
			err  error
		}
		done := make(chan pageResult, 1)
		go func() {
			page, err := FlushLegacySettlements(parent, shard, first.Cursor, 8)
			done <- pageResult{page: page, err: err}
		}()
		requireContractLifecycleBlockedBy(t, ctx, barrier, blocker)
		cancelParent()
		var canceled pageResult
		select {
		case canceled = <-done:
		case <-ctx.Done():
			t.Fatal("canceled second head did not join", ctx.Err())
		}
		server.Raise(barrier.Rollback(ctx))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_legacy_head_allocation_cancel ON transfer_contract;
                DROP FUNCTION synthetic_legacy_head_allocation_cancel();`))
		})
		page := canceled.page
		if canceled.err == nil || page.Visited != 5 || page.Completed != 4 || page.BusyOrGone != 1 || page.Failed != 0 || page.HeadVisited != 1 || page.HeadBusyOrGone != 1 || page.HeadCompleted != 0 || page.HeadFailed != 0 {
			t.Fatalf("canceled second head lost interleaved work: %+v, %v", page, canceled.err)
		}
		if page.Cursor == nil || page.Cursor.ContractId != tailIds[6] || !page.Cursor.PassEndTime.Equal(first.Cursor.PassEndTime) {
			t.Fatal("canceled second head lost the latest forward cursor")
		}
		requireLegacySettlementTestState(t, ctx, head, headId, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, head, headId, 0)
		requireLegacySettlementTestState(t, ctx, tail, tailIds[0], true, false, 999956, 400)
		projectLegacyProviderTotalsForTest(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var swept, provided ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT
                (SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=$1),
                (SELECT provided_byte_count FROM account_balance WHERE network_id=$2)`, tailIds[0], tail.destinationNetworkId).Scan(&swept, &provided))
			if swept != 0 || provided != 44 {
				t.Fatal("canceled head committed or preceding payouts were lost", swept, provided)
			}
		})
		server.Raise(headHeld.Rollback(ctx))
		cursor := page.Cursor
		for range 4 {
			resumed, err := FlushLegacySettlements(ctx, shard, cursor, 8)
			if err != nil || resumed.Failed != 0 || resumed.BusyOrGone != 0 || resumed.Visited > 8 || resumed.HeadVisited > 2 {
				t.Fatalf("canceled allocation failed to resume: %+v, %v", resumed, err)
			}
			cursor = resumed.Cursor
			if cursor == nil && resumed.Visited == 0 {
				break
			}
		}
		requireLegacySettlementTestState(t, ctx, head, headId, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, head, headId, 11)
		requireLegacySettlementTestState(t, ctx, tail, tailIds[0], false, true, 999912, 0)
		projectLegacyProviderTotalsForTest(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var swept, provided ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT
                (SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
                (SELECT provided_byte_count FROM account_balance WHERE network_id=$2)`, tailIds, tail.destinationNetworkId).Scan(&swept, &provided))
			if swept != 88 || provided != swept {
				t.Fatal("cancellation replay changed exact payouts", swept, provided)
			}
		})
	})
}

// A held first head may consume only one head slot; later released heads progress.
func TestLegacySettlementHeadAllocationSkipsBusyFirstOwner(t *testing.T) {
	legacySettlementHeadAllocationControl(t, true)
}

// Both grants are first held so the cursor moves past a real busy prefix. Only
// the chosen oldest grant can remain held when the continued page runs.
func legacySettlementHeadAllocationControl(t *testing.T, keepFirstBusy bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		head, headId, tail, tailIds := legacyHeadRevisitFixture(t, ctx, 160)
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
		shard := int(headId[15]) % LegacySettlementShardCount
		first, err := FlushLegacySettlements(ctx, shard, nil, 64)
		if err != nil || first.Visited != 64 || first.BusyOrGone != 64 || first.Completed != 0 || first.Cursor == nil || first.Cursor.ContractId != tailIds[62] {
			t.Fatalf("initial pass did not establish the held old prefix: %+v, %v", first, err)
		}
		server.Raise(tailHeld.Rollback(ctx))
		if !keepFirstBusy {
			server.Raise(headHeld.Rollback(ctx))
		}
		page, err := FlushLegacySettlements(ctx, shard, first.Cursor, 64)
		wantBusy := 0
		if keepFirstBusy {
			wantBusy = 1
		}
		if err != nil || page.Visited != 64 || page.Completed != 64-wantBusy || page.BusyOrGone != wantBusy || page.Failed != 0 || page.HeadVisited != 16 || page.HeadCompleted != 16-wantBusy || page.HeadBusyOrGone != wantBusy || page.HeadFailed != 0 {
			t.Fatalf("released old prefix did not receive distinct bounded head slots: %+v, %v", page, err)
		}
		if page.Cursor == nil || page.Cursor.ContractId != tailIds[110] || !page.Cursor.PassEndTime.Equal(first.Cursor.PassEndTime) {
			t.Fatal("head allocation rewound or consumed the forward cursor")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var terminalHeads, terminalForward int
			server.Raise(conn.QueryRow(ctx, `SELECT
                (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'),
                (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($2) AND outcome='settled')`, tailIds[:15], tailIds[63:111]).Scan(&terminalHeads, &terminalForward))
			if terminalHeads != 15 || terminalForward != 48 {
				t.Fatal("head or forward work was repeated/skipped", terminalHeads, terminalForward)
			}
		})
		if keepFirstBusy {
			requireLegacySettlementTestState(t, ctx, head, headId, true, false, 1000, 100)
			requireLegacyProviderDurability(t, ctx, head, headId, 0)
			server.Raise(headHeld.Rollback(ctx))
		}
		cursor := page.Cursor
		for range 8 {
			result, err := FlushLegacySettlements(ctx, shard, cursor, 64)
			if err != nil || result.Failed != 0 || result.BusyOrGone != 0 || result.Visited > 64 || result.HeadVisited > 16 {
				t.Fatalf("remaining cohort lost bounded progress: %+v, %v", result, err)
			}
			cursor = result.Cursor
			if cursor == nil && result.Visited == 0 {
				break
			}
		}
		requireLegacySettlementTestState(t, ctx, head, headId, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, head, headId, 11)
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
				t.Fatal("head allocation changed exact financial conservation", pending, terminal, credit, swept, provided)
			}
		})
		if Testing_NetEscrowByteCount(ctx, tail.balanceId) != 0 {
			t.Fatal("completed cohort retained reservations")
		}
	})
}
