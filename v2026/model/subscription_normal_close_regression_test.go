// Normal settlement must survive retransmitted checkpoints and grant expiry.
// Retries and retention controls exercise the root causes before any forced close.
package model

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A lost acknowledgement repeats one 400-byte checkpoint. The old id-less
// accumulator produced 900, whose mean exceeded the 600-byte reservation.
// Both legacy and identified retries must now settle the actual 500-byte work.
func TestNormalCloseCheckpointReplayRootCause(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, legacyEscrow := range []bool{false, true} {
			for _, identified := range []bool{false, true} {
				f := asyncPayoutRecoveryFixture(t, ctx)
				var contractId server.Id
				if legacyEscrow {
					contract, posts := createNetEscrowOrderingTestContract(ctx, f, 600)
					server.RunPosts(ctx, posts...)
					contractId = contract.ContractId
				} else {
					contractId = createRedisAdmissionTest(ctx, f, 600).ContractId
				}
				checkpoint := ContractCloseReport{ReportId: server.NewId(), ContractId: contractId,
					ClientId: f.destinationId, AckedByteCount: 400, Checkpoint: true}
				closedBefore := contractClosedCounter.ConfirmedCount()
				send := func(report ContractCloseReport) error {
					if identified {
						_, err := CloseContractWithReport(ctx, report)
						return err
					}
					return CloseContract(ctx, contractId, report.ClientId, report.AckedByteCount, report.Checkpoint)
				}
				server.Raise(send(checkpoint))
				// The first report committed, but its reply is discarded. Deliver
				// the same serialized operation again before the final report.
				server.Raise(send(checkpoint))
				server.Raise(send(ContractCloseReport{ReportId: server.NewId(), ContractId: contractId,
					ClientId: f.destinationId, AckedByteCount: 100}))
				closeErr := send(ContractCloseReport{ReportId: server.NewId(), ContractId: contractId,
					ClientId: f.sourceId, AckedByteCount: 500})
				if legacyEscrow {
					if closeErr != nil {
						t.Fatal("legacy report did not commit its financial handoff", closeErr)
					}
					var completed, busy bool
					completed, busy, _, closeErr = flushLegacySettlement(ctx, contractId)
					if busy || !completed {
						t.Fatalf("normal legacy settlement completed=%t busy=%t identified=%t", completed, busy, identified)
					}
				}
				if closeErr != nil {
					t.Fatal("checkpoint retry prevented normal settlement", identified, closeErr)
				}
				var terminal, disputed, beforeExpiration bool
				var sourceBytes, destinationBytes, earned ByteCount
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NOT NULL,dispute,
						expiration_time > clock_timestamp() AT TIME ZONE 'UTC',
						(SELECT used_transfer_byte_count FROM contract_close WHERE contract_id=$1 AND party='source'),
						(SELECT used_transfer_byte_count FROM contract_close WHERE contract_id=$1 AND party='destination'),
						COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1),0)
						FROM transfer_contract WHERE contract_id=$1`, contractId).
						Scan(&terminal, &disputed, &beforeExpiration, &sourceBytes, &destinationBytes, &earned))
				})
				wantDestination, wantEarned := ByteCount(500), ByteCount(500)
				if !terminal || disputed || !beforeExpiration || sourceBytes != 500 || destinationBytes != wantDestination || earned != wantEarned {
					t.Fatalf("normal close lost the causal accounting boundary: identified=%t terminal=%t dispute=%t before_expiration=%t source=%d destination=%d earned=%d",
						identified, terminal, disputed, beforeExpiration, sourceBytes, destinationBytes, earned)
				}
				if !legacyEscrow {
					_, _, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
					if err != nil || busy {
						t.Fatal("normal debit owner could not settle identified work", err, busy)
					}
				}
				var remaining ByteCount
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.balanceId).Scan(&remaining))
				})
				if remaining != 1000-wantEarned {
					t.Fatal("normal closure charged an unfunded or duplicated report", remaining)
				}
				if delta := contractClosedCounter.ConfirmedCount() - closedBefore; delta != 1 {
					t.Fatal("normal closure did not increment the committed close counter once", delta)
				}
			}
		}
	})
}

// The old accumulator also turned duplicate delivery into a dispute, returning
// nil without a terminal outcome. A replay must not manufacture a disagreement.
func TestNormalCloseCheckpointReplayDisputeRootCause(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		const mib ByteCount = 1024 * 1024
		for _, identified := range []bool{false, true} {
			f := asyncPayoutRecoveryFixture(t, ctx)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET
					start_balance_byte_count=$2::bigint,balance_byte_count=$2::bigint,net_revenue_nano_cents=$2::bigint*2
					WHERE balance_id=$1`, f.balanceId, 128*mib))
			})
			contractId := createRedisAdmissionTest(ctx, f, 64*mib).ContractId
			checkpointId := server.NewId()
			for _, report := range []ContractCloseReport{
				{ReportId: checkpointId, ClientId: f.destinationId, AckedByteCount: 32 * mib, Checkpoint: true},
				{ReportId: checkpointId, ClientId: f.destinationId, AckedByteCount: 32 * mib, Checkpoint: true},
				{ReportId: server.NewId(), ClientId: f.sourceId, AckedByteCount: 40 * mib},
				{ReportId: server.NewId(), ClientId: f.destinationId, AckedByteCount: 8 * mib},
			} {
				report.ContractId = contractId
				if identified {
					_, err := CloseContractWithReport(ctx, report)
					server.Raise(err)
				} else {
					server.Raise(CloseContract(ctx, contractId, report.ClientId, report.AckedByteCount, report.Checkpoint))
				}
			}
			var terminal, disputed, beforeExpiration, intent bool
			var sourceBytes, destinationBytes, earned ByteCount
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NOT NULL,dispute,
					expiration_time > clock_timestamp() AT TIME ZONE 'UTC',
					EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1),
					(SELECT used_transfer_byte_count FROM contract_close WHERE contract_id=$1 AND party='source'),
					(SELECT used_transfer_byte_count FROM contract_close WHERE contract_id=$1 AND party='destination'),
					COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1),0)
					FROM transfer_contract WHERE contract_id=$1`, contractId).
					Scan(&terminal, &disputed, &beforeExpiration, &intent, &sourceBytes, &destinationBytes, &earned))
			})
			wantDestination, wantEarned := 40*mib, 40*mib
			if !terminal || disputed || !beforeExpiration || intent || sourceBytes != 40*mib || destinationBytes != wantDestination || earned != wantEarned {
				t.Fatalf("duplicated report did not determine ordinary dispute: identified=%t terminal=%t disputed=%t source=%d destination=%d earned=%d",
					identified, terminal, disputed, sourceBytes, destinationBytes, earned)
			}
		}
	})
}

// Grant expiry stops new admission, but outstanding contracts still own its
// funds. The historical expiry-only delete removed that authority and made an
// otherwise valid bilateral normal close fail its joined-escrow calculation.
func TestNormalCloseGrantRetentionRootCause(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, unsafeRetention := range []bool{true, false} {
			f := asyncPayoutRecoveryFixture(t, ctx)
			contract, posts := createNetEscrowOrderingTestContract(ctx, f, 600)
			server.RunPosts(ctx, posts...)
			cutoff := server.NowUtc().Add(-7 * 24 * time.Hour)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=$2 WHERE balance_id=$1`, f.balanceId, cutoff.Add(-time.Hour)))
				if unsafeRetention {
					// The old retention predicate checked grant age, with no
					// unsettled-escrow exclusion. Scope its negative control to
					// this synthetic grant; no production identifier is used.
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id=$1 AND end_time <= $2`, f.balanceId, cutoff))
				}
			})
			if !unsafeRetention {
				removeCompletedTransferBalanceBatches(ctx, cutoff)
			}
			for _, clientId := range []server.Id{f.sourceId, f.destinationId} {
				_, err := CloseContractWithReport(ctx, ContractCloseReport{ReportId: server.NewId(), ContractId: contract.ContractId,
					ClientId: clientId, AckedByteCount: 500})
				server.Raise(err)
			}
			completed, busy, _, err := flushLegacySettlement(ctx, contract.ContractId)
			if busy || completed == unsafeRetention || unsafeRetention && !errors.Is(err, errContractInsufficientEscrow) || !unsafeRetention && err != nil {
				t.Fatalf("retention control did not determine normal settlement: unsafe=%t completed=%t busy=%t err=%v", unsafeRetention, completed, busy, err)
			}
			var terminal, beforeExpiration bool
			var earned ByteCount
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NOT NULL,
					expiration_time > clock_timestamp() AT TIME ZONE 'UTC',
					COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1),0)
					FROM transfer_contract WHERE contract_id=$1`, contract.ContractId).Scan(&terminal, &beforeExpiration, &earned))
			})
			if terminal == unsafeRetention || !beforeExpiration || !unsafeRetention && earned != 500 || unsafeRetention && earned != 0 {
				t.Fatal("normal close fabricated funds or depended on expiration", terminal, beforeExpiration, earned)
			}
		}
	})
}
