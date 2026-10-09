// Exercise the planner's temporary inputs and exact assignment statement with
// a synthetic paid history. Plans and operation counts qualify local behavior;
// measured runtime is not evidence of production throughput.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Both arms update the same selected slice, then roll back independently. The
// control only removes temporary statistics; permanent data and indexes match.
func TestPaymentPlanSweepAssignmentTemporaryStatistics(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		const historyRows = 262144
		const selectedRows = 32768
		const networkCount = 32
		completedPaymentId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
				(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,payment_id)
				SELECT md5('synthetic-payout-contract-'||i)::uuid,
					md5('synthetic-payout-balance-'||(i%64))::uuid,
					md5('synthetic-payout-network-'||(i%$3))::uuid,
					md5('synthetic-payout-client-'||(i%$3))::uuid,7,11,
					CASE WHEN i<=$2 THEN NULL::uuid ELSE $4::uuid END
				FROM generate_series(1,$1::int) AS rows(i)`, historyRows, selectedRows, networkCount, completedPaymentId))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_escrow_sweep`))
		})
		rollback := errors.New("synthetic payout plan control rollback")
		for _, removeStatistics := range []bool{true, false} {
			value := server.HandleError(func() {
				server.MaintenanceTx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='90s'`))
					configurePaymentPlanTransaction(ctx, tx)
					planner := &PaymentPlanner{
						ctx:             ctx,
						tx:              tx,
						subsidyConfig:   payoutTransitionRevenueConfig(),
						paymentPlanId:   server.NewId(),
						networkPayments: map[server.Id]*AccountPayment{},
					}
					server.Raise(planner.planPayments())
					planner.carryPaymentBonuses()
					planner.setWallets()
					if len(planner.networkPayments) != networkCount {
						t.Fatal("selected networks changed", len(planner.networkPayments))
					}
					for networkId, payment := range planner.networkPayments {
						if payment.Payout != 11*selectedRows/networkCount || payment.PayoutByteCount != 7*selectedRows/networkCount || planner.networkSweepCounts[networkId] != selectedRows/networkCount {
							t.Fatal("selected payout amount or original-owner count changed", payment)
						}
					}
					if removeStatistics {
						// Reproduce the old session-local inputs without touching the
						// selection predicate or permanent history statistics.
						server.RaisePgResult(tx.Exec(ctx, `
						CREATE TEMP TABLE synthetic_selected_copy ON COMMIT DROP AS TABLE temp_account_payment;
						DROP TABLE temp_account_payment;
						ALTER TABLE synthetic_selected_copy RENAME TO temp_account_payment;
						CREATE TEMP TABLE synthetic_network_copy (LIKE temp_payment_network_ids INCLUDING ALL) ON COMMIT DROP;
						INSERT INTO synthetic_network_copy SELECT * FROM temp_payment_network_ids;
						DROP TABLE temp_payment_network_ids;
						ALTER TABLE synthetic_network_copy RENAME TO temp_payment_network_ids`))
					}
					var selectedStats, networkStats int
					server.Raise(tx.QueryRow(ctx, `SELECT
						(SELECT COUNT(*) FROM pg_statistic WHERE starelid='temp_account_payment'::regclass),
						(SELECT COUNT(*) FROM pg_statistic WHERE starelid='temp_payment_network_ids'::regclass)`).Scan(&selectedStats, &networkStats))
					if removeStatistics {
						if selectedStats != 0 || networkStats != 0 {
							t.Fatal("control retained temporary statistics", selectedStats, networkStats)
						}
					} else if selectedStats < 6 || networkStats != 2 {
						t.Fatal("planner joined temporary inputs without their current statistics", selectedStats, networkStats)
					}
					var raw []byte
					server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) `+paymentPlanAssignSweepsSql).Scan(&raw))
					var plan any
					server.Raise(json.Unmarshal(raw, &plan))
					var assignedRows, unchangedHistory int
					server.Raise(tx.QueryRow(ctx, `SELECT
						COUNT(*) FILTER (WHERE network_ids.payment_id=sweep.payment_id),
						COUNT(*) FILTER (WHERE sweep.payment_id=$1)
						FROM transfer_escrow_sweep sweep LEFT JOIN temp_payment_network_ids network_ids USING(network_id)`, completedPaymentId).Scan(&assignedRows, &unchangedHistory))
					if assignedRows != selectedRows || unchangedHistory != historyRows-selectedRows {
						t.Fatal("assignment changed selected ownership or paid history", assignedRows, unchangedHistory)
					}
					t.Logf("missing_temp_statistics=%t history_rows=%d selected_rows=%d networks=%d selected_statistics=%d network_statistics=%d plan=%s",
						removeStatistics, historyRows, selectedRows, networkCount, selectedStats, networkStats, raw)
					panic(rollback)
				}, server.TxReadCommitted, server.OptNoRetry())
			})
			if value != rollback {
				t.Fatal("plan control failed before its explicit rollback", value)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var unpaidRows, paidRows int
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE payment_id IS NULL), COUNT(*) FILTER (WHERE payment_id=$1) FROM transfer_escrow_sweep`, completedPaymentId).Scan(&unpaidRows, &paidRows))
			if unpaidRows != selectedRows || paidRows != historyRows-selectedRows {
				t.Fatal("comparison arms did not roll back their assignments", unpaidRows, paidRows)
			}
		})
	})
}
