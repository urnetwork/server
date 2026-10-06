// Actual report and expiry owners recover canceled endpoints without changing
// authenticated usage, collapsing equal work, or losing payer/provider custody.
package model

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Two equal original checkpoints lose their replies after commit. Stable
// identities permit replay before a canceled endpoint leaves expiry to finish.
func TestReportReplayThenCanceledClientExpiryConservesGrant(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
		})
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 1000)
		server.RunPosts(ctx, posts...)
		requireLegacySettlementTestState(t, ctx, f, contract.ContractId, false, false, 1000, 1000)
		source := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.sourceId, AckedByteCount: 800}
		if applied, err := CloseContractWithReport(ctx, source); err != nil || !applied {
			t.Fatal("source's original final report did not commit")
		}
		checkpoints := []ContractCloseReport{
			{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.destinationId, AckedByteCount: 400, Checkpoint: true},
			{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.destinationId, AckedByteCount: 400, Checkpoint: true},
		}
		for _, report := range checkpoints {
			// Commit the genuine report owner, deliberately omitting its public
			// continuation/reply. There is no transport timing or mock ledger.
			server.Tx(ctx, func(tx server.PgTx) {
				applied, err := closeContractReportInTx(ctx, tx, report)
				server.Raise(err)
				if !applied {
					t.Fatal("distinct equal work did not acquire original report custody")
				}
			}, server.TxReadCommitted)
		}
		for _, report := range checkpoints {
			if applied, err := CloseContractWithReport(ctx, report); err != nil || applied {
				t.Fatal("lost-reply retry changed authenticated work")
			}
		}
		assertCloseReportCounts(t, ctx, contract.ContractId, 3, 1600)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		final := ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId, ClientId: f.destinationId}
		if applied, err := CloseContractWithReport(canceled, final); applied || !errors.Is(err, context.Canceled) {
			t.Fatal("canceled endpoint acquired a final report")
		}
		var cutoff time.Time
		var originals []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT GREATEST(c.create_time,max(r.close_time))
				FROM transfer_contract c JOIN contract_close r USING(contract_id)
				WHERE c.contract_id=$1 GROUP BY c.create_time`, contract.ContractId).Scan(&cutoff))
			server.Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(r) ORDER BY report_id)
				FROM contract_close_report_evidence r WHERE contract_id=$1`, contract.ContractId).Scan(&originals))
		})
		// The exact persisted quiet boundary is supplied directly. No sleep or
		// short negative timeout decides whether the owner may retire it.
		closed, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
		if err != nil || closed != 0 {
			t.Fatal("expiry did not leave the real legacy financial continuation")
		}
		proofBefore, proof := readContractExpiryTestSnapshot(t, ctx, contract.ContractId)
		if proof.Expiry == nil || len(proof.Expiry.Reports) != 2 || proof.ByteCount != 800 ||
			proof.Expiry.Reports[ContractPartySource].Checkpoint || !proof.Expiry.Reports[ContractPartyDestination].Checkpoint ||
			proof.Expiry.Reports[ContractPartySource].ByteCount != 800 || proof.Expiry.Reports[ContractPartyDestination].ByteCount != 800 {
			t.Fatal("expiry proof collapsed equal work or authenticated synthetic finality")
		}
		complete, busy, _, err := flushLegacySettlement(ctx, contract.ContractId)
		if err != nil || !complete || busy {
			t.Fatal("identified replay stranded fully funded ordinary usage")
		}
		requireLegacySettlementTestState(t, ctx, f, contract.ContractId, false, true, 200, 0)
		requireLegacyProviderDurability(t, ctx, f, contract.ContractId, 800, 800)
		for _, report := range append(checkpoints, source) {
			if applied, err := CloseContractWithReport(ctx, report); err != nil || applied {
				t.Fatal("terminal original retry repeated financial work")
			}
		}
		proofAfter, _ := readContractExpiryTestSnapshot(t, ctx, contract.ContractId)
		if !bytes.Equal(proofBefore, proofAfter) {
			t.Fatal("settlement changed original completed-work proof")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var after []byte
			server.Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(r) ORDER BY report_id)
				FROM contract_close_report_evidence r WHERE contract_id=$1`, contract.ContractId).Scan(&after))
			if !bytes.Equal(originals, after) {
				t.Fatal("expiry or replay changed retained original reports")
			}
		})
		// Released capacity is usable by a healthy same-payer contract. Its
		// legacy empty-id final reports still use the ordinary compatibility path.
		healthy, healthyPosts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, healthyPosts...)
		server.Raise(CloseContract(ctx, healthy.ContractId, f.sourceId, 11, false))
		server.Raise(CloseContract(ctx, healthy.ContractId, f.destinationId, 11, false))
		complete, busy, _, err = flushLegacySettlement(ctx, healthy.ContractId)
		if err != nil || !complete || busy {
			t.Fatal("expired peer prevented healthy neighbor progress")
		}
		requireLegacySettlementTestState(t, ctx, f, healthy.ContractId, false, true, 189, 0)
		projectLegacyProviderTotalsForTest(t, ctx)
		projectLegacyProviderTotalsForTest(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count=811 AND provided_net_revenue_nano_cents=811
				FROM account_balance WHERE network_id=$1`, f.destinationNetworkId).Scan(&exact))
			if !exact {
				t.Fatal("recovery duplicated or lost the durable provider projection")
			}
		})
	})
}

