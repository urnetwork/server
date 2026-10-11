// Staged sweep assignment for committed payment plans.
//
// The plan transaction commits its payments with their selected sweeps staged
// in payment_plan_sweep, instead of updating every selected
// transfer_escrow_sweep row inline. Bounded batches then move each staged sweep
// from its selected previous owner to its plan payment, one committed
// transaction per batch, behind a durable keyset cursor. A drain or restart
// loses at most one batch; the next payout run resumes from the cursor.
//
// Invariants:
//   - at most one plan is pending (a unique partial index), and no new
//     selection runs while one is pending, because unassigned staged sweeps
//     still look unpaid
//   - the owner guard on transfer_escrow_sweep refuses any other writer that
//     would move a staged sweep to a different payment, including an older
//     binary's planner
//   - a batch assigns a sweep only while it still has its selected previous
//     owner, and a canceled previous owner is still safely canceled
//   - a batch that cannot assign every staged row rolls back and leaves the
//     plan pending: its payments stay unpayable and nothing is released
//   - the plan's payments become payable only when the last staged row is
//     assigned (payment key reservation and AdvancePayment both refuse a
//     pending plan)
//
// Retention can delete an unpaid contract that is not held by an active
// payment. Sweeps of contracts at that risk are assigned inside the plan
// transaction, so a staged sweep cannot be deleted before its batch unless the
// assignment stays pending past paymentPlanAssignmentHorizon.
package model

