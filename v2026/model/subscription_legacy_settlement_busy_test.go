package model

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

// Native test-only triggers force a real lock refusal after the unchanged
// financial owner writes its speculative prefix. They do not model a Main lock.
func holdLegacyDrainBusyFence(t testing.TB, ctx context.Context, ids []server.Id, deferred bool) (server.PgTx, func()) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
CREATE TABLE customer_drain_busy_fence (guard_id integer PRIMARY KEY);
INSERT INTO customer_drain_busy_fence VALUES (1);
CREATE TABLE customer_drain_busy_target (contract_id uuid PRIMARY KEY);
CREATE TABLE customer_drain_busy_commit_marker (contract_id uuid PRIMARY KEY);
CREATE FUNCTION customer_drain_busy_sweep() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM customer_drain_busy_target WHERE contract_id=NEW.contract_id) THEN
  IF NOT EXISTS(
   SELECT 1 FROM transfer_contract c
   JOIN transfer_escrow e ON e.contract_id=c.contract_id AND e.balance_id=NEW.balance_id
   JOIN transfer_balance b ON b.balance_id=e.balance_id
   WHERE c.contract_id=NEW.contract_id AND c.outcome IS NOT NULL
    AND c.provider_usage IS NOT NULL AND e.settled
    AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent i WHERE i.contract_id=c.contract_id)
    AND b.balance_byte_count=b.start_balance_byte_count-
      (SELECT COALESCE(sum(s.payout_byte_count),0) FROM transfer_escrow_sweep s WHERE s.balance_id=b.balance_id)
  ) THEN
   RAISE EXCEPTION 'synthetic financial prefix missing';
  END IF;
  IF TG_ARGV[0]='deferred' THEN
   INSERT INTO customer_drain_busy_commit_marker VALUES(NEW.contract_id);
  ELSE
   PERFORM guard_id FROM customer_drain_busy_fence WHERE guard_id=1 FOR UPDATE NOWAIT;
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE FUNCTION customer_drain_busy_commit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM pending_task
   WHERE function_name='github.com/urnetwork/server/model.ApplyLegacyProviderTotals'
     AND (args_json::jsonb->>'contract_id')::uuid=NEW.contract_id) THEN
  RAISE EXCEPTION 'synthetic durable projection prefix missing';
 END IF;
 PERFORM guard_id FROM customer_drain_busy_fence WHERE guard_id=1 FOR UPDATE NOWAIT;
 RETURN NEW;
