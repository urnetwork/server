// The pending-intent owner applies the existing expiry exclusion when legacy
// direction is absent. It never derives usage direction from monetary custody.
package model

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Both public expiry paths deliberately yield to an existing intent. Only its
// ordinary worker can retain the zero-credit proof and finish unchanged money.
// Fixed old/future clocks exercise policy without waiting for real expiry.
func TestLegacySettlementMissingDirectionExpiryPolicy(t *testing.T) {
	for _, test := range []struct {
		name              string
		createdAge        time.Duration
		reportAge         time.Duration
		explicitDeadline  bool
		deadlineRemaining time.Duration
		due               bool
	}{
		{name: "legacy absolute", createdAge: 2 * time.Hour, due: true},
		{name: "legacy quiet", createdAge: 10 * time.Minute, reportAge: 10 * time.Minute, due: true},
		{name: "legacy recent"},
		{name: "explicit expired", explicitDeadline: true, deadlineRemaining: -time.Hour, due: true},
		{name: "explicit future", createdAge: 2 * time.Hour, explicitDeadline: true, deadlineRemaining: time.Hour},
	} {
		env := server.DefaultTestEnv()
		env.RerunCount = 0
		env.Run(t, func(t testing.TB) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			f := legacyFinancialCohortSeed(t, ctx, 8)
			id := f.ids[3]
			now := server.NowUtc()
			var deadline *time.Time
			if test.explicitDeadline {
				value := now.Add(test.deadlineRemaining)
				deadline = &value
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=NULL,create_time=$2,expiration_time=$3 WHERE contract_id=$1`,
					id, now.Add(-test.createdAge), deadline))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, id, now.Add(-test.reportAge)))
			}, server.TxReadCommitted, server.OptNoRetry())
			f.reports = legacyFinancialCohortReports(ctx, f.ids)
			count, _, err := ForceCloseOpenContractIdsPage(ctx, now.Add(-5*time.Minute), 16, 1, 0, 0, nil)
			if err != nil || count != 0 {
				t.Fatal("expiry sweep bypassed existing intent custody", test.name, count, err)
			}
			repair, err := RepairContractExpiry(ctx, ContractExpiryRepairRequest{
				ExpectedPayerNetworkId: f.payer.sourceNetworkId, ContractIds: []server.Id{id}, Apply: true,
			})
			if err != nil || len(repair.Contracts) != 1 || repair.Contracts[0].Status != "legacy_intent_present" || repair.Contracts[0].ProofCommitted {
				t.Fatal("explicit repair bypassed existing intent custody", test.name, repair, err)
			}
			legacyFinancialCohortRequire(t, ctx, f, map[server.Id]bool{})
			wire, closeWire := legacyFinancialRunProtocolBind(t, ctx)
			defer closeWire()
			before := contractClosedCounter.Snapshot()
			wire.enabled(true)
			page, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, nil, 8)
			wire.enabled(false)
			wantCompleted, wantVisited, wantFailed := 3, 4, 1
			if test.due {
				wantCompleted, wantVisited, wantFailed = 8, 8, 0
				f.excludedUsage = map[server.Id]bool{id: true}
			}
			if err != nil || page.Completed != wantCompleted || page.Visited != wantVisited || page.Failed != wantFailed ||
				page.BusyOrGone != 0 || page.FinancialCohortCompleted != 3 || page.FinancialCohortFallbacks != 1 || page.FinancialCohortWriteRollbacks != 0 {
				t.Fatal("pending legacy direction did not follow ordinary expiry policy", test.name, page, err)
			}
			for route, counts := range wire.snapshot() {
				if counts["transactions_with_writes_rolled_back"] != 0 || counts["connections_closed_with_active_writes"] != 0 ||
					counts["connections_closed_after_begin_without_end_command"] != 0 || test.due && counts["rollback_commands_observed"] != 0 {
					t.Fatal("direction admission attempted writes before its own eligibility", test.name, route, counts)
				}
			}
			legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids[:wantCompleted]))
			server.Db(ctx, func(conn server.PgConn) {
				var unchangedDirection, unverified bool
				server.Raise(conn.QueryRow(ctx, `SELECT usage_origin_is_source IS NULL,usage_unverified FROM transfer_contract WHERE contract_id=$1`, id).Scan(&unchangedDirection, &unverified))
				if !unchangedDirection || unverified != test.due {
					t.Fatal("worker inferred direction or prepared an ineligible exclusion", test.name)
				}
			})
			after := contractClosedCounter.Snapshot()
			if !before.Stable || !after.Stable || after.Confirmed-before.Confirmed != uint64(wantCompleted) ||
				after.Uncertain != before.Uncertain || after.Untracked != before.Untracked {
				t.Fatal("legacy direction retirement changed commit custody", test.name, before, after)
			}
			if test.due {
				proof, snapshot := readContractExpiryTestSnapshot(t, ctx, id)
				if snapshot.ByteCount != 0 || snapshot.Expiry != nil || snapshot.ExcludedReason != "expired_unconfirmed" {
					t.Fatal("unknown legacy direction manufactured delivery evidence", test.name, snapshot)
				}
				replay, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, nil, 8)
				if err != nil || replay.Visited != 0 || replay.Completed != 0 || replay.Cursor != nil || replay.More {
					t.Fatal("expired legacy intent replay repeated work", test.name, replay, err)
				}
				replayedProof, _ := readContractExpiryTestSnapshot(t, ctx, id)
				if !bytes.Equal(proof, replayedProof) {
					t.Fatal("replay changed the immutable legacy exclusion", test.name)
				}
				legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
			}
		})
	}
}

// An earlier expiry owns a retained exclusion even when direction is absent.
// Cohort admission must consume that proof before refusing missing direction.
func TestLegacyFinancialCohortRetainedMissingDirectionExpiry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := legacyFinancialCohortSeed(t, ctx, 8)
		id := f.ids[3]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=NULL,create_time=$2 WHERE contract_id=$1`, id, server.NowUtc().Add(-2*time.Hour)))
			fresh, err := prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-5*time.Minute))
			server.Raise(err)
			if fresh == nil || !fresh.usageUnverifiedRetained {
				t.Fatal("fixture did not retain existing expiry authority")
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		proof, _ := readContractExpiryTestSnapshot(t, ctx, id)
		f.excludedUsage = map[server.Id]bool{id: true}
		attempts, err := flushLegacySettlementCohort(ctx, f.ids)
		if err != nil || len(attempts) != len(f.ids) {
			t.Fatal("retained missing-direction expiry lost cohort admission", attempts, err)
		}
		for _, attempt := range attempts {
			if !attempt.completed || attempt.fallback || attempt.busy || attempt.financialWriteRollback {
				t.Fatal("valid retained exclusion entered individual fallback", attempt)
			}
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		after, _ := readContractExpiryTestSnapshot(t, ctx, id)
		if !bytes.Equal(proof, after) {
			t.Fatal("cohort rewrote the retained expiry exclusion")
		}
	})
}

