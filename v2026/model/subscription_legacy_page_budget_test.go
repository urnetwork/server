package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Each statement stays below the worker's two-second limit, while a loaded
// page cannot fit all 64 independent commits inside its fifteen-second budget.
// A page timeout must checkpoint the committed prefix, preserve the interrupted
// contract's full reservation, and resume without a task-wide error backoff.
func TestLegacySettlementLoadedPageBudgetPreservesContinuation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		const count = 64
		const credit = 100_000
		const reserved = 100
		const usage = 11
		f := newNetEscrowOrderingTestFixture(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance
                SET start_balance_byte_count=$2,balance_byte_count=$2 WHERE balance_id=$1`, f.balanceId, credit))
		})
		ids := make([]server.Id, count)
		oldest := server.NowUtc().Add(-time.Hour)
		for index := range ids {
			escrow, posts := createNetEscrowOrderingTestContract(ctx, f, reserved)
			server.RunPosts(ctx, posts...)
			id := escrow.ContractId
			id[15] = 0
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, id))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, id))
			})
			ids[index] = id
			server.Raise(CloseContract(ctx, id, f.sourceId, usage, false))
			server.Raise(CloseContract(ctx, id, f.destinationId, usage, false))
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, id, oldest.Add(time.Duration(index)*time.Millisecond)))
			})
		}
		// The setup-only ID rewrites must not add deferred cold repair to
		// this financial page-budget control.
		refreshNetEscrow(ctx, []server.Id{f.balanceId})

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
                CREATE FUNCTION synthetic_legacy_settlement_residence() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN
                    PERFORM pg_sleep(0.35);
                    RETURN NEW;
                END $$;
                CREATE TRIGGER synthetic_legacy_settlement_residence
                BEFORE UPDATE OF outcome ON transfer_contract
                FOR EACH ROW WHEN (OLD.outcome IS NULL AND NEW.outcome IS NOT NULL)
                EXECUTE FUNCTION synthetic_legacy_settlement_residence();`))
		})
		started := time.Now()
		first, err := FlushLegacySettlements(ctx, 0, nil, count)
		elapsed := time.Since(started)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_legacy_settlement_residence ON transfer_contract;
                DROP FUNCTION synthetic_legacy_settlement_residence();`))
		})
		terminal := 0
		pending := 0
		var remaining, spent int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT
                (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'),
                (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1)),
                (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
                (SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow WHERE contract_id=ANY($1))`,
				ids, f.balanceId).Scan(&terminal, &pending, &remaining, &spent))
		})
		if terminal <= 0 || terminal >= count || pending != count-terminal || remaining != credit-usage*int64(terminal) || spent != usage*int64(terminal) {
			t.Fatalf("loaded partial page lost financial conservation: terminal=%d pending=%d credit=%d spent=%d result=%+v err=%v", terminal, pending, remaining, spent, first, err)
		}
		if debt := Testing_NetEscrowByteCount(ctx, f.balanceId); debt != reserved*int64(pending) {
			t.Fatalf("interrupted page reservation=%d want=%d", debt, reserved*pending)
		}
		t.Logf("loaded page elapsed=%s committed=%d pending=%d result=%+v err=%v", elapsed, terminal, pending, first, err)
		if err != nil || first.Completed != terminal || first.Failed != 0 || !first.More || first.Cursor == nil || first.Cursor.ContractId != ids[terminal-1] {
			t.Fatalf("page budget discarded its completed prefix instead of continuing: %+v, %v", first, err)
		}
		second, err := FlushLegacySettlements(ctx, 0, first.Cursor, count)
		if err != nil || second.Failed != 0 || first.Completed+second.Completed != count || second.Cursor != nil {
			t.Fatalf("continued page skipped the interrupted intent: %+v, %v", second, err)
		}
		projectLegacyProviderTotalsForTest(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var provided, swept int64
			server.Raise(conn.QueryRow(ctx, `SELECT
                (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'),
                (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1)),
                (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2),
                (SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1)),
                (SELECT provided_byte_count FROM account_balance WHERE network_id=$3)`,
				ids, f.balanceId, f.destinationNetworkId).Scan(&terminal, &pending, &remaining, &swept, &provided))
			if terminal != count || pending != 0 || remaining != credit-usage*count || swept != usage*count || provided != usage*count {
				t.Fatalf("continued page changed exact accounting: terminal=%d pending=%d credit=%d swept=%d provided=%d", terminal, pending, remaining, swept, provided)
			}
		})
		if debt := Testing_NetEscrowByteCount(ctx, f.balanceId); debt != 0 {
			t.Fatalf("continued page abandoned reservation=%d", debt)
		}
		if replay, err := FlushLegacySettlements(ctx, 0, nil, count); err != nil || replay.Visited != 0 {
			t.Fatalf("completed page replay changed accounting: %+v, %v", replay, err)
		}
	})
}

