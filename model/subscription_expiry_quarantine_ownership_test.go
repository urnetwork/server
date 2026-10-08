// An expiry quarantine advances legacy reservation revisions even though it
// pays nothing. Its real recovery path must share the grant's financial owner.
package model

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestTransferBalanceOwnerCoversExpiryQuarantine(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		var reruns atomic.Int32
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		neighbor, posts := createNetEscrowOrderingTestContract(ctx, f, 200)
		server.RunPosts(ctx, posts...)
		AddToStream(ctx, contract.ContractId, f.sourceId, f.destinationId, nil)
		server.Raise(CloseContract(ctx, contract.ContractId, f.sourceId, 3, true))
		server.Raise(CloseContract(ctx, contract.ContractId, f.destinationId, 3, true))
		other := newNetEscrowOrderingTestFixture(t, ctx)
		otherId := newLegacyPayerTestIntent(t, ctx, other, server.NewId(), 100, 11)
		old := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
		cutoff := server.NowUtc().Add(time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, contract.ContractId, old))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, contract.ContractId, old))
			// The synthetic cutoff also admits the finalized reports from the
			// refused first pass. Keep the unrelated live reservation newer.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, neighbor.ContractId, cutoff.Add(time.Hour)))
			// A real publication failure sends the ordinary expiry continuation
			// into its established no-payout quarantine. No financial helper or
			// ownership call is replaced, and neither report is invented.
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION test_expiry_quarantine_queue_refusal() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'synthetic expiry queue publication unavailable'; END; $$;
 CREATE TRIGGER test_expiry_quarantine_queue_refusal BEFORE INSERT ON legacy_settlement_intent
 FOR EACH ROW EXECUTE FUNCTION test_expiry_quarantine_queue_refusal()`))
		}, server.TxReadCommitted, server.OptNoRetry())
		defer func() {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer stop()
			server.HandleError(func() {
				server.Tx(cleanup, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(cleanup, `DROP TRIGGER test_expiry_quarantine_queue_refusal ON legacy_settlement_intent;
 DROP FUNCTION test_expiry_quarantine_queue_refusal()`))
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { t.Error("quarantine fixture cleanup failed", err) })
		}()
		var revision int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$1`, f.balanceId).Scan(&revision))
		})
		release := holdTransferBalanceTestOwner(t, ctx, []server.Id{f.balanceId})
		defer release()
		var refused atomic.Int32
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipRefused && slices.Contains(event.Keys, server.NewPgOwnershipKey("transfer_balance", f.balanceId)) {
				refused.Add(1)
			}
		})
		selected, err := ForceCloseOpenContractIds(observed, cutoff, 10, 1, 1, 0)
		if selected != 1 || err == nil || !strings.Contains(err.Error(), "synthetic expiry queue publication unavailable") || refused.Load() != 1 {
			t.Fatal("actual expiry quarantine did not refuse the held revision owner", selected, refused.Load(), err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND usage_unverified AND provider_usage IS NOT NULL
 AND (SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$2)=$3
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=1000
 AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id IN ($1,$4) AND settled)
 FROM transfer_contract WHERE contract_id=$1`, contract.ContractId, f.balanceId, revision, neighbor.ContractId).Scan(&exact))
			if !exact || Testing_NetEscrowByteCount(ctx, f.balanceId) != 300 {
				t.Fatal("refused quarantine changed money, terminal custody or shared revision")
			}
		})
		completed, busy, _, err := flushLegacySettlement(ctx, otherId)
		if err != nil || !completed || busy {
			t.Fatal("held quarantine owner blocked an independent payer", completed, busy, err)
		}
		requireLegacySettlementTestState(t, ctx, other, otherId, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, other, otherId, 11)
		release()
		selected, err = ForceCloseOpenContractIds(ctx, cutoff, 10, 1, 1, 0)
		if selected != 1 || err == nil || !strings.Contains(err.Error(), "synthetic expiry queue publication unavailable") {
			t.Fatal("released expiry owner lost its original quarantine diagnostic", selected, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome='settled' AND usage_unverified
 AND (SELECT revision FROM transfer_balance_net_escrow_revision WHERE balance_id=$2)>$3
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=1000
 AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id IN ($1,$4) AND settled)
 AND (SELECT outcome IS NULL FROM transfer_contract WHERE contract_id=$4)
 FROM transfer_contract WHERE contract_id=$1`, contract.ContractId, f.balanceId, revision, neighbor.ContractId).Scan(&exact))
			if !exact || Testing_NetEscrowByteCount(ctx, f.balanceId) != 200 {
				t.Fatal("quarantine changed its no-payout policy or neighboring reservation")
			}
		})
		_, snapshot := readContractExpiryTestSnapshot(t, ctx, contract.ContractId)
		if snapshot.ByteCount != 3 || snapshot.Expiry == nil || len(snapshot.Expiry.Reports) != 2 {
			t.Fatal("quarantine changed the original bounded usage proof")
		}
		if _, _, found := GetStream(ctx, contract.ContractId); found {
			t.Fatal("completed quarantine retained the old stream")
		}
		selected, err = ForceCloseOpenContractIds(ctx, cutoff, 10, 1, 1, 0)
		if selected != 0 || err != nil || reruns.Load() != 0 {
			t.Fatal("quarantine replay repeated work or automatically retried a transaction", selected, err, reruns.Load())
		}
	})
}
