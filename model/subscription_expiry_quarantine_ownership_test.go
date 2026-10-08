// An expiry quarantine advances legacy reservation revisions even though it
// pays nothing. Its real recovery path must share the grant's financial owner.
package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// A single scheduling refusal never authorizes quarantine. Unrelated joined
// failures retain their existing policy instead of borrowing that exception.
func TestForceCloseOwnershipBusyClassification(t *testing.T) {
	other := errors.New("synthetic independent close failure")
	cleanupErr := errors.New("synthetic still-open verification")
	for _, test := range []struct {
		err        error
		quarantine int
	}{
		{err: errTransferBalanceOwnershipBusy},
		{err: fmt.Errorf("wrapped: %w", errTransferBalanceOwnershipBusy)},
		{err: errors.Join(errTransferBalanceOwnershipBusy, other), quarantine: 1},
		{err: other, quarantine: 1},
	} {
		quarantined, checked := 0, 0
		err := finishForceCloseContract(test.err, func() error {
			quarantined++
			return nil
		}, func() error {
			checked++
			return cleanupErr
		})
		if quarantined != test.quarantine || checked != 1 || !errors.Is(err, test.err) || !errors.Is(err, cleanupErr) {
			t.Fatal("ownership refusal changed quarantine authority or final-state verification", quarantined, checked, err)
		}
	}
}

// Native close owns a private provider publication key. Losing that admission
// must retain debit/payout custody even when its holder releases before the
// expiry code decides whether to quarantine the contract.
func TestExpiryPublicationBusyRetainsFinancialCustody(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		var reruns atomic.Int32
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 100)
		neighbor := createRedisAdmissionTest(ctx, f, 200)
		AddToStream(ctx, contract.ContractId, f.sourceId, f.destinationId, nil)
		server.Raise(CloseContract(ctx, contract.ContractId, f.sourceId, 11, true))
		server.Raise(CloseContract(ctx, contract.ContractId, f.destinationId, 11, true))
		cutoff := server.NowUtc().Add(time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			old := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, contract.ContractId, old))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, contract.ContractId, old))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, neighbor.ContractId, cutoff.Add(time.Hour)))
		}, server.TxReadCommitted, server.OptNoRetry())
		key := task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contract.ContractId))
		release := holdFinancialTestOwner(t, ctx, []server.PgOwnershipKey{key})
		defer release()
		var refused atomic.Int32
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipRefused && slices.Contains(event.Keys, key) {
				refused.Add(1)
				// Join the real holder before returning the observed refusal.
				// A later successful admission cannot change its meaning.
				release()
			}
		})
		selected, err := ForceCloseOpenContractIds(observed, cutoff, 10, 1, 1, 0)
		if selected != 1 || !errors.Is(err, errTransferBalanceOwnershipBusy) || refused.Load() != 1 {
			t.Fatal("native expiry did not exercise its exact publication refusal", selected, refused.Load(), err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NULL AND provider_usage IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM pending_task WHERE run_once_key=$2)
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1 AND settled)
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$3)=1000
 FROM transfer_contract WHERE contract_id=$1`, contract.ContractId, task.RunOnce("legacy_provider_totals", contract.ContractId).String(), f.balanceId).Scan(&exact))
			if !exact || Testing_NetEscrowByteCount(ctx, f.balanceId) != 300 {
				t.Fatal("ownership refusal became a terminal no-payout quarantine")
			}
		})
		selected, err = ForceCloseOpenContractIds(ctx, cutoff, 10, 1, 1, 0)
		if selected != 1 || err != nil {
			t.Fatal("retained native expiry did not resume ordinary settlement", selected, err)
		}
		applied, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
		if err != nil || applied != 1 || released != 1 || busy {
			t.Fatal("resumed native expiry did not debit exactly once", applied, released, busy, err)
		}
		drain, err := legacyFinancialDrainOwners(t, ctx, 1, nil)
		if err != nil || drain.Finished != 1 || drain.Remaining != 0 {
			t.Fatal("resumed native expiry lost its actual provider completion", drain, err)
		}
		requireLegacyProviderDurability(t, ctx, f, contract.ContractId, 11)
		credit, pending, appliedRows := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 989 || pending+appliedRows != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 200 {
			t.Fatal("resumed native expiry changed exact debit or neighboring reservation", credit, pending, appliedRows)
		}
		_, snapshot := readContractExpiryTestSnapshot(t, ctx, contract.ContractId)
		if snapshot.ByteCount != 11 || snapshot.Expiry == nil || len(snapshot.Expiry.Reports) != 2 {
			t.Fatal("resumed native expiry changed original usage")
		}
		selected, err = ForceCloseOpenContractIds(ctx, cutoff, 10, 1, 1, 0)
		if selected != 0 || err != nil {
			t.Fatal("native expiry replay reclaimed a completed contract", selected, err)
		}
		applied, released, busy, err = flushTransferDebitBalance(ctx, f.balanceId)
		if err != nil || applied != 0 || released != 0 || busy || reruns.Load() != 0 {
			t.Fatal("native expiry replay repeated consumption or automatically retried a transaction", applied, released, busy, err, reruns.Load())
		}
		requireLegacyProviderDurability(t, ctx, f, contract.ContractId, 11)
	})
}

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
