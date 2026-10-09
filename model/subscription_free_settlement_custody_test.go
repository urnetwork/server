// Retained payer authority survives missing escrow on every no-payout route.
package model

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Retry scheduling is separate from the immutable accepted intent authority.
// The exact original Redis token and deadline remain independently visible.
type freeSettlementCustodyState struct {
	durable    string
	total      int64
	token      int64
	expiration float64
}

// Read each resource after releasing the previous resource's connection.
func readFreeSettlementCustodyState(ctx context.Context, f netEscrowOrderingTestFixture, id server.Id) freeSettlementCustodyState {
	var state freeSettlementCustodyState
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT jsonb_build_object(
            'contract',to_jsonb(c),
            'reports',(SELECT jsonb_agg(to_jsonb(r) ORDER BY party) FROM contract_close r WHERE contract_id=$1),
            'intent',(SELECT to_jsonb(i)-'failure_code'-'next_attempt_time' FROM legacy_settlement_intent i WHERE contract_id=$1),
            'balance',(SELECT to_jsonb(b) FROM transfer_balance b WHERE balance_id=$2),
            'escrows',(SELECT jsonb_agg(to_jsonb(e) ORDER BY balance_id) FROM transfer_escrow e WHERE contract_id=$1),
            'sweeps',(SELECT jsonb_agg(to_jsonb(s) ORDER BY balance_id,network_id) FROM transfer_escrow_sweep s WHERE contract_id=$1),
            'journals',(SELECT jsonb_agg(to_jsonb(j) ORDER BY balance_id) FROM transfer_debit_journal j WHERE contract_id=$1))::text
            FROM transfer_contract c WHERE contract_id=$1`, id, f.balanceId).Scan(&state.durable))
	})
	keys := redisContractReservationKeys(f.balanceId)
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		state.total, err = r.Get(ctx, keys[0]).Int64()
		server.Raise(err)
		state.token, err = r.HGet(ctx, keys[1], id.String()).Int64()
		server.Raise(err)
		state.expiration, err = r.ZScore(ctx, keys[2], id.String()).Result()
		server.Raise(err)
	})
	return state
}

// The second authentic report commits before public paid settlement attempts
// its outcome. The trigger interrupts only that second transaction.
func interruptPaidExpiryPublicClose(t testing.TB, ctx context.Context, id, destinationId server.Id) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_paid_outcome_abort() RETURNS trigger LANGUAGE plpgsql AS $$
            BEGIN RAISE EXCEPTION USING ERRCODE='57014',MESSAGE='synthetic paid outcome interruption'; END;
            $$;
            CREATE TRIGGER synthetic_paid_outcome_abort BEFORE UPDATE ON transfer_contract
            FOR EACH ROW WHEN (OLD.outcome IS NULL AND NEW.outcome IS NOT NULL)
            EXECUTE FUNCTION synthetic_paid_outcome_abort();`))
	})
	var interrupted error
	server.HandleError(func() { interrupted = CloseContract(ctx, id, destinationId, 17, false) }, func(err error) { interrupted = err })
	if interrupted == nil {
		t.Fatal("public paid close did not stop after committing its report")
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_paid_outcome_abort ON transfer_contract; DROP FUNCTION synthetic_paid_outcome_abort();`))
	})
	server.Db(ctx, func(conn server.PgConn) {
		var exact bool
		server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND payer_network_id IS NOT NULL AND NOT dispute
            AND NOT usage_unverified AND provider_usage IS NULL
            AND EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1 AND redis_reserved AND NOT settled)
            AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
            AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
            AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
            AND (SELECT count(*)=2 AND bool_and(NOT checkpoint AND used_transfer_byte_count=17) FROM contract_close WHERE contract_id=$1)
            FROM transfer_contract WHERE contract_id=$1`, id).Scan(&exact))
		if !exact {
			t.Fatal("public paid interruption did not retain its reservation and two final reports")
		}
	})
}