import (
	"context"
	"errors"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Rows assigned per committed batch. One batch updated about 7k rows/s on Main,
// so a batch commits within seconds. Tests lower it to force several batches.
var paymentPlanAssignmentBatchSize = 50000

// Observes each committed batch, after its commit. Nil in production; tests
// use it to interrupt a run at an exact batch boundary.
var paymentPlanAssignmentBatchCommitted func(paymentPlanId server.Id, assignedSweepCount int64)

// A staged sweep is safe from retention for at least this long: contracts
// stamped for retention, or old enough to be stamped by the straggler pass
// within this horizon, are assigned in the plan transaction instead. A
// completed payment's contracts become due CompletedContractExpiration after
// completion, which bounds the horizon.
const paymentPlanAssignmentHorizon = CompletedContractExpiration

var ErrPaymentPlanAssignmentPending = errors.New("a payment plan's sweep assignment is pending; it must complete before another plan selects sweeps")

var errPaymentPlanAssignmentChanged = errors.New("a staged payment plan sweep changed owner before its assignment; the plan remains pending without payable payments")

// The plan of the payment `p` has a staged assignment that has not completed.
const paymentPlanAssignmentPendingSql = `EXISTS (SELECT 1 FROM payment_plan_assignment pending_assignment
	WHERE pending_assignment.payment_plan_id = p.payment_plan_id AND pending_assignment.complete_time IS NULL)`

// A selected sweep whose contract retention could delete before its batch.
// `selected_escrow` is a temp_account_payment row.
const paymentPlanRetentionRiskSql = `(selected_escrow.contract_create_time IS NULL OR
	selected_escrow.contract_reap_time IS NOT NULL OR selected_escrow.contract_create_time < $1)`

// Assign the retention-risk sweeps of the paid networks inside the plan
// transaction, with the same selected-owner predicate as assignSweeps.
var paymentPlanAssignRetentionRiskSweepsSql = `
	UPDATE transfer_escrow_sweep AS sweep
	SET
		payment_id = payment_network_ids.payment_id
	FROM
		temp_account_payment AS selected_escrow,
		temp_payment_network_ids AS payment_network_ids
	WHERE
		sweep.contract_id = selected_escrow.contract_id AND
		sweep.balance_id = selected_escrow.balance_id AND
		sweep.network_id = selected_escrow.network_id AND
		sweep.network_id = payment_network_ids.network_id AND
		sweep.payment_id IS NOT DISTINCT FROM selected_escrow.previous_payment_id AND
		` + paymentPlanRetentionRiskSql

var paymentPlanRetentionRiskCountSql = `
	SELECT COUNT(*)
	FROM temp_account_payment AS selected_escrow
	INNER JOIN temp_payment_network_ids AS payment_network_ids ON
		payment_network_ids.network_id = selected_escrow.network_id
	WHERE ` + paymentPlanRetentionRiskSql

var paymentPlanStageSweepsSql = `
	INSERT INTO payment_plan_sweep (
		contract_id,
		balance_id,
		network_id,
		payment_plan_id,
		previous_payment_id,
		payment_id
	)
	SELECT
		selected_escrow.contract_id,
		selected_escrow.balance_id,
		selected_escrow.network_id,
		$2,
		selected_escrow.previous_payment_id,
		payment_network_ids.payment_id
	FROM temp_account_payment AS selected_escrow
	INNER JOIN temp_payment_network_ids AS payment_network_ids ON
		payment_network_ids.network_id = selected_escrow.network_id
	WHERE NOT ` + paymentPlanRetentionRiskSql

// One batch: the next staged rows after the cursor, assigned only while each
// sweep keeps its selected previous owner and a canceled previous owner is
// still safely canceled. Returns the batch size, the assigned count and the
// batch's last key.
const paymentPlanAssignmentBatchSql = `
	WITH batch AS MATERIALIZED (
		SELECT
			contract_id,
			balance_id,
			network_id,
			previous_payment_id,
			payment_id
		FROM payment_plan_sweep
		WHERE
			payment_plan_id = $1 AND
			(contract_id, balance_id, network_id) > ($2::uuid, $3::uuid, $4::uuid)
		ORDER BY contract_id, balance_id, network_id
		LIMIT $5
	), assigned AS (
		UPDATE transfer_escrow_sweep AS sweep
		SET
			payment_id = batch.payment_id
		FROM batch
		WHERE
			sweep.contract_id = batch.contract_id AND
			sweep.balance_id = batch.balance_id AND
			sweep.network_id = batch.network_id AND
			sweep.payment_id IS NOT DISTINCT FROM batch.previous_payment_id AND
			(
				batch.previous_payment_id IS NULL OR
				EXISTS (
					SELECT 1 FROM account_payment previous
					WHERE
						previous.payment_id = batch.previous_payment_id AND
						previous.canceled AND
						NOT previous.completed AND
						previous.circle_idempotency_key IS NULL AND
						previous.payment_record IS NULL AND
						previous.tx_hash IS NULL
				)
			)
		RETURNING sweep.contract_id
	), last_key AS (
		SELECT contract_id, balance_id, network_id
		FROM batch
		ORDER BY contract_id DESC, balance_id DESC, network_id DESC
		LIMIT 1
	)
	SELECT
		(SELECT COUNT(*) FROM batch),
		(SELECT COUNT(*) FROM assigned),
		last_key.contract_id,
		last_key.balance_id,
		last_key.network_id
	FROM (SELECT 1) one
	LEFT JOIN last_key ON true
`

// The zero id orders before every staged key, so it is the initial cursor.
var paymentPlanAssignmentZeroId = server.Id{}

// stageSweeps reserves the selected sweeps of the plan's paid networks for the
// batched assignment. Withheld networks are absent from networkPayments, so
// their selected sweeps stay with their previous owners, as with assignSweeps.
// A short count rolls back the whole plan, as with assignSweeps.
func (self *PaymentPlanner) stageSweeps() {
	var expectedSweepCount int64
	for networkId := range self.networkPayments {
		expectedSweepCount += self.networkSweepCounts[networkId]
	}
	if expectedSweepCount == 0 {
		return
	}
	retentionRiskTime := server.NowUtc().Add(-(StragglerContractExpiration - paymentPlanAssignmentHorizon))

	var riskSweepCount int64
	server.Raise(self.tx.QueryRow(self.ctx, paymentPlanRetentionRiskCountSql, retentionRiskTime).Scan(&riskSweepCount))
	if 0 < riskSweepCount {
		tag := server.RaisePgResult(self.tx.Exec(self.ctx, paymentPlanAssignRetentionRiskSweepsSql, retentionRiskTime))
		if tag.RowsAffected() != riskSweepCount {
			panic(errPaymentPlanSelectionChanged)
		}
	}

	stagedSweepCount := expectedSweepCount - riskSweepCount
	if stagedSweepCount < 0 {
		panic(errPaymentPlanSelectionChanged)
	}
	if 0 < stagedSweepCount {
		server.RaisePgResult(self.tx.Exec(
			self.ctx,
			`
			INSERT INTO payment_plan_assignment (
				payment_plan_id,
				create_time,
				sweep_count
			) VALUES ($1, $2, $3)
			`,
			self.paymentPlanId,
			server.NowUtc(),
			stagedSweepCount,
		))
		tag := server.RaisePgResult(self.tx.Exec(self.ctx, paymentPlanStageSweepsSql, retentionRiskTime, self.paymentPlanId))
		if tag.RowsAffected() != stagedSweepCount {
			panic(errPaymentPlanSelectionChanged)
		}
	}
	self.stagedSweepCount = stagedSweepCount
	glog.Infof(
		"[plan]staged sweep assignment selected_rows=%d staged_rows=%d retention_risk_rows=%d\n",
		expectedSweepCount,
		stagedSweepCount,
		riskSweepCount,
	)
}

func paymentPlanAssignmentPendingInTx(ctx context.Context, tx server.PgTx) (pending bool) {
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM payment_plan_assignment WHERE complete_time IS NULL)`,
	).Scan(&pending))
	return
}

// The plan's staged sweep assignment has not completed, so its payments are
// not payable yet.
func PaymentPlanAssignmentPending(ctx context.Context, paymentPlanId server.Id) (pending bool, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		returnErr = conn.QueryRow(
			ctx,
			`SELECT EXISTS (
				SELECT 1 FROM payment_plan_assignment
				WHERE payment_plan_id = $1 AND complete_time IS NULL
			)`,
			paymentPlanId,
		).Scan(&pending)
	})
	return
}

// completePaymentPlanAssignments assigns the staged sweeps of the pending plan
// in bounded committed batches until the plan completes. progress reports
// whether any batch or completion committed. An interruption, or a staged row
// that cannot be assigned, returns an error and leaves the plan pending.
func completePaymentPlanAssignments(ctx context.Context) (progress bool, returnErr error) {
	defer func() {
		if value := recover(); value != nil {
			if err, ok := value.(error); ok {
				returnErr = err
				return
			}
			panic(value)
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return progress, err
		}
		var done bool
		var completed bool
		var assignedCount int64
		var committedPlanId server.Id
		var committedSweepCount int64
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			done = false
			completed = false
			assignedCount = 0
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, paymentPlanLockKey))

			var paymentPlanId server.Id
			var sweepCount int64
			var assignedSweepCount int64
			var cursorContractId, cursorBalanceId, cursorNetworkId *server.Id
			found := false
			result, err := tx.Query(
				ctx,
				`
				SELECT
					payment_plan_id,
					sweep_count,
					assigned_sweep_count,
					cursor_contract_id,
					cursor_balance_id,
					cursor_network_id
				FROM payment_plan_assignment
				WHERE complete_time IS NULL
				FOR UPDATE
				`,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					found = true
					server.Raise(result.Scan(
						&paymentPlanId,
						&sweepCount,
						&assignedSweepCount,
						&cursorContractId,
						&cursorBalanceId,
						&cursorNetworkId,
					))
				}
			})
			if !found {
				done = true
				return
			}
			cursor := [3]server.Id{paymentPlanAssignmentZeroId, paymentPlanAssignmentZeroId, paymentPlanAssignmentZeroId}
			if cursorContractId != nil && cursorBalanceId != nil && cursorNetworkId != nil {
				cursor = [3]server.Id{*cursorContractId, *cursorBalanceId, *cursorNetworkId}
			}

			batchCount, batchAssignedCount, lastKey := runPaymentPlanAssignmentBatchInTx(
				ctx,
				tx,
				paymentPlanAssignmentMetrics,
				paymentPlanId,
				cursor,
				paymentPlanAssignmentBatchSize,
			)
			if batchCount == 0 {
				var remainingCount int64
				server.Raise(tx.QueryRow(
					ctx,
					`SELECT COUNT(*) FROM payment_plan_sweep WHERE payment_plan_id = $1`,
					paymentPlanId,
				).Scan(&remainingCount))
				if remainingCount != 0 || assignedSweepCount != sweepCount {
					panic(errPaymentPlanAssignmentChanged)
				}
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE payment_plan_assignment SET complete_time = $2 WHERE payment_plan_id = $1`,
					paymentPlanId,
					server.NowUtc(),
				))
				glog.Infof("[plan][%s]staged sweep assignment complete rows=%d\n", paymentPlanId, sweepCount)
				completed = true
				done = true
				return
			}
			if batchAssignedCount != batchCount || lastKey == nil {
				panic(errPaymentPlanAssignmentChanged)
			}
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
				DELETE FROM payment_plan_sweep
				WHERE
					payment_plan_id = $1 AND
					(contract_id, balance_id, network_id) > ($2::uuid, $3::uuid, $4::uuid) AND
					(contract_id, balance_id, network_id) <= ($5::uuid, $6::uuid, $7::uuid)
				`,
				paymentPlanId,
				cursor[0], cursor[1], cursor[2],
				lastKey[0], lastKey[1], lastKey[2],
			))
			if tag.RowsAffected() != batchCount {
				panic(errPaymentPlanAssignmentChanged)
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE payment_plan_assignment
				SET
					assigned_sweep_count = assigned_sweep_count + $2,
					cursor_contract_id = $3,
					cursor_balance_id = $4,
					cursor_network_id = $5
				WHERE payment_plan_id = $1
				`,
				paymentPlanId,
				batchCount,
				lastKey[0], lastKey[1], lastKey[2],
			))
			assignedCount = batchCount
			committedPlanId = paymentPlanId
			committedSweepCount = assignedSweepCount + batchCount
		}, server.TxReadCommitted)
		if 0 < assignedCount && paymentPlanAssignmentBatchCommitted != nil {
			paymentPlanAssignmentBatchCommitted(committedPlanId, committedSweepCount)
		}
		// a completion also commits durable state
		progress = progress || 0 < assignedCount || completed
		if done {
			return progress, nil
		}
	}
}

