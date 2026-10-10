// Expiry reconciles disputed checkpoints atomically while retaining original reports.
package model

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Explicit deadlines retain live checkpoint flags. The historical NULL-deadline
// continuation finalizes them, but both paths preserve the original immutable
// checkpoint proof and exactly the same delivered amounts.
func requireExpirationCheckpointReports(t testing.TB, ctx context.Context, id server.Id, checkpoint bool) []byte {
	t.Helper()
	data, proof := readContractExpiryTestSnapshot(t, ctx, id)
	if proof.Expiry == nil || proof.ByteCount != 300 || len(proof.Expiry.Reports) != 2 {
		t.Fatal("disputed expiry lost its bilateral delivered-work proof")
	}
	for _, party := range []ContractParty{ContractPartySource, ContractPartyDestination} {
		report, ok := proof.Expiry.Reports[party]
		if !ok || !report.Checkpoint || report.ByteCount != 300 {
			t.Fatal("disputed expiry rewrote an original checkpoint", party, report)
		}
	}
	server.Db(ctx, func(conn server.PgConn) {
		var count int
		var unchanged bool
		server.Raise(conn.QueryRow(ctx, `SELECT count(*),bool_and(checkpoint=$2 AND used_transfer_byte_count=300)
			FROM contract_close WHERE contract_id=$1`, id, checkpoint).Scan(&count, &unchanged))
		if count != 2 || !unchanged {
			t.Fatalf("checkpoint finalization changed delivered bytes or flags: count=%d unchanged=%t", count, unchanged)
		}
	})
	return data
}

// Deadline closure commits exact debt before the independent debit worker.
// Verify its retained consumption, then apply and replay that owner explicitly.
func requireDisputedExpirationDebitAndDrain(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, id server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var exact bool
		server.Raise(conn.QueryRow(ctx, `SELECT count(*)=1
			AND bool_and(balance_id=$2 AND debit_byte_count=300 AND NOT applied)
			FROM transfer_debit_journal WHERE contract_id=$1`, id, f.balanceId).Scan(&exact))
		if !exact {
			t.Fatal("disputed deadline lost its exact pending debit")
		}
	})
	requireLegacySettlementTestState(t, ctx, f, id, false, true, 1000, 300)
	for _, want := range []int{1, 0} {
		applied, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
		if err != nil || busy || applied != want || released != want {
			t.Fatal("disputed deadline debit or replay changed consumption", applied, released, busy, err)
		}
	}
}

