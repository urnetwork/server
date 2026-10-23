// Paid-sweep weeks advance atomically with a resumable contract cursor.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// The one definition of the operator blocks a payment earns in. The payout
// plan (paymentPlanBlocksSql) and the history rollup (pointsBlockRollupSql)
// both build from it, so the two cannot disagree. Every sweep the payment pays
// that carries payout bytes is paid traffic, and it places the payment in the
// operator block that contains its sweep time; a sweep before the genesis is
// in no block. `sweep` is the sweep row's alias; `genesis` and `blockSeconds`
// are the statement's parameters for SubnetBlockGenesis and
// SubnetBlockDuration in seconds.

// The sweep is paid traffic.
func pointsPaidSweepSql(sweep string) string {
	return sweep + ".payout_byte_count > 0"
}

// The sweep is in an operator block.
func pointsSweepInBlocksSql(sweep string, genesis string) string {
	return sweep + ".sweep_time >= " + genesis + "::timestamp"
}

// The 1-based operator block of a sweep in pointsSweepInBlocksSql.
func pointsSweepBlockSql(sweep string, genesis string, blockSeconds string) string {
	return "1 + floor(extract(epoch FROM (" + sweep + ".sweep_time - " + genesis + "::timestamp)) / " + blockSeconds + "::numeric)::bigint"
}

// The bounds of one history rollup run. A run is part of a leaderboard
// rebuild, which runs again a minute later while the rollup is incomplete
// (controller.RebuildPointsLeaderboardPost), so a backfill takes many short
// runs instead of holding payout transactions or a taskworker for hours.
type PointsBlockRollupSettings struct {
	// The most distinct contracts of one payment a batch reads, in one
	// transaction. A payment with fewer contracts left completes in that
	// batch. It bounds the batch's output, not its sweep reads: a contract can
	// have many sweeps. Must be positive.
	BatchContracts int
	// The most batches a run commits. Non-positive commits none.
	MaxBatches int
	// No batch starts after a run has used this long; it is not a statement
	// deadline, so the batch in flight runs to its end. Non-positive starts
	// none.
	RunBudget time.Duration
}

// The bounds the history backfill has run with since the rollup shipped
// (2026-09-23).
func DefaultPointsBlockRollupSettings() *PointsBlockRollupSettings {
	return &PointsBlockRollupSettings{
		BatchContracts: 1000,
		MaxBatches:     12,
		RunBudget:      45 * time.Second,
	}
}

// AdvancePointsBlockRollup materializes earning weeks from paid contracts in
// bounded, resumable transactions, for the payments planned before plans
// stored their own blocks (finalizePayments). A plan's payments are complete
// when it commits, so this visits only that history. It is deliberately
// outside payout planning: a large historical plan must not extend the
// payment write transaction. Each paid sweep's settlement time is the
// operator's established payout reporting clock. A contract with sweeps on
// both sides of Sunday earns in both blocks; its final close time alone is
// not sufficient evidence.
func AdvancePointsBlockRollup(ctx context.Context, settings *PointsBlockRollupSettings) (advancedBatches int, returnErr error) {
	deadline := server.NowUtc().Add(settings.RunBudget)
	for advancedBatches < settings.MaxBatches && server.NowUtc().Before(deadline) {
		var batchCount int64 = -1
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			result, err := tx.Query(ctx, pointsBlockRollupSql, settings.BatchContracts, SubnetBlockGenesis, int64(SubnetBlockDuration/time.Second))
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
var pointsBlockRollupSql = `
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
						AND ` + pointsPaidSweepSql("sweep") + `
				), inserted AS (
					INSERT INTO account_payment_block (payment_id, block_number)
					SELECT DISTINCT payment.payment_id,
						` + pointsSweepBlockSql("paid", "$2", "$3") + `
					FROM payment, paid
					WHERE ` + pointsSweepInBlocksSql("paid", "$2") + `
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