// A stale owner can repair scheduling metadata only. NULL payer metadata on a
// companion still resolves through its real grant before any expiry proof write.
func TestLegacySettlementMissingDirectionRekeysBeforeExpiry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := legacyFinancialCohortSeed(t, ctx, 1)
		id := f.ids[0]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=NULL,payer_network_id=NULL,
            companion_contract_id=$2,create_time=$3 WHERE contract_id=$1`, id, server.NewId(), server.NowUtc().Add(-2*time.Hour)))
		}, server.TxReadCommitted, server.OptNoRetry())
		staleOwner := ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: f.payer.sourceId}
		stale := context.WithValue(ctx, legacySettlementCloseScopeKey{}, staleOwner)
		completed, busy, gate, err := flushLegacySettlement(stale, id)
		if err != nil || completed || !busy || gate != legacySettlementBusyAdmission {
			t.Fatal("stale owner entered missing-direction expiry", completed, busy, gate, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, map[server.Id]bool{})
		server.Db(ctx, func(conn server.PgConn) {
			var retained bool
			server.Raise(conn.QueryRow(ctx, `SELECT c.payer_network_id IS NULL AND c.usage_origin_is_source IS NULL
            AND NOT c.usage_unverified AND c.provider_usage IS NULL AND i.payer_network_id=$2 AND i.source_client_id=$3
            FROM transfer_contract c JOIN legacy_settlement_intent i USING(contract_id) WHERE c.contract_id=$1`, id, f.payer.sourceNetworkId, f.payer.sourceId).Scan(&retained))
			if !retained {
				t.Fatal("rekey changed financial or usage authority")
			}
		})
		page, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, nil, 8)
		if err != nil || page.Completed != 1 || page.Failed != 0 || page.BusyOrGone != 0 {
			t.Fatal("actual grant owner did not retire the rekeyed legacy intent", page, err)
		}
		f.excludedUsage = map[server.Id]bool{id: true}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
	})
}

// Preparing an exclusion cannot bypass insufficient own escrow. Proof, intent
// deletion and any attempted financial work roll back under that one owner.
func TestLegacySettlementMissingDirectionExpiryRollbackKeepsCustody(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := legacyFinancialCohortSeed(t, ctx, 2)
		id := f.ids[0]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=NULL,create_time=$2 WHERE contract_id=$1`, id, server.NowUtc().Add(-2*time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=2 WHERE contract_id=$1`, id))
		}, server.TxReadCommitted, server.OptNoRetry())
		f.reservationAmounts[0] = 2
		first, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, nil, 8)
		if err != nil || first.Visited != 1 || first.Completed != 0 || first.Failed != 1 || first.Cursor == nil || first.Cursor.ContractId != id {
			t.Fatal("expiry proof changed ordinary insufficient-escrow refusal", first, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, map[server.Id]bool{})
		server.Db(ctx, func(conn server.PgConn) {
			var retained bool
			server.Raise(conn.QueryRow(ctx, `SELECT NOT c.usage_unverified AND c.provider_usage IS NULL
            AND i.failure_code='accounting' AND i.next_attempt_time>statement_timestamp() AT TIME ZONE 'UTC'
            FROM transfer_contract c JOIN legacy_settlement_intent i USING(contract_id) WHERE c.contract_id=$1`, id).Scan(&retained))
			if !retained {
				t.Fatal("failed money detached expiry proof or lost accounting custody")
			}
		})
		tail, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, first.Cursor, 8)
		if err != nil || tail.Completed != 1 || tail.Failed != 0 || tail.Cursor != nil || tail.More {
			t.Fatal("rejected expiry held the same payer's healthy tail", tail, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids[1:]))
	})
}

// Grant refusal remains read-only, and the explicit financial drain retains
// its documented policy even after ordinary workers gain expiry preparation.
func TestLegacySettlementMissingDirectionBusyAndExplicitDrainKeepProof(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := legacyFinancialCohortSeed(t, ctx, 1)
		id := f.ids[0]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=NULL,create_time=$2 WHERE contract_id=$1`, id, server.NowUtc().Add(-2*time.Hour)))
		}, server.TxReadCommitted, server.OptNoRetry())
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.balanceIds[0]))
		completed, busy, _, err := flushLegacySettlement(ctx, id)
		if err != nil || completed || !busy {
			t.Fatal("missing direction bypassed held grant custody", completed, busy, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, map[server.Id]bool{})
		server.Raise(held.Rollback(ctx))
		request := LegacySettlementDrainRequest{ExpectedPayerNetworkId: f.payer.sourceNetworkId, ContractIds: []server.Id{id}}
		preview, err := DrainLegacySettlements(ctx, request)
		if err != nil || len(preview.Contracts) != 1 || preview.Contracts[0].Status != "eligible" {
			t.Fatal("explicit drain lost its existing financial eligibility", preview, err)
		}
		request.Apply = true
		applied, err := DrainLegacySettlements(ctx, request)
		if err != nil || len(applied.Contracts) != 1 || applied.Contracts[0].Status != "failed" || applied.Contracts[0].FinancialCommitAcknowledged {
			t.Fatal("explicit financial drain gained expiry preparation authority", applied, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, map[server.Id]bool{})
		page, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, nil, 8)
		if err != nil || page.Completed != 1 || page.Failed != 0 || page.BusyOrGone != 0 {
			t.Fatal("ordinary worker could not retire unchanged intent after explicit refusal", page, err)
		}
		f.excludedUsage = map[server.Id]bool{id: true}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
	})
}