// A stopped endpoint can leave no report at all. Expiry records that absence,
// releases the entire untouched reservation, and never invents provider work.
func TestReportlessCanceledClientExpiryReleasesWholeGrant(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 1000)
		server.RunPosts(ctx, posts...)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := captureContractExpiryRepair(func() error { return CloseContract(canceled, contract.ContractId, f.sourceId, 0, false) }); err == nil {
			t.Fatal("stopped endpoint unexpectedly committed a final report")
		}
		var cutoff time.Time
		server.Db(ctx, func(conn server.PgConn) {
			var absent bool
			server.Raise(conn.QueryRow(ctx, `SELECT create_time,
				NOT EXISTS(SELECT 1 FROM contract_close WHERE contract_id=$1)
				FROM transfer_contract WHERE contract_id=$1`, contract.ContractId).Scan(&cutoff, &absent))
			if !absent {
				t.Fatal("canceled endpoint left an authenticated report")
			}
		})
		closed, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
		if err != nil || closed != 0 {
			t.Fatal("reportless expiry lost its ordinary financial continuation")
		}
		proofBefore, proof := readContractExpiryTestSnapshot(t, ctx, contract.ContractId)
		if proof.Expiry == nil || len(proof.Expiry.Reports) != 0 || proof.ByteCount != 0 || proof.ExcludedReason != "expired_unconfirmed" {
			t.Fatal("reportless expiry manufactured completed work")
		}
		complete, busy, _, err := flushLegacySettlement(ctx, contract.ContractId)
		if err != nil || !complete || busy {
			t.Fatal("reportless canceled endpoint permanently reserved its grant")
		}
		requireLegacySettlementTestState(t, ctx, f, contract.ContractId, false, true, 1000, 0)
		requireLegacyProviderDurability(t, ctx, f, contract.ContractId, 0)
		complete, _, _, err = flushLegacySettlement(ctx, contract.ContractId)
		if err != nil || complete {
			t.Fatal("reportless recovery replay reclaimed a terminal owner")
		}
		proofAfter, _ := readContractExpiryTestSnapshot(t, ctx, contract.ContractId)
		if !bytes.Equal(proofBefore, proofAfter) {
			t.Fatal("reportless settlement authenticated its synthetic closes")
		}
		healthy, healthyPosts := createNetEscrowOrderingTestContract(ctx, f, 1000)
		server.RunPosts(ctx, healthyPosts...)
		requireLegacySettlementTestState(t, ctx, f, healthy.ContractId, false, false, 1000, 1000)
	})
}