// A deliberate custody edit occurs after actual expiry proof preparation.
// Each fixture has its own database so an unresolved row cannot inflate the
// next case's attempted-row count. No financial outcome is invented in setup.
func runFreeExpiryPayerCustodyRefusal(t *testing.T, shape string) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		batch := &legacySettlementPostBatch{mirrorBalanceIdSet: map[server.Id]bool{}}
		ctx = context.WithValue(ctx, legacySettlementPostBatchKey{}, batch)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 100)
		id := contract.ContractId
		server.Raise(CloseContract(ctx, id, f.sourceId, 17, shape != "final"))
		switch shape {
		case "disputed":
			SetContractDispute(ctx, id, true)
		case "final":
			interruptPaidExpiryPublicClose(t, ctx, id, f.destinationId)
		case "partial":
		default:
			t.Fatal("unknown custody refusal fixture")
		}
		expireFreeExpiryOwnerContracts(ctx, []server.Id{id}, true)
		var before freeSettlementCustodyState
		arrivals := 0
		callCtx := context.WithValue(ctx, forceCloseContinuationContextKey{}, func(parent context.Context, selected server.Id) context.Context {
			if selected == id {
				arrivals++
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_escrow WHERE contract_id=$1`, id))
				})
				before = readFreeSettlementCustodyState(ctx, f, id)
			}
			return parent
		})
		beforeClosed := contractClosedCounter.Snapshot()
		count, _, err := ForceCloseOpenContractIdsPage(callCtx, server.NowUtc().Add(-5*time.Minute), 32, 1, 0, 0, nil)
		if count != 1 || arrivals != 1 || !errors.Is(err, errContractFreeSettlementOwner) || forceCloseErrorAllowsQuarantine(err) ||
			before.token != 100 || before.total != 100 || before != readFreeSettlementCustodyState(ctx, f, id) || freeExpiryOwnerClockCount(batch) != 0 {
			t.Fatal("missing paid escrow acquired free settlement, report edits, or quarantine authority", shape, count, err)
		}
		afterClosed := contractClosedCounter.Snapshot()
		if !beforeClosed.Stable || !afterClosed.Stable || afterClosed.Confirmed != beforeClosed.Confirmed ||
			afterClosed.Uncertain != beforeClosed.Uncertain || afterClosed.Untracked != beforeClosed.Untracked {
			t.Fatal("paid custody refusal counted an attempted row as a terminal outcome", beforeClosed, afterClosed)
		}
		count, _, err = ForceCloseOpenContractIdsPage(ctx, server.NowUtc().Add(-5*time.Minute), 32, 1, 0, 0, nil)
		if count != 1 || !errors.Is(err, errContractFreeSettlementOwner) || before != readFreeSettlementCustodyState(ctx, f, id) || freeExpiryOwnerClockCount(batch) != 0 {
			t.Fatal("paid custody refusal replay released its retained obligation", shape, count, err)
		}
	})
}

// A public dispute cannot lose its paid authority after proof preparation.
func TestContractExpirationFreeDisputeRetainsPayerWithoutEscrow(t *testing.T) {
	runFreeExpiryPayerCustodyRefusal(t, "disputed")
}

// An interrupted public final pair cannot enter ordinary no-payout recovery.
func TestContractExpirationFreeFinalRetainsPayerWithoutEscrow(t *testing.T) {
	runFreeExpiryPayerCustodyRefusal(t, "final")
}

// A synthetic missing peer must pass custody admission before its report write.
func TestContractExpirationFreePartialRetainsPayerWithoutEscrow(t *testing.T) {
	runFreeExpiryPayerCustodyRefusal(t, "partial")
}

// The individual owner preserves the full intent; the actual payer page may
// update only its ordinary failure/backoff fields. A genuine source-owned free
// intent still closes with its positive usage and one destination clock.
func TestLegacyFreeSettlementRetainsPayerWithoutEscrow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		batch := &legacySettlementPostBatch{mirrorBalanceIdSet: map[server.Id]bool{}}
		ctx = context.WithValue(ctx, legacySettlementPostBatchKey{}, batch)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		id := createRedisAdmissionTest(ctx, f, 100).ContractId
		server.Raise(CloseContract(ctx, id, f.sourceId, 17, true))
		SetContractDispute(ctx, id, true)
		expireFreeExpiryOwnerContracts(ctx, []server.Id{id}, true)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, id))
			server.Raise(queueLegacySettlementInTx(ctx, tx, id, ContractOutcomeSettled, true))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_escrow WHERE contract_id=$1`, id))
		})
		readIntent := func() string {
			var value string
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT row_to_json(i)::text FROM legacy_settlement_intent i WHERE contract_id=$1`, id).Scan(&value))
			})
			return value
		}
		before, beforeIntent := readFreeSettlementCustodyState(ctx, f, id), readIntent()
		beforeClosed := contractClosedCounter.Snapshot()
		owned := context.WithValue(ctx, legacySettlementCloseScopeKey{}, ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: f.sourceNetworkId})
		completed, busy, _, err := flushLegacySettlement(owned, id)
		if completed || busy || !errors.Is(err, errContractFreeSettlementOwner) || beforeIntent != readIntent() ||
			before != readFreeSettlementCustodyState(ctx, f, id) || freeExpiryOwnerClockCount(batch) != 0 {
			t.Fatal("payer intent was forgiven solely because escrow disappeared", completed, busy, err)
		}
		page, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, 8)
		if err != nil || page.Visited != 1 || page.Completed != 0 || page.Failed != 1 || page.BusyOrGone != 0 ||
			before != readFreeSettlementCustodyState(ctx, f, id) || freeExpiryOwnerClockCount(batch) != 0 {
			t.Fatal("payer page changed missing-escrow custody instead of retaining failure", page, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var pending bool
			server.Raise(conn.QueryRow(ctx, `SELECT failure_code='operational' AND next_attempt_time>statement_timestamp() AT TIME ZONE 'UTC'
                FROM legacy_settlement_intent WHERE contract_id=$1`, id).Scan(&pending))
			if !pending {
				t.Fatal("payer custody refusal lost ordinary retry scheduling")
			}
		})
		afterRefusal := contractClosedCounter.Snapshot()
		if !beforeClosed.Stable || !afterRefusal.Stable || afterRefusal.Confirmed != beforeClosed.Confirmed ||
			afterRefusal.Uncertain != beforeClosed.Uncertain || afterRefusal.Untracked != beforeClosed.Untracked {
			t.Fatal("payer refusal counted an outcome", beforeClosed, afterRefusal)
		}
		free := newFreeExpiryOwnerFixture(ctx)
		freeId, err := CreateContractNoEscrow(ctx, free.networkId, free.sourceId, free.networkId, free.destinationId, 100)
		server.Raise(err)
		server.Raise(CloseContract(ctx, freeId, free.sourceId, 17, true))
		expireFreeExpiryOwnerContracts(ctx, []server.Id{freeId}, true)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, freeId))
			server.Raise(queueLegacySettlementInTx(ctx, tx, freeId, ContractOutcomeSettled, false))
		})
		freeOwner := ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: free.sourceId}
		freePage, err := runLegacyCloseSettlementPages(ctx, freeOwner, nil, 8, false)
		if err != nil || freePage.Completed != 1 || freePage.Failed != 0 || freeExpiryOwnerClockCount(batch) != 1 || before != readFreeSettlementCustodyState(ctx, f, id) {
			t.Fatal("payer refusal blocked or borrowed its genuine source-owned peer", freePage, err)
		}
		requireFreeExpiryOwnerClosed(t, ctx, []server.Id{freeId})
	})
}

// Real endpoint reports remain authentic even when financial custody breaks;
// the later public outcome owner must refuse to reinterpret paid work as free.
func TestContractFreePublicCloseRetainsPayerWithoutEscrow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		server.Redis(ctx, func(r server.RedisClient) { server.Raise(r.Del(ctx, clockTransferByteCountRedisKey).Err()) })
		f := newNetEscrowOrderingTestFixture(t, ctx)
		id := createRedisAdmissionTest(ctx, f, 100).ContractId
		server.Raise(CloseContract(ctx, id, f.sourceId, 17, false))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_escrow WHERE contract_id=$1`, id))
		})
		before := readFreeSettlementCustodyState(ctx, f, id)
		beforeClosed := contractClosedCounter.Snapshot()
		err := CloseContract(ctx, id, f.destinationId, 17, false)
		if !errors.Is(err, errContractFreeSettlementOwner) {
			t.Fatal("public close reinterpreted retained payer as a free outcome", err)
		}
		after := readFreeSettlementCustodyState(ctx, f, id)
		if before.total != after.total || before.token != after.token || before.expiration != after.expiration || after.token != 100 {
			t.Fatal("public no-escrow refusal changed its original paid reservation")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var retained bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND payer_network_id=$2 AND NOT usage_unverified AND provider_usage IS NULL
                AND (SELECT balance_byte_count=1000 FROM transfer_balance WHERE balance_id=$3)
                AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
                AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
                AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
                AND (SELECT count(*)=2 AND bool_and(NOT checkpoint AND used_transfer_byte_count=17) FROM contract_close WHERE contract_id=$1)
                FROM transfer_contract WHERE contract_id=$1`, id, f.sourceNetworkId, f.balanceId).Scan(&retained))
			if !retained {
				t.Fatal("public paid refusal lost authentic reports or monetary custody")
			}
		})
		afterClosed := contractClosedCounter.Snapshot()
		if !beforeClosed.Stable || !afterClosed.Stable || beforeClosed.Confirmed != afterClosed.Confirmed ||
			beforeClosed.Uncertain != afterClosed.Uncertain || beforeClosed.Untracked != afterClosed.Untracked {
			t.Fatal("public paid refusal counted a terminal outcome", beforeClosed, afterClosed)
		}
		if clock, ok := GetClock(ctx); ok && clock.TotalTransferByteCount != "0" {
			t.Fatal("public paid refusal published an uncommitted transfer clock")
		}
	})
}

// Reaped escrow on an already terminal paid row is ordinary retention. Its
// public/expiry replays stay inert rather than acquiring a new custody error.
func TestContractFreeSettlementTerminalReplayAfterEscrowRetention(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		id := createRedisAdmissionTest(ctx, f, 100).ContractId
		server.Raise(CloseContract(ctx, id, f.sourceId, 17, false))
		server.Raise(CloseContract(ctx, id, f.destinationId, 17, false))
		applied, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
		if err != nil || applied != 1 || released != 1 || busy {
			t.Fatal("terminal retention fixture did not finish its actual paid debit", applied, released, busy, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_escrow WHERE contract_id=$1`, id))
		})
		before := readRedisExpiryRepairTestState(ctx, id)
		beforeClosed := contractClosedCounter.Snapshot()
		closed, err := settleContract(ctx, id)
		if err != nil || closed {
			t.Fatal("terminal paid settlement replay acquired a free-custody error", closed, err)
		}
		err = captureContractExpiryRepair(func() error { return closeContractWithExpiryScope(ctx, nil, id, f.sourceId, 0, false) })
		if !errors.Is(err, errContractAlreadySettled) || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
			t.Fatal("terminal expiry report replay changed its existing outcome semantics", err)
		}
		afterClosed := contractClosedCounter.Snapshot()
		if !beforeClosed.Stable || !afterClosed.Stable || beforeClosed.Confirmed != afterClosed.Confirmed ||
			beforeClosed.Uncertain != afterClosed.Uncertain || beforeClosed.Untracked != afterClosed.Untracked {
			t.Fatal("terminal paid replay repeated confirmed closure", beforeClosed, afterClosed)
		}
	})
}
