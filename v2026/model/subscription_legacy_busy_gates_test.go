// Attribute refused visits at their existing ownership gates without new reads.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// A held intent and a gone intent share this gate; neither proves a lock census.
func TestLegacySettlementBusyGateIntentUnavailable(t *testing.T) {
	legacySettlementBusyGateControl(t, legacySettlementBusyIntent)
}

// Owning the intent does not imply ownership of its contract.
func TestLegacySettlementBusyGateContractUnavailable(t *testing.T) {
	legacySettlementBusyGateControl(t, legacySettlementBusyContract)
}

// A held shared grant is classified after both per-contract owners are held.
func TestLegacySettlementBusyGateGrantSetMismatch(t *testing.T) {
	legacySettlementBusyGateControl(t, legacySettlementBusyGrantSet)
}

// Both a forward key and its old head exercise the same granted lock gate.
func legacySettlementBusyGateControl(t *testing.T, gate legacySettlementBusyGate) {
	t.Helper()
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
		ids := []server.Id{headId, tailIds[0]}
		switch gate {
		case legacySettlementBusyIntent:
			server.RaisePgResult(held.Exec(ctx, `SELECT contract_id FROM legacy_settlement_intent WHERE contract_id=ANY($1) ORDER BY contract_id FOR UPDATE`, ids))
		case legacySettlementBusyContract:
			server.RaisePgResult(held.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=ANY($1) ORDER BY contract_id FOR UPDATE`, ids))
		case legacySettlementBusyGrantSet:
			server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=ANY($1) ORDER BY balance_id FOR UPDATE`, []server.Id{head.balanceId, tail.balanceId}))
		}
		shard := int(headId[15]) % LegacySettlementShardCount
		first, err := FlushLegacySettlements(ctx, shard, nil, 1)
		if err != nil || first.BusyOrGone != 1 || first.Cursor == nil || first.Cursor.ContractId != headId {
			t.Fatalf("granted gate did not establish an old head: %+v, %v", first, err)
		}
		page, err := FlushLegacySettlements(ctx, shard, first.Cursor, 2)
		if err != nil || page.Visited != 2 || page.Completed != 0 || page.Failed != 0 || page.BusyOrGone != 2 || page.HeadBusyOrGone != 1 || page.Cursor == nil || page.Cursor.ContractId != tailIds[0] {
			t.Fatalf("granted gate changed bounded traversal: %+v, %v", page, err)
		}
		totals := []int{page.BusyIntentUnavailable, page.BusyContractUnavailable, page.BusyGrantSetMismatch}
		heads := []int{page.HeadBusyIntentUnavailable, page.HeadBusyContractUnavailable, page.HeadBusyGrantSetMismatch}
		for index := range totals {
			want := 0
			if legacySettlementBusyGate(index+1) == gate {
				want = 1
			}
			if totals[index] != 2*want || heads[index] != want {
				t.Fatal("busy gates did not partition total and head visits", gate, totals, heads)
			}
		}
		requireLegacySettlementTestState(t, ctx, head, headId, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, head, headId, 0)
		server.Raise(held.Rollback(ctx))
		last, err := FlushLegacySettlements(ctx, shard, nil, 64)
		if err != nil || last.Completed != 3 || last.BusyOrGone != 0 || last.Failed != 0 || last.BusyIntentUnavailable+last.BusyContractUnavailable+last.BusyGrantSetMismatch != 0 {
			t.Fatalf("released gate did not recover: %+v, %v", last, err)
		}
		requireLegacySettlementTestState(t, ctx, head, headId, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, head, headId, 11)
		requireLegacySettlementTestState(t, ctx, tail, tailIds[0], false, true, 999978, 0)
		completed, busy, goneGate, err := flushLegacySettlement(ctx, headId)
		if err != nil || completed || !busy || goneGate != legacySettlementBusyIntent {
			t.Fatal("gone intent was misclassified or repeated payment", completed, busy, goneGate, err)
		}
		requireLegacyProviderDurability(t, ctx, head, headId, 11)
	})
}

