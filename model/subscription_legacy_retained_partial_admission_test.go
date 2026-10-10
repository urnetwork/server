// Retained usage proof never substitutes for ordinary report continuation.
package model

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A source batch used to consume the retained proof and skip the absent peer,
// publishing a zero destination clock. The individual owner must first apply
// ordinary billing continuation while retaining exactly the original proof.
func TestLegacySourceBatchRetainedPartialKeepsContinuationClock(t *testing.T) {
	for _, unknownDirection := range []bool{false, true} {
		env := server.DefaultTestEnv()
		env.RerunCount = 0
		env.Run(t, func(t testing.TB) {
			ctx := t.Context()
			f := legacySourceBatchSeed(t, ctx, 8)
			id := f.ids[3]
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL,
                    usage_origin_is_source=CASE WHEN $3 THEN NULL ELSE usage_origin_is_source END WHERE contract_id=$1`,
					id, server.NowUtc().Add(-61*time.Minute), unknownDirection))
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM contract_close WHERE contract_id=$1 AND party='destination'`, id))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, id, server.NowUtc()))
				fresh, err := prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-5*time.Minute))
				if err != nil || fresh == nil || !fresh.usageUnverifiedRetained {
					t.Fatal("retained source fixture did not own original expiry proof", fresh, err)
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			proof, snapshot := readContractExpiryTestSnapshot(t, ctx, id)
			if snapshot.ByteCount != 0 || len(snapshot.Providers) != 0 || snapshot.ExcludedReason != "expired_unconfirmed" ||
				unknownDirection && snapshot.Expiry != nil || !unknownDirection &&
				(snapshot.Expiry == nil || len(snapshot.Expiry.Reports) != 1 || snapshot.Expiry.Reports[ContractPartySource].ByteCount != 11) {
				t.Fatal("source fixture manufactured a missing-peer proof", snapshot)
			}
			healthyIds := append(append([]server.Id{}, f.ids[:3]...), f.ids[4:]...)
			healthyReports := legacyFinancialCohortReports(ctx, healthyIds)
			posts := &legacySettlementPostBatch{mirrorBalanceIdSet: map[server.Id]bool{}}
			owned := context.WithValue(ctx, legacySettlementCloseScopeKey{}, f.owner)
			owned = context.WithValue(owned, legacySettlementPostBatchKey{}, posts)
			readClocks := func() []ByteCount {
				posts.stateLock.Lock()
				defer posts.stateLock.Unlock()
				out := append([]ByteCount{}, posts.clockByteCounts...)
				slices.Sort(out)
				return out
			}
			server.Redis(ctx, func(r server.RedisClient) { server.Raise(r.Del(ctx, clockTransferByteCountRedisKey).Err()) })
			wire, closeWire := legacyFinancialRunProtocolBind(t, ctx)
			defer closeWire()
			before := contractClosedCounter.Snapshot()
			wire.enabled(true)
			attempts, err := flushLegacySourceSettlementBatch(owned, f.ids)
			wire.enabled(false)
			if err != nil || len(attempts) != 4 || !attempts[3].fallback || attempts[3].completed || attempts[3].busy {
				t.Fatal("retained source partial bypassed read-only continuation admission", unknownDirection, attempts, err)
			}
			for _, attempt := range attempts[:3] {
				if !attempt.completed || attempt.fallback || attempt.busy || attempt.financialWriteRollback {
					t.Fatal("retained source partial lost its healthy prefix", attempt)
				}
			}
			if !slices.Equal(readClocks(), []ByteCount{9, 9, 9}) {
				t.Fatal("refused source partial published a clock before continuation", readClocks())
			}
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND provider_usage=$2::jsonb
                    AND EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
                    AND (SELECT count(*)=1 AND bool_and(party='source' AND used_transfer_byte_count=11 AND NOT checkpoint)
                        FROM contract_close WHERE contract_id=$1)
                    FROM transfer_contract WHERE contract_id=$1`, id, proof).Scan(&exact))
				if !exact {
					t.Fatal("batch changed its refused source partial or proof")
				}
			})
			wire.enabled(true)
			page, err := runLegacyCloseSettlementPages(owned, f.owner, nil, 8, true)
			wire.enabled(false)
			if err != nil || page.Completed != 5 || page.Failed != 0 || page.BusyOrGone != 0 || page.More || page.Cursor != nil {
				t.Fatal("retained source partial did not continue with its healthy tail", page, err)
			}
			if !slices.Equal(readClocks(), []ByteCount{9, 9, 9, 9, 9, 9, 9, 11}) {
				t.Fatal("retained source partial lost or repeated its mirrored clock", readClocks())
			}
			if !bytes.Equal(healthyReports, legacyFinancialCohortReports(ctx, healthyIds)) {
				t.Fatal("source continuation changed healthy original reports")
			}
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT
                    (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled')=8
                    AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($1))
                    AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=ANY($1))
                    AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=ANY($1))
                    AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=ANY($1))
                    AND (SELECT count(*)=2 AND bool_and(NOT checkpoint AND used_transfer_byte_count=11)
                        FROM contract_close WHERE contract_id=$2)`, f.ids, id).Scan(&exact))
				if !exact {
					t.Fatal("source retained continuation gained money or changed billing fallback")
				}
			})
			afterProof, _ := readContractExpiryTestSnapshot(t, ctx, id)
			if !bytes.Equal(proof, afterProof) {
				t.Fatal("synthetic source peer rewrote its retained original usage")
			}
			for route, counts := range wire.snapshot() {
				if counts["transactions_with_writes_rolled_back"] != 0 || counts["rollback_commands_observed"] != 0 ||
					counts["connections_closed_with_active_writes"] != 0 || counts["connections_closed_after_begin_without_end_command"] != 0 {
					t.Fatal("retained partial caused a shared write rollback", route, counts)
				}
			}
			after := contractClosedCounter.Snapshot()
			if !before.Stable || !after.Stable || after.Confirmed-before.Confirmed != 8 ||
				after.Uncertain != before.Uncertain || after.Untracked != before.Untracked {
				t.Fatal("retained source continuation changed commit counting", before, after)
			}
			replay, err := runLegacyCloseSettlementPages(owned, f.owner, nil, 8, true)
			if err != nil || replay.Completed != 0 || replay.Visited != 0 || replay.More || replay.Cursor != nil || len(readClocks()) != 8 {
				t.Fatal("retained source replay duplicated a clock or outcome", replay, err, readClocks())
			}
			posts.finish(ctx)
			clock, ok := GetClock(ctx)
			if !ok || clock.TotalTransferByteCount != "74" {
				t.Fatal("joined source clock does not contain seven originals and one mirrored report", clock, ok)
			}
		})
	}
}