// Both reservation owners retire a disputed bilateral checkpoint. Fresh NULL
// rows stay protected, while their 60-minute fallback ignores report recency.
// Every path retains the same original proof and financial owner.
func TestContractExpirationDisputedCheckpointsKeepFinancialCustody(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, scenario := range []struct {
			name            string
			redis           bool
			missingDeadline bool
		}{
			{name: "redis absolute", redis: true},
			{name: "legacy absolute"},
			{name: "redis NULL hard deadline", redis: true, missingDeadline: true},
			{name: "legacy NULL hard deadline", missingDeadline: true},
		} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			var contract *TransferEscrow
			if scenario.redis {
				contract = createRedisAdmissionTest(ctx, f, 1000)
			} else {
				var posts []func() any
				contract, posts = createNetEscrowOrderingTestContract(ctx, f, 1000)
				server.RunPosts(ctx, posts...)
			}
			id := contract.ContractId
			server.Raise(CloseContract(ctx, id, f.sourceId, 300, true))
			server.Raise(CloseContract(ctx, id, f.destinationId, 300, true))
			SetContractDispute(ctx, id, true)
			cutoff := server.NowUtc().Add(-5 * time.Minute)
			if scenario.missingDeadline {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL,create_time=$2 WHERE contract_id=$1`, id, cutoff))
				})
				before := readRedisExpiryRepairTestState(ctx, id)
				count, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
				if err != nil || count != 0 || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
					t.Fatalf("%s fresh dispute lost custody: count=%d error=%v", scenario.name, count, err)
				}
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, id, server.NowUtc().Add(-61*time.Minute)))
				})
			} else {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, id, cutoff))
				})
			}
			count, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
			wantCount := int64(1)
			if scenario.missingDeadline && !scenario.redis {
				wantCount = 0
			}
			if err != nil || count != wantCount {
				t.Fatalf("%s disputed expiration count=%d want=%d err=%v", scenario.name, count, wantCount, err)
			}
			proof := requireExpirationCheckpointReports(t, ctx, id, !scenario.missingDeadline)
			if scenario.redis && !scenario.missingDeadline {
				requireDisputedExpirationDebitAndDrain(t, ctx, f, id)
			}
			if scenario.missingDeadline {
				if scenario.redis {
					flushed, err := FlushTransferDebits(ctx, int(f.balanceId[15])%TransferDebitShardCount, nil, 1)
					if err != nil || flushed.Failed != 0 {
						t.Fatal("historical Redis continuation lost its debit", err)
					}
				} else {
					requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 1000)
					complete, busy, _, err := flushLegacySettlement(ctx, id)
					if err != nil || !complete || busy {
						t.Fatal("historical legacy continuation lost its financial owner", complete, busy, err)
					}
				}
			}
			requireLegacySettlementTestState(t, ctx, f, id, false, true, 700, 0)
			if scenario.missingDeadline {
				// The old no-deadline continuation can still publish its
				// independently durable provider projection after the close.
				requireLegacyProviderDurability(t, ctx, f, id, 300)
			} else {
				requireDeadlineProviderDurability(t, ctx, f.destinationNetworkId, id, 300)
			}
			checkCompleted := func() {
				server.Db(ctx, func(conn server.PgConn) {
					var paid ByteCount
					var settled bool
					server.Raise(conn.QueryRow(ctx, `SELECT outcome=$3 AND NOT dispute,
						COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1 AND network_id=$2),0)
						FROM transfer_contract WHERE contract_id=$1`, id, f.destinationNetworkId, ContractOutcomeSettled).Scan(&settled, &paid))
					if !settled || paid != 300 {
						t.Fatalf("%s settled=%t provider bytes=%d want=300", scenario.name, settled, paid)
					}
				})
			}
			checkCompleted()
			count, _, err = ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
			if err != nil || count != 0 || !bytes.Equal(proof, requireExpirationCheckpointReports(t, ctx, id, !scenario.missingDeadline)) {
				t.Fatal("disputed expiry replay changed terminal accounting or original proof", err)
			}
			requireLegacySettlementTestState(t, ctx, f, id, false, true, 700, 0)
			checkCompleted()
		}
	})
}

// PostgreSQL rejects the dispute clear in the reconciliation transaction.
// Its proof, outcome and all money must roll back together; the original report
// rows remain available for a subsequent successful retirement.
func TestContractExpirationDisputedCheckpointCancellationRollsBackFinalization(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 1000)
		id := contract.ContractId
		server.Raise(CloseContract(ctx, id, f.sourceId, 300, true))
		server.Raise(CloseContract(ctx, id, f.destinationId, 300, true))
		SetContractDispute(ctx, id, true)
		cutoff := server.NowUtc().Add(-5 * time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, id, cutoff))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_expiration_dispute_cancel() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION USING ERRCODE='57014', MESSAGE='synthetic expiration dispute cancellation'; END;
				$$;
				CREATE TRIGGER synthetic_expiration_dispute_cancel AFTER UPDATE ON transfer_contract
				FOR EACH ROW WHEN (OLD.dispute AND NOT NEW.dispute AND NEW.outcome IS NULL)
				EXECUTE FUNCTION synthetic_expiration_dispute_cancel();`))
		})
		before := readRedisExpiryRepairTestState(ctx, id)
		_, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
		var canceled *pgconn.PgError
		if !errors.As(err, &canceled) || canceled.Code != "57014" {
			t.Fatal("dispute clear did not retain its database cancellation", err)
		}
		if !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
			t.Fatal("canceled reconciliation changed its original reports or financial state")
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, false, 1000, 1000)
		server.Db(ctx, func(conn server.PgConn) {
			var unchanged bool
			server.Raise(conn.QueryRow(ctx, `SELECT dispute AND outcome IS NULL AND provider_usage IS NULL
				AND (SELECT count(*)=2 AND bool_and(checkpoint AND used_transfer_byte_count=300)
				FROM contract_close WHERE contract_id=$1) FROM transfer_contract WHERE contract_id=$1`, id).Scan(&unchanged))
			if !unchanged {
				t.Fatal("canceled reconciliation changed proof, dispute or original reports")
			}
		})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_expiration_dispute_cancel ON transfer_contract`))
		})
		count, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
		if err != nil || count != 1 {
			t.Fatal("disputed expiry did not resume with its original proof", count, err)
		}
		proof := requireExpirationCheckpointReports(t, ctx, id, true)
		requireDisputedExpirationDebitAndDrain(t, ctx, f, id)
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 700, 0)
		requireDeadlineProviderDurability(t, ctx, f.destinationNetworkId, id, 300)
		count, _, err = ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
		if err != nil || count != 0 || !bytes.Equal(proof, requireExpirationCheckpointReports(t, ctx, id, true)) {
			t.Fatal("reconciled checkpoint replay changed original proof", count, err)
		}
	})
}