// Static missing or expired grants cannot create a count/lock membership gap.
// Missing required funding remains an accounting refusal, with no busy gate.
func TestLegacySettlementBusyGateMissingAndExpiredGrants(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_time=now()-interval '2 days',end_time=now()-interval '1 day' WHERE balance_id=$1`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,4096)`, id, server.NewId()))
		})
		completed, busy, gate, err := flushLegacySettlement(ctx, id)
		if err != nil || !completed || busy || gate != legacySettlementBusyNone {
			t.Fatal("static missing/expired grant invented a busy refusal", completed, busy, gate, err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, f, id, 11)
		missing, missingId := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_id=$2 WHERE contract_id=$1`, missingId, server.NewId()))
		})
		page, err := FlushLegacySettlements(ctx, int(missingId[15])%LegacySettlementShardCount, nil, 1)
		if err != nil || page.Failed != 1 || page.Completed != 0 || page.BusyOrGone != 0 || page.BusyIntentUnavailable+page.BusyContractUnavailable+page.BusyGrantSetMismatch != 0 {
			t.Fatalf("missing required funding was not an accounting refusal: %+v, %v", page, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var code string
			var terminal bool
			var credit ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT intent.failure_code,contract.outcome IS NOT NULL,balance.balance_byte_count
                FROM legacy_settlement_intent intent JOIN transfer_contract contract USING(contract_id)
                CROSS JOIN transfer_balance balance WHERE intent.contract_id=$1 AND balance.balance_id=$2`, missingId, missing.balanceId).Scan(&code, &terminal, &credit))
			if code != "accounting" || terminal || credit != 1000 {
				t.Fatal("missing required funding lost its hold", code, terminal, credit)
			}
		})
		requireLegacyProviderDurability(t, ctx, missing, missingId, 0)
	})
}

// Insert a previously missing joined balance only after the real count has
// finished. The next statement sees it without any externally held grant lock.
func TestLegacySettlementBusyGateChangedJoinSnapshot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		f, id := legacySettlementTestIntent(t, ctx)
		appearing := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,100)`, id, appearing))
		})
		changed := false
		server.Tx(ctx, func(tx server.PgTx) {
			wrapped := &legacySettlementCountBarrierTx{PgTx: tx, afterCount: func() {
				server.Tx(ctx, func(other server.PgTx) {
					server.RaisePgResult(other.Exec(ctx, `INSERT INTO transfer_balance(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents)
                    VALUES($1,$2,now(),now()+interval '30 days',1000,1000,0)`, appearing, f.sourceNetworkId))
				}, server.TxReadCommitted)
				changed = true
			}}
			_, completed, busy, gate, err := flushLegacySettlementInTx(ctx, wrapped, id)
			if err != nil || completed || !busy || gate != legacySettlementBusyGrantSet || !changed {
				t.Fatal("changed statement snapshot was not a grant-set refusal", completed, busy, gate, changed, err)
			}
		}, server.TxReadCommitted)
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, f, id, 0)
		completed, busy, gate, err := flushLegacySettlement(ctx, id)
		if err != nil || !completed || busy || gate != legacySettlementBusyNone {
			t.Fatal("stable successor could not recover changed membership", completed, busy, gate, err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, f, id, 11)
		if Testing_NetEscrowByteCount(ctx, appearing) != 0 {
			t.Fatal("stable successor retained the unused grant reservation")
		}
	})
}

// This test-only transaction wrapper runs a real independent commit between the
// count and lock statements, without adding a hook to production ownership code.
type legacySettlementCountBarrierTx struct {
	server.PgTx
	afterCount func()
}

// All statements use the real transaction; only the count's completed read arms
// the deterministic change before the next statement takes its snapshot.
func (self *legacySettlementCountBarrierTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := self.PgTx.QueryRow(ctx, sql, args...)
	if sql == legacySettlementExpectedGrantCountSQL {
		return &legacySettlementCountBarrierRow{Row: row, afterCount: self.afterCount}
	}
	return row
}

// Scan must finish and release its result before another transaction changes membership.
type legacySettlementCountBarrierRow struct {
	pgx.Row
	afterCount func()
}

// A failed count never triggers the synthetic membership change.
func (self *legacySettlementCountBarrierRow) Scan(dest ...any) error {
	err := self.Row.Scan(dest...)
	if err == nil {
		self.afterCount()
	}
	return err
}