END $$;
CREATE CONSTRAINT TRIGGER customer_drain_busy_at_commit
 AFTER INSERT ON customer_drain_busy_commit_marker
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION customer_drain_busy_commit();
`))
		mode := "immediate"
		if deferred {
			mode = "deferred"
		}
		server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER customer_drain_busy_after_sweep
 AFTER INSERT ON transfer_escrow_sweep FOR EACH ROW
 EXECUTE FUNCTION customer_drain_busy_sweep('`+mode+`')`))
		for _, id := range ids {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO customer_drain_busy_target VALUES($1)`, id))
		}
	})
	conn := acquireContractLifecycleTestConnection(t, ctx)
	held, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		t.Fatal("could not own the synthetic lock fence transaction")
	}
	released := false
	cleanup := func() {
		if released {
			return
		}
		_ = held.Rollback(context.Background())
		conn.Release()
		released = true
	}
	t.Cleanup(cleanup)
	// Completion of this statement is the barrier. NOWAIT can only refuse
	// while this explicitly owned row remains locked; elapsed time is irrelevant.
	server.RaisePgResult(held.Exec(ctx, `SELECT guard_id FROM customer_drain_busy_fence WHERE guard_id=1 FOR UPDATE`))
	return held, cleanup
}

// Observe the original PostgreSQL error through the same transaction wrapper
// used by the adapter, distinguishing callback completion from commit refusal.
func requireLegacyDrainTypedBusy(t testing.TB, ctx context.Context, id, payer server.Id, wantBodyComplete bool) {
	t.Helper()
	bodyComplete := false
	err := captureContractExpiryRepair(func() error {
		server.Tx(ctx, func(tx server.PgTx) {
			configureContractExpiryRepairTx(ctx, tx)
			_, completed, busy, _, err := drainLegacySettlementInTx(ctx, tx, id, payer)
			server.Raise(err)
			if !completed || busy {
				t.Fatal("typed error probe did not reach the unchanged financial owner")
			}
			bodyComplete = true
		}, server.TxReadCommitted, server.OptNoRetry())
		return nil
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" || legacySettlementDrainErrorStatus(err) != "busy" || bodyComplete != wantBodyComplete {
		t.Fatal("native lock refusal lost its exact SQLSTATE or transaction phase")
	}
}

// Read aggregate accounting through actual database and Redis owners. Pending
// provider projection plus applied totals must equal the durable sweep ledger.
func requireLegacyDrainBusyConservation(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, ids []server.Id, debit, reserved int64) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var credit, swept, revenue, durableBytes, durableRevenue, markerCount int64
		server.Raise(conn.QueryRow(ctx, `WITH pending AS (
 SELECT allocation FROM pending_task
 CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') allocation
 WHERE function_name=$3 AND NOT (args_json::jsonb->>'applied')::boolean
 AND (allocation->>'network_id')::uuid=$2
) SELECT
 (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1),
 (SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($4)),
 (SELECT COALESCE(sum(payout_net_revenue_nano_cents),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($4)),
 COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$2),0)+(SELECT COALESCE(sum((allocation->>'bytes')::bigint),0) FROM pending),
 COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$2),0)+(SELECT COALESCE(sum((allocation->>'revenue')::bigint),0) FROM pending),
 (SELECT count(*) FROM customer_drain_busy_commit_marker)`, f.balanceId, f.destinationNetworkId,
			task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName(), ids).Scan(&credit, &swept, &revenue, &durableBytes, &durableRevenue, &markerCount))
		if credit != 1000-debit || swept != debit || revenue != debit || durableBytes != debit || durableRevenue != debit {
			t.Fatal("typed refusal or recovery changed exact payer/provider conservation")
		}
		if debit == 0 && markerCount != 0 {
			t.Fatal("commit refusal retained its speculative constraint marker")
		}
	})
	if int64(Testing_NetEscrowByteCount(ctx, f.balanceId)) != reserved {
		t.Fatal("typed refusal or recovery changed the actual Redis reservation")
	}
}

// Two interleaved refused rows retain custody while thirty real sibling commits
// progress. Releasing the causal row permits each refused contract exactly once.
func TestLegacySettlementDrainTypedBusyPartitionAndRecovery(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
		})
		ids := make([]server.Id, 32)
		reports := make(map[server.Id][]byte, len(ids))
		for i := range ids {
			ids[i] = newLegacyDrainTestIntent(t, ctx, f, true)
			reports[ids[i]] = legacyDrainTestReports(ctx, ids[i])
		}
		refusedIds := []server.Id{ids[1], ids[30]}
		held, cleanup := holdLegacyDrainBusyFence(t, ctx, refusedIds, false)
		defer cleanup()
		request := LegacySettlementDrainRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: ids, Apply: true}
		result, err := DrainLegacySettlements(ctx, request)
		if err != nil || len(result.Contracts) != len(ids) {
			t.Fatal("native typed-busy batch lost its finite result")
		}
		acknowledged, refused := 0, 0
		for i, entry := range result.Contracts {
			if entry.ContractId != ids[i] || entry.Mirror != "not_verified" {
				t.Fatal("typed-busy batch changed input order or invented mirror verification")
			}
			if i == 1 || i == 30 {
				if entry.Status != "busy" || entry.FinancialCommitAcknowledged || entry.PostProcessing != "not_started" {
					t.Fatal("real downstream lock refusal was mislabeled or acknowledged")
				}
				requireLegacyDrainNoFinancialPrefix(t, ctx, ids[i])
				requireLegacySettlementTestState(t, ctx, f, ids[i], true, false, 910, 20)
				refused++
			} else {
				if entry.Status != "financial_committed" || !entry.FinancialCommitAcknowledged || entry.PostProcessing != "returned_unverified" {
					t.Fatal("a held synthetic refusal stopped a healthy sibling")
				}
				acknowledged++
			}
			if !bytes.Equal(reports[ids[i]], legacyDrainTestReports(ctx, ids[i])) {
				t.Fatal("typed refusal changed an original final report")
			}
		}
		if acknowledged != 30 || refused != 2 {
			t.Fatal("native batch did not establish the exact committed/refused partition")
		}
		requireLegacyDrainTypedBusy(t, ctx, refusedIds[0], f.sourceNetworkId, false)
		requireLegacyDrainBusyConservation(t, ctx, f, ids, 90, 20)
		for _, id := range refusedIds {
			requireLegacyDrainNoFinancialPrefix(t, ctx, id)
		}
		server.Raise(held.Rollback(ctx))
		request.ContractIds = refusedIds
		request.Apply = false
		preview, err := DrainLegacySettlements(ctx, request)
		if err != nil || len(preview.Contracts) != 2 {
			t.Fatal("fresh finite preview after fence release failed")
		}
		for _, entry := range preview.Contracts {
			if entry.Status != "eligible" || entry.FinancialCommitAcknowledged {
				t.Fatal("released intent did not retain ordinary custody")
			}
		}
		request.Apply = true
		recovered, err := DrainLegacySettlements(ctx, request)
		if err != nil || len(recovered.Contracts) != 2 {
			t.Fatal("released intents failed their scoped financial owner")
		}
		for _, entry := range recovered.Contracts {
			if entry.Status != "financial_committed" || !entry.FinancialCommitAcknowledged || entry.PostProcessing != "returned_unverified" || entry.Mirror != "not_verified" {
				t.Fatal("recovery did not acknowledge one financial commit per held intent")
			}
		}
		for _, id := range ids {
			if !bytes.Equal(reports[id], legacyDrainTestReports(ctx, id)) {
				t.Fatal("recovery rewrote an original final report")
			}
		}
		requireLegacyDrainBusyConservation(t, ctx, f, ids, 96, 0)
		replay, err := DrainLegacySettlements(ctx, request)
		if err != nil {
			t.Fatal("terminal replay failed")
		}
		for _, entry := range replay.Contracts {
			if entry.Status != "busy_intent_or_absent" || entry.FinancialCommitAcknowledged || entry.PostProcessing != "not_started" {
				t.Fatal("terminal replay was mistaken for generic lock refusal or reconsumed")
			}
		}
		projectLegacyProviderTotalsForTest(t, ctx)
		projectLegacyProviderTotalsForTest(t, ctx)
		requireLegacyDrainBusyConservation(t, ctx, f, ids, 96, 0)
	})
}

// A deferred native constraint proves that generic busy can arise from Commit,
// while PostgreSQL still rolls back all completed callback writes atomically.
func TestLegacySettlementDrainTypedBusyCommitRefusal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
		})
		id := newLegacyDrainTestIntent(t, ctx, f, true)
		ids := []server.Id{id}
		reports := legacyDrainTestReports(ctx, id)
		held, cleanup := holdLegacyDrainBusyFence(t, ctx, ids, true)
		defer cleanup()
		requireLegacyDrainTypedBusy(t, ctx, id, f.sourceNetworkId, true)
		requireLegacyDrainNoFinancialPrefix(t, ctx, id)
		requireLegacyDrainBusyConservation(t, ctx, f, ids, 0, 10)
		request := LegacySettlementDrainRequest{ExpectedPayerNetworkId: f.sourceNetworkId, ContractIds: ids, Apply: true}
		result, err := DrainLegacySettlements(ctx, request)
		if err != nil || len(result.Contracts) != 1 || result.Contracts[0].Status != "busy" || result.Contracts[0].FinancialCommitAcknowledged || result.Contracts[0].PostProcessing != "not_started" {
			t.Fatal("commit-time server refusal acquired a financial acknowledgement")
		}
		requireLegacyDrainNoFinancialPrefix(t, ctx, id)
		requireLegacyDrainBusyConservation(t, ctx, f, ids, 0, 10)
		if !bytes.Equal(reports, legacyDrainTestReports(ctx, id)) {
			t.Fatal("commit refusal changed original reports")
		}
		server.Raise(held.Rollback(ctx))
		result, err = DrainLegacySettlements(ctx, request)
		if err != nil || result.Contracts[0].Status != "financial_committed" || !result.Contracts[0].FinancialCommitAcknowledged {
			t.Fatal("released commit fence did not permit the unchanged financial owner")
		}
		projectLegacyProviderTotalsForTest(t, ctx)
		projectLegacyProviderTotalsForTest(t, ctx)
		requireLegacyDrainBusyConservation(t, ctx, f, ids, 3, 0)
		result, err = DrainLegacySettlements(ctx, request)
		if err != nil || result.Contracts[0].FinancialCommitAcknowledged || result.Contracts[0].Status != "busy_intent_or_absent" {
			t.Fatal("commit-refusal recovery repeated a financial transition")
		}
		if !bytes.Equal(reports, legacyDrainTestReports(ctx, id)) {
			t.Fatal("commit-refusal recovery changed original reports")
		}
		requireLegacyDrainBusyConservation(t, ctx, f, ids, 3, 0)
	})
}