// Fresh partials retain ordinary terminal-report guards. Expiry never accepts
// malformed counts, an unknown party, or a missing adjudicated authority.
func TestLegacySettlementMissingDirectionKeepsReportRefusals(t *testing.T) {
	for _, mode := range []string{"checkpoint", "missing_peer", "negative", "unknown_party", "adjudicated_checkpoint", "adjudicated_missing_source"} {
		env := server.DefaultTestEnv()
		env.RerunCount = 0
		env.Run(t, func(t testing.TB) {
			ctx := t.Context()
			f := legacyFinancialCohortSeed(t, ctx, 1)
			id := f.ids[0]
			now := server.NowUtc()
			created := now
			if mode == "negative" || mode == "unknown_party" || mode == "adjudicated_missing_source" {
				created = now.Add(-2 * time.Hour)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=NULL,create_time=$2,expiration_time=NULL WHERE contract_id=$1`, id, created))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, id, now))
				switch mode {
				case "checkpoint", "adjudicated_checkpoint":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET checkpoint=true WHERE contract_id=$1 AND party='source'`, id))
				case "missing_peer":
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM contract_close WHERE contract_id=$1 AND party='destination'`, id))
				case "negative":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=-1 WHERE contract_id=$1 AND party='source'`, id))
				case "unknown_party":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET party='unknown_party' WHERE contract_id=$1 AND party='source'`, id))
				case "adjudicated_missing_source":
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM contract_close WHERE contract_id=$1 AND party='source'`, id))
				}
				if mode == "adjudicated_checkpoint" || mode == "adjudicated_missing_source" {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET outcome='dispute_resolved_to_source' WHERE contract_id=$1`, id))
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			f.reports = legacyFinancialCohortReports(ctx, f.ids)
			wire, closeWire := legacyFinancialRunProtocolBind(t, ctx)
			defer closeWire()
			wire.enabled(true)
			completed, busy, _, err := flushLegacySettlement(ctx, id)
			wire.enabled(false)
			if err == nil || completed || busy {
				t.Fatal("expiry exclusion replaced a required original report", mode, completed, busy, err)
			}
			for route, counts := range wire.snapshot() {
				if counts["transactions_with_writes_committed"] != 0 || counts["transactions_with_writes_rolled_back"] != 0 {
					t.Fatal("invalid original reports entered expiry writes", mode, route, counts)
				}
			}
			legacyFinancialCohortRequire(t, ctx, f, map[server.Id]bool{})
		})
	}
}

// Source-owned intents use the same per-contract admission after source-batch
// fallback. Legacy unknown direction remains zero-credit and creates no money.
func TestLegacySourceSettlementMissingDirectionExpiry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		ids := []server.Id{server.NewId(), server.NewId()}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
            (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time)
            SELECT id,$2,$3,$4,$5,16,$6 FROM unnest($1::uuid[]) AS requested(id)`,
				ids, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, server.NowUtc().Add(-2*time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint)
            SELECT id,party,3,false FROM unnest($1::uuid[]) AS requested(id)
            CROSS JOIN (VALUES ('source'),('destination')) AS reports(party)`, ids))
			for _, id := range ids {
				server.Raise(queueLegacySettlementInTx(ctx, tx, id, ContractOutcomeSettled, false))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		reports := legacyFinancialCohortReports(ctx, ids)
		owner := ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: f.sourceId}
		result, err := runLegacyCloseSettlementPages(ctx, owner, nil, 8, true)
		if err != nil || result.Completed != len(ids) || result.Failed != 0 || result.BusyOrGone != 0 || result.More || result.Cursor != nil {
			t.Fatal("ordinary source owner did not retire unknown legacy direction", result, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
            (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'
                AND usage_origin_is_source IS NULL AND usage_unverified AND provider_usage->>'excluded_reason'='expired_unconfirmed'
                AND (provider_usage->>'byte_count')::bigint=0 AND provider_usage->'providers'='[]'::jsonb)=$2
            AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($1))
            AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=ANY($1))
            AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=ANY($1))
            AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=ANY($1))`, ids, len(ids)).Scan(&exact))
			if !exact {
				t.Fatal("source expiry inferred usage or created financial custody")
			}
		})
		if !bytes.Equal(reports, legacyFinancialCohortReports(ctx, ids)) {
			t.Fatal("source expiry rewrote an original report")
		}
		replay, err := runLegacyCloseSettlementPages(ctx, owner, nil, 8, true)
		if err != nil || replay.Visited != 0 || replay.Completed != 0 || replay.Cursor != nil || replay.More {
			t.Fatal("source legacy expiry replay repeated work", replay, err)
		}
	})
}