// A real PostgreSQL lock barrier cancels the parent only after one intent
// committed and the next entered its transaction. Parent cancellation must
// remain an error; it cannot inherit the page owner's normal yield behavior.
func TestLegacySettlementParentCancellationKeepsPartialPageError(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		f, firstID := legacySettlementTestIntent(t, ctx)
		escrow, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		secondID := escrow.ContractId
		secondID[15] = firstID[15]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, secondID))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, secondID))
		})
		server.Raise(CloseContract(ctx, secondID, f.sourceId, 11, false))
		server.Raise(CloseContract(ctx, secondID, f.destinationId, 11, false))
		refreshNetEscrow(ctx, []server.Id{f.balanceId})
		oldest := server.NowUtc().Add(-time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, firstID, oldest))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, secondID, oldest.Add(time.Millisecond)))
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
                CREATE FUNCTION synthetic_legacy_settlement_cancel() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN
                    IF NEW.contract_id = TG_ARGV[0]::uuid THEN
                        PERFORM set_config('lock_timeout','0',true);
                        PERFORM pg_advisory_xact_lock(731019);
                    END IF;
                    RETURN NEW;
                END $$;
                CREATE TRIGGER synthetic_legacy_settlement_cancel
                BEFORE UPDATE OF outcome ON transfer_contract
                FOR EACH ROW WHEN (OLD.outcome IS NULL AND NEW.outcome IS NOT NULL)
                EXECUTE FUNCTION synthetic_legacy_settlement_cancel('%s');`, secondID)))
		})
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT pg_advisory_xact_lock(731019)`))
		blocker := contractLifecycleTestBackendPid(t, ctx, held)
		parent, cancelParent := context.WithCancel(ctx)
		defer cancelParent()
		type outcome struct {
			result LegacySettlementFlushResult
			err    error
		}
		done := make(chan outcome, 1)
		shard := int(firstID[15]) % LegacySettlementShardCount
		go func() {
			result, err := FlushLegacySettlements(parent, shard, nil, 64)
			done <- outcome{result, err}
		}()
		requireContractLifecycleBlockedBy(t, ctx, held, blocker)
		cancelParent()
		var canceled outcome
		select {
		case canceled = <-done:
		case <-ctx.Done():
			t.Fatal("canceled page failed to join", ctx.Err())
		}
		server.Raise(held.Rollback(ctx))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_legacy_settlement_cancel ON transfer_contract;
                DROP FUNCTION synthetic_legacy_settlement_cancel();`))
		})
		if canceled.err == nil || parent.Err() != context.Canceled || canceled.result.Completed != 1 {
			t.Fatalf("parent cancellation acknowledged a partial page: %+v, %v", canceled.result, canceled.err)
		}
		requireLegacySettlementTestState(t, ctx, f, firstID, false, true, 989, 100)
		requireLegacySettlementTestState(t, ctx, f, secondID, true, false, 989, 100)
		resumed, err := FlushLegacySettlements(ctx, shard, nil, 64)
		if err != nil || resumed.Completed != 1 || resumed.Cursor != nil {
			t.Fatalf("parent-canceled intent did not resume: %+v, %v", resumed, err)
		}
		requireLegacySettlementTestState(t, ctx, f, secondID, false, true, 978, 0)
	})
}