// Billing permits one checkpoint, but a retained proof must not let a paid
// cohort skip its expiry finalization. All exact money still uses that cohort's
// unchanged individual allocation and the ordinary fallback owner.
func TestLegacyFinancialCohortRetainedPartialKeepsContinuation(t *testing.T) {
	for _, unknownDirection := range []bool{false, true} {
		env := server.DefaultTestEnv()
		env.RerunCount = 0
		env.Run(t, func(t testing.TB) {
			ctx := t.Context()
			f := legacyFinancialCohortSeed(t, ctx, 8)
			id := f.ids[3]
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL,
                    usage_origin_is_source=CASE WHEN $3 THEN NULL ELSE usage_origin_is_source END WHERE contract_id=$1`,
					id, server.NowUtc().Add(-61*time.Minute), unknownDirection))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET checkpoint=(party='source'),close_time=$2 WHERE contract_id=$1`, id, server.NowUtc()))
				fresh, err := prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-5*time.Minute))
				if err != nil || fresh == nil || !fresh.usageUnverifiedRetained {
					t.Fatal("paid fixture did not retain its original partial proof", fresh, err)
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			proof, _ := readContractExpiryTestSnapshot(t, ctx, id)
			healthyIds := append(append([]server.Id{}, f.ids[:3]...), f.ids[4:]...)
			healthyReports := legacyFinancialCohortReports(ctx, healthyIds)
			wire, closeWire := legacyFinancialRunProtocolBind(t, ctx)
			defer closeWire()
			before := contractClosedCounter.Snapshot()
			wire.enabled(true)
			attempts, err := flushLegacySettlementCohort(ctx, f.ids)
			wire.enabled(false)
			if err != nil || len(attempts) != 4 || !attempts[3].fallback || attempts[3].completed || attempts[3].busy {
				t.Fatal("retained paid partial bypassed read-only continuation admission", unknownDirection, attempts, err)
			}
			for _, attempt := range attempts[:3] {
				if !attempt.completed || attempt.fallback || attempt.busy || attempt.financialWriteRollback {
					t.Fatal("retained paid partial lost its healthy prefix", attempt)
				}
			}
			prefix := f
			prefix.ids = f.ids[:3]
			prefix.reports = legacyFinancialCohortReports(ctx, prefix.ids)
			legacyFinancialCohortRequire(t, ctx, prefix, legacyFinancialCohortCompleted(prefix.ids))
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND provider_usage=$2::jsonb
                    AND EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
                    AND (SELECT count(*)=2 AND bool_and(used_transfer_byte_count=3 AND checkpoint=(party='source'))
                        FROM contract_close WHERE contract_id=$1)
                    FROM transfer_contract WHERE contract_id=$1`, id, proof).Scan(&exact))
				if !exact {
					t.Fatal("paid batch changed its refused partial or retained proof")
				}
			})
			wire.enabled(true)
			page, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, nil, 8)
			wire.enabled(false)
			if err != nil || page.Completed != 5 || page.Failed != 0 || page.BusyOrGone != 0 || page.More || page.Cursor != nil ||
				page.FinancialCohortWriteRollbacks != 0 {
				t.Fatal("retained paid partial blocked its healthy tail", page, err)
			}
			if !bytes.Equal(healthyReports, legacyFinancialCohortReports(ctx, healthyIds)) {
				t.Fatal("retained paid continuation changed a healthy original report")
			}
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(NOT checkpoint AND used_transfer_byte_count=3)
                    FROM contract_close WHERE contract_id=$1`, id).Scan(&exact))
				if !exact {
					t.Fatal("retained paid continuation changed byte counts or kept a checkpoint")
				}
			})
			afterProof, _ := readContractExpiryTestSnapshot(t, ctx, id)
			if !bytes.Equal(proof, afterProof) {
				t.Fatal("retained paid continuation recomputed original usage")
			}
			f.reports = legacyFinancialCohortReports(ctx, f.ids)
			if unknownDirection {
				f.excludedUsage = map[server.Id]bool{id: true}
			}
			legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
			for route, counts := range wire.snapshot() {
				if counts["transactions_with_writes_rolled_back"] != 0 || counts["rollback_commands_observed"] != 0 ||
					counts["connections_closed_with_active_writes"] != 0 || counts["connections_closed_after_begin_without_end_command"] != 0 {
					t.Fatal("retained paid partial rolled back a healthy write", route, counts)
				}
			}
			after := contractClosedCounter.Snapshot()
			if !before.Stable || !after.Stable || after.Confirmed-before.Confirmed != 8 ||
				after.Uncertain != before.Uncertain || after.Untracked != before.Untracked {
				t.Fatal("retained paid continuation changed commit counting", before, after)
			}
			replay, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, nil, 8)
			if err != nil || replay.Completed != 0 || replay.Visited != 0 || replay.More || replay.Cursor != nil {
				t.Fatal("retained paid continuation repeated on replay", replay, err)
			}
			legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		})
	}
}

