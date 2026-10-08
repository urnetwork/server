// Expiry finalizes disputed checkpoints through their existing financial owner.
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

// Live report flags may become final, but original checkpoint proof and the
// delivered amounts remain exactly the same throughout settlement and replay.
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

// Both reservation owners retire a disputed bilateral checkpoint. NULL
// deadlines are immediately eligible even with fresh reports; every path
// retains the same original proof and financial owner.
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
			{name: "redis NULL immediate", redis: true, missingDeadline: true},
			{name: "legacy NULL immediate", missingDeadline: true},
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
			} else {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, id, cutoff))
				})
			}
			count, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
			wantCount := int64(0)
			if scenario.redis {
				wantCount = 1
			}
			if err != nil || count != wantCount {
				t.Fatalf("%s disputed expiration count=%d want=%d err=%v", scenario.name, count, wantCount, err)
			}
			proof := requireExpirationCheckpointReports(t, ctx, id, false)
			if scenario.redis {
				flushed, err := FlushTransferDebits(ctx, int(f.balanceId[15])%TransferDebitShardCount, nil, 1)
				if err != nil || flushed.Failed != 0 {
					t.Fatal("disputed Redis debit failed", err)
				}
			} else {
				requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 1000)
				server.Db(ctx, func(conn server.PgConn) {
					var dispute bool
					server.Raise(conn.QueryRow(ctx, `SELECT dispute FROM transfer_contract WHERE contract_id=$1`, id).Scan(&dispute))
					if !dispute {
						t.Fatal("queued legacy intent cleared dispute before financial completion")
					}
				})
				complete, busy, _, err := flushLegacySettlement(ctx, id)
				if err != nil || !complete || busy {
					t.Fatalf("legacy disputed continuation: complete=%t busy=%t err=%v", complete, busy, err)
				}
			}
			requireLegacySettlementTestState(t, ctx, f, id, false, true, 700, 0)
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
			if err != nil || count != 0 || !bytes.Equal(proof, requireExpirationCheckpointReports(t, ctx, id, false)) {
				t.Fatal("disputed expiry replay changed terminal accounting or original proof", err)
			}
			requireLegacySettlementTestState(t, ctx, f, id, false, true, 700, 0)
			checkCompleted()
		}
	})
}

// PostgreSQL rejects the dispute clear after the checkpoint update has run.
// Their shared transaction must roll flags, outcome and all money back, while
// retaining the separately committed original proof for a successful retry.
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
		_, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
		var canceled *pgconn.PgError
		if !errors.As(err, &canceled) || canceled.Code != "57014" {
			t.Fatal("dispute clear did not retain its database cancellation", err)
		}
		proof := requireExpirationCheckpointReports(t, ctx, id, true)
		requireLegacySettlementTestState(t, ctx, f, id, false, false, 1000, 1000)
		server.Db(ctx, func(conn server.PgConn) {
			var dispute bool
			server.Raise(conn.QueryRow(ctx, `SELECT dispute FROM transfer_contract WHERE contract_id=$1`, id).Scan(&dispute))
			if !dispute {
				t.Fatal("canceled finalization cleared dispute")
			}
		})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_expiration_dispute_cancel ON transfer_contract`))
		})
		count, _, err := ForceCloseOpenContractIdsPage(ctx, cutoff, 32, 1, 0, 0, nil)
		if err != nil || count != 1 || !bytes.Equal(proof, requireExpirationCheckpointReports(t, ctx, id, false)) {
			t.Fatal("disputed expiry did not resume with its original proof", count, err)
		}
		flushed, err := FlushTransferDebits(ctx, int(f.balanceId[15])%TransferDebitShardCount, nil, 1)
		if err != nil || flushed.Failed != 0 {
			t.Fatal("resumed disputed debit failed", err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 700, 0)
	})
}
