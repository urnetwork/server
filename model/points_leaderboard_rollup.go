// Paid-sweep weeks advance atomically with a resumable contract cursor.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

const (
	pointsBlockRollupBatchContracts = 1000
	pointsBlockRollupMaxBatches     = 12
	pointsBlockRollupRunBudget      = 45 * time.Second
)

// AdvancePointsBlockRollup materializes earning weeks from paid contracts in
// bounded, resumable transactions. It is deliberately outside payout planning:
// a large historical plan must not extend the payment write transaction.
// Each paid sweep's settlement time is the operator's established payout
// reporting clock. A contract with sweeps on both sides of Sunday earns in
// both blocks; its final close time alone is not sufficient evidence.
func AdvancePointsBlockRollup(ctx context.Context) (advancedBatches int, returnErr error) {
	deadline := server.NowUtc().Add(pointsBlockRollupRunBudget)
	for advancedBatches < pointsBlockRollupMaxBatches && server.NowUtc().Before(deadline) {
		var batchCount int64 = -1
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			result, err := tx.Query(ctx, pointsBlockRollupSql, pointsBlockRollupBatchContracts, SubnetBlockGenesis, int64(SubnetBlockDuration/time.Second))
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&batchCount))
				}
			})
		}, server.TxReadCommitted)
		if batchCount < 0 {
			return advancedBatches, nil
		}
		advancedBatches++
	}
	return advancedBatches, nil
}

// The production statement is shared with the native plan and accounting regression.
const pointsBlockRollupSql = `
				WITH payment AS MATERIALIZED (
					SELECT payment_id, block_rollup_cursor
					FROM account_payment
					WHERE NOT block_rollup_complete
						AND EXISTS (
							SELECT 1 FROM account_point AS point
							WHERE point.account_payment_id = account_payment.payment_id
								AND point.point_value > 0
								AND point.create_time >= $2::timestamp
						)
						-- a plan still assigning its staged sweeps would end the
						-- rollup early, before its remaining sweeps are its own
						AND NOT EXISTS (
							SELECT 1 FROM payment_plan_assignment AS pending_assignment
							WHERE pending_assignment.payment_plan_id = account_payment.payment_plan_id
								AND pending_assignment.complete_time IS NULL
						)
					ORDER BY create_time, payment_id
					LIMIT 1
					FOR UPDATE SKIP LOCKED
				), batch AS MATERIALIZED (
					SELECT DISTINCT sweep.contract_id
					FROM transfer_escrow_sweep AS sweep
					-- Scalar bounds reach the payment/contract index before joins.
					-- The inclusive floor retains zero UUIDs for a null cursor;
					-- the strict check excludes an actual cursor's contract.
					WHERE sweep.payment_id = (SELECT payment_id FROM payment)
						AND sweep.contract_id >= COALESCE(
							(SELECT block_rollup_cursor FROM payment),
							'00000000-0000-0000-0000-000000000000'::uuid
						)
						AND ((SELECT block_rollup_cursor FROM payment) IS NULL
							OR sweep.contract_id > (SELECT block_rollup_cursor FROM payment))
					ORDER BY sweep.contract_id
					-- Keep custom and generic plans streaming the bounded prefix.
					LIMIT (SELECT $1::bigint)
				), paid AS MATERIALIZED (
					SELECT sweep.sweep_time
					FROM batch
					JOIN transfer_escrow_sweep AS sweep ON
						sweep.contract_id = batch.contract_id
					WHERE sweep.payment_id = (SELECT payment_id FROM payment)
						-- Bound sweep reads as well as batch membership. Empty
						-- batches give null bounds and cannot read paid history.
						AND sweep.contract_id >= (SELECT contract_id FROM batch ORDER BY contract_id LIMIT 1)
						AND sweep.contract_id <= (SELECT contract_id FROM batch ORDER BY contract_id DESC LIMIT 1)
						AND sweep.payout_byte_count > 0
				), inserted AS (
					INSERT INTO account_payment_block (payment_id, block_number)
					SELECT DISTINCT payment.payment_id,
						1 + floor(extract(epoch FROM (paid.sweep_time - $2::timestamp)) / $3::numeric)::bigint
					FROM payment, paid
					WHERE paid.sweep_time >= $2::timestamp
					ON CONFLICT DO NOTHING
					RETURNING block_number
				), advanced AS (
					UPDATE account_payment AS account
					SET block_rollup_cursor = COALESCE(
						(SELECT contract_id FROM batch ORDER BY contract_id DESC LIMIT 1),
						account.block_rollup_cursor
					),
					block_rollup_complete = (SELECT COUNT(*) FROM batch) < $1
					FROM payment
					WHERE account.payment_id = payment.payment_id
					RETURNING (SELECT COUNT(*) FROM batch) AS batch_count
				)
				SELECT batch_count FROM advanced
			`