// Readiness follows the accepted outcome, so an adjudicated source needs no
// invented destination. Its already-final selected report remains batchable.
func TestLegacySourceBatchRetainedAdjudicationKeepsMissingUnselectedPeer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := legacySourceBatchSeed(t, ctx, 2)
		id := f.ids[0]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=NULL,create_time=$2 WHERE contract_id=$1`, id, server.NowUtc().Add(-61*time.Minute)))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM contract_close WHERE contract_id=$1 AND party='destination'`, id))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET outcome='dispute_resolved_to_source' WHERE contract_id=$1`, id))
			fresh, err := prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-5*time.Minute))
			if err != nil || fresh == nil || !fresh.usageUnverifiedRetained {
				t.Fatal("adjudicated fixture did not retain expiry", fresh, err)
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		proof, _ := readContractExpiryTestSnapshot(t, ctx, id)
		reports := legacyFinancialCohortReports(ctx, f.ids)
		owned := context.WithValue(ctx, legacySettlementCloseScopeKey{}, f.owner)
		attempts, err := flushLegacySourceSettlementBatch(owned, f.ids)
		if err != nil || len(attempts) != 2 {
			t.Fatal("selected final adjudication lost source batch eligibility", attempts, err)
		}
		for _, attempt := range attempts {
			if !attempt.completed || attempt.fallback || attempt.busy || attempt.financialWriteRollback {
				t.Fatal("adjudicated readiness required an unselected peer", attempt)
			}
		}
		after, _ := readContractExpiryTestSnapshot(t, ctx, id)
		if !bytes.Equal(proof, after) || !bytes.Equal(reports, legacyFinancialCohortReports(ctx, f.ids)) {
			t.Fatal("adjudicated batch invented a peer or recomputed retained usage")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome='dispute_resolved_to_source' AND usage_origin_is_source IS NULL
                AND NOT EXISTS(SELECT 1 FROM contract_close WHERE contract_id=$1 AND party='destination')
                AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=ANY($2))
                AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=ANY($2))
                AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($2))
                FROM transfer_contract WHERE contract_id=$1`, id, f.ids).Scan(&exact))
			if !exact {
				t.Fatal("adjudicated retained source batch changed accepted authority or money")
			}
		})
	})
}
