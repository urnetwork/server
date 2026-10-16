// Finite settlement passes revisit released owners despite continuing arrivals.
package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A released oldest owner must return even while each page gets new due work.
// Explicit PostgreSQL locks and committed arrivals force the growing-tail order.
func TestLegacySettlementGrowingTailRevisitsReleasedOwner(t *testing.T) {
	legacySettlementGrowingTailControl(t, false)
}

// Tasks persisted by an older worker gain a cutoff on their first continuation.
func TestLegacySettlementGrowingTailBootstrapsOldCursor(t *testing.T) {
	legacySettlementGrowingTailControl(t, true)
}

// Both cursor formats must finish the original cohort before following arrivals.
func legacySettlementGrowingTailControl(t *testing.T, oldCursor bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		oldest, oldestId := legacySettlementTestIntent(t, ctx)
		shard := int(oldestId[15]) % LegacySettlementShardCount
		type queuedContract struct {
			fixture    netEscrowOrderingTestFixture
			contractId server.Id
		}
		contracts := []queuedContract{{fixture: oldest, contractId: oldestId}}
		queue := func() server.Id {
			fixture := newNetEscrowOrderingTestFixture(t, ctx)
			escrow, posts := createNetEscrowOrderingTestContract(ctx, fixture, 100)
			server.RunPosts(ctx, posts...)
			id := escrow.ContractId
			id[15] = oldestId[15]
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, id))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, id))
			})
			server.Raise(CloseContract(ctx, id, fixture.sourceId, 11, false))
			server.Raise(CloseContract(ctx, id, fixture.destinationId, 11, false))
			refreshNetEscrow(ctx, []server.Id{fixture.balanceId})
			contracts = append(contracts, queuedContract{fixture: fixture, contractId: id})
			return id
		}
		initialTailId := queue()
		oldestTime := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, oldestId, oldestTime))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, initialTailId, oldestTime.Add(time.Second)))
		})
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, oldest.balanceId))
		first, err := FlushLegacySettlements(ctx, shard, nil, 1)
		if err != nil || first.Visited != 1 || first.BusyOrGone != 1 || first.Completed != 0 || first.Cursor == nil || first.Cursor.ContractId != oldestId || !first.More {
			t.Fatalf("held oldest row did not yield its cursor: %+v, %v", first, err)
		}
		requireLegacySettlementTestState(t, ctx, oldest, oldestId, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, oldest, oldestId, 0)
		server.Raise(held.Rollback(ctx))

		cursor := first.Cursor
		if oldCursor {
			data, err := json.Marshal(struct {
				NextAttemptTime time.Time `json:"next_attempt_time"`
				ContractId      server.Id `json:"contract_id"`
			}{NextAttemptTime: cursor.NextAttemptTime, ContractId: cursor.ContractId})
			server.Raise(err)
			cursor = &LegacySettlementCursor{}
			server.Raise(json.Unmarshal(data, cursor))
		}
		for page := range 4 {
			queue()
			result, err := FlushLegacySettlements(ctx, shard, cursor, 1)
			if err != nil || result.Failed != 0 || result.BusyOrGone != 0 {
				t.Fatalf("released traversal failed at page %d: %+v, %v", page, result, err)
			}
			if page == 0 && (result.Completed != 1 || result.Cursor == nil || result.Cursor.ContractId != initialTailId) {
				t.Fatalf("busy head prevented the original tail from progressing: %+v", result)
			}
			cursor = result.Cursor
		}
		// The old loop always finds another tail row and never revisits this
		// now-unlocked owner. A finite pass must wrap and settle it by here.
		requireLegacySettlementTestState(t, ctx, oldest, oldestId, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, oldest, oldestId, 11)

		for page := 0; page <= len(contracts); page++ {
			result, err := FlushLegacySettlements(ctx, shard, cursor, 64)
			if err != nil || result.Failed != 0 || result.BusyOrGone != 0 {
				t.Fatalf("finite queue failed to drain: %+v, %v", result, err)
			}
			cursor = result.Cursor
			if cursor == nil && result.Visited == 0 {
				break
			}
		}
		for _, contract := range contracts {
			requireLegacySettlementTestState(t, ctx, contract.fixture, contract.contractId, false, true, 989, 0)
			requireLegacyProviderDurability(t, ctx, contract.fixture, contract.contractId, 11)
		}
		if replay, err := FlushLegacySettlements(ctx, shard, nil, 64); err != nil || replay.Visited != 0 {
			t.Fatalf("completed traversal replay changed accounting: %+v, %v", replay, err)
		}
	})
}

// A failed owner leaves the finite pass when its retry time advances. Its debt
// stays protected while healthy siblings finish and the next pass excludes it.
func TestLegacySettlementDeferredFailureLeavesFixedPass(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		failed, failedId := legacySettlementTestIntent(t, ctx)
		healthy := newNetEscrowOrderingTestFixture(t, ctx)
		escrow, posts := createNetEscrowOrderingTestContract(ctx, healthy, 100)
		server.RunPosts(ctx, posts...)
		healthyId := escrow.ContractId
		healthyId[15] = failedId[15]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, healthyId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET contract_id=$2 WHERE contract_id=$1`, escrow.ContractId, healthyId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=101 WHERE contract_id=$1`, failedId))
		})
		server.Raise(CloseContract(ctx, healthyId, healthy.sourceId, 11, false))
		server.Raise(CloseContract(ctx, healthyId, healthy.destinationId, 11, false))
		refreshNetEscrow(ctx, []server.Id{healthy.balanceId})
		oldestTime := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, failedId, oldestTime))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET next_attempt_time=$2 WHERE contract_id=$1`, healthyId, oldestTime.Add(time.Second)))
		})
		shard := int(failedId[15]) % LegacySettlementShardCount
		first, err := FlushLegacySettlements(ctx, shard, nil, 64)
		if err != nil || first.Failed != 1 || first.Completed != 0 || first.Cursor == nil || first.Cursor.ContractId != failedId || first.Cursor.PassEndTime.IsZero() {
			t.Fatalf("accounting failure did not retain its pass cursor: %+v, %v", first, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var next time.Time
			var code string
			server.Raise(conn.QueryRow(ctx, `SELECT next_attempt_time,failure_code FROM legacy_settlement_intent WHERE contract_id=$1`, failedId).Scan(&next, &code))
			if code != "accounting" || !first.Cursor.PassEndTime.Before(next) || next.Before(server.NowUtc().Add(14*time.Minute)) {
				t.Fatal("accounting retry lost its delay or remained in the current pass", code)
			}
		})
		continued, err := FlushLegacySettlements(ctx, shard, first.Cursor, 64)
		if err != nil || continued.Failed != 0 || continued.Completed != 1 || continued.Cursor != nil {
			t.Fatalf("deferred failure blocked the healthy remainder: %+v, %v", continued, err)
		}
		if next, err := FlushLegacySettlements(ctx, shard, nil, 64); err != nil || next.Visited != 0 {
			t.Fatalf("new pass retried the failure before its delay: %+v, %v", next, err)
		}
		requireLegacySettlementTestState(t, ctx, failed, failedId, true, false, 1000, 100)
		requireLegacyProviderDurability(t, ctx, failed, failedId, 0)
		requireLegacySettlementTestState(t, ctx, healthy, healthyId, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, healthy, healthyId, 11)
	})
}