// Observations of the batch statement use the plan assignment metric set.
var paymentPlanAssignmentMetrics = paymentSweepAssignmentMetrics

// runPaymentPlanAssignmentBatchInTx runs one batch statement and observes it
// with the existing assignment metrics: selected rows are the batch rows, and a
// short assignment is a changed selection.
func runPaymentPlanAssignmentBatchInTx(
	ctx context.Context,
	tx server.PgTx,
	metrics *paymentSweepAssignmentMetricSet,
	paymentPlanId server.Id,
	cursor [3]server.Id,
	batchSize int,
) (batchCount int64, assignedCount int64, lastKey *[3]server.Id) {
	start := metrics.now()
	metrics.inflight.Inc()
	outcome := "error"
	observedRows := int64(0)
	defer func() {
		seconds := max(0, metrics.now().Sub(start).Seconds())
		metrics.inflight.Dec()
		metrics.inflightRows.Sub(float64(observedRows))
		metrics.seconds.WithLabelValues(outcome).Observe(seconds)
		if glog.V(1) {
			glog.Infof("[plan][%s]staged sweep batch rows=%d outcome=%s seconds=%.3f\n", paymentPlanId, observedRows, outcome, seconds)
		}
	}()
	observedRows = int64(batchSize)
	metrics.inflightRows.Add(float64(observedRows))

	var lastContractId, lastBalanceId, lastNetworkId *server.Id
	server.Raise(tx.QueryRow(
		ctx,
		paymentPlanAssignmentBatchSql,
		paymentPlanId,
		cursor[0], cursor[1], cursor[2],
		batchSize,
	).Scan(&batchCount, &assignedCount, &lastContractId, &lastBalanceId, &lastNetworkId))
	metrics.selectedRows.Observe(float64(batchCount))
	if lastContractId != nil && lastBalanceId != nil && lastNetworkId != nil {
		lastKey = &[3]server.Id{*lastContractId, *lastBalanceId, *lastNetworkId}
	}
	if assignedCount != batchCount {
		outcome = "selection_changed"
		glog.Errorf("[plan][%s]staged sweep batch assigned %d of %d rows; the plan remains pending\n", paymentPlanId, assignedCount, batchCount)
		return
	}
	outcome = "matched"
	return
}
