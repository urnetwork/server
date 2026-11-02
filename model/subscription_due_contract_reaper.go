// The delete pass of RemoveCompletedContracts: hard deletes each contract
// whose reap_time has passed, with its dependent rows, one bounded batch per
// transaction.
//
// A batch names its rows by the batch's contract ids in dependent tables of
// billions of rows, whose statistics can badly misjudge how many rows one
// contract id matches. On Main (2026-11-02) transfer_escrow.contract_id had
// n_distinct 182,900 over 4.96 billion rows: about 27,000 escrows per contract,
// where the real count is about 1.6. With the 50,000 ids this pass then deleted
// at once, the planner costed `transfer_escrow.contract_id = ANY(ids)` at 1.36
// billion rows and chose a sequential scan of the 481 GB table, from about
// 40,000 ids up. That was ten to twenty minutes on CPU in one transaction,
// holding the vacuum horizon, and often past the task's MaxTime, which rolled
// the batch back and restarted the run within seconds. So a batch is planned
// and bounded as the horizon reaper's are:
//   - it reads at most dueContractReapBatchSize due contracts;
//   - the statements after the reap_time window read their rows with bitmap
//     scans only (contractHorizonBitmapSettings), which no misestimate can turn
//     into a sequential scan. The window keeps its ordered index scan;
//   - each statement is bounded by dueContractReapStatementTimeout, and each
//     lock wait by dueContractReapLockTimeout. A batch that reaches either bound
//     rolls back, and the pass stops until the task's next run. No statement
//     is JIT compiled, which the misestimates would otherwise buy for each;
//   - the pass is paced to dueContractReapMaxRowsPerSecond and ends at its
//     budget, which RemoveCompletedContracts keeps inside the task's MaxTime.
package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Due contracts one delete-pass transaction reads, at most: the horizon
// reaper's batch size (DefaultContractHorizonSettings), a few seconds of work.
// A var so tests can force several batches.
var dueContractReapBatchSize = 2000

// Due contracts the delete pass reads per second, at most: the horizon reaper's
// default rate. Over the pass's five-minute budget that is about 60,000
// contracts a run, about what the former single 50,000-contract batch deleted
// in a run that finished. A var so tests can pace slowly.
var dueContractReapMaxRowsPerSecond = 200.0

// The bounds of each batch, the horizon reaper's (DefaultContractHorizonSettings).
const (
	dueContractReapStatementTimeout = 30 * time.Second
	dueContractReapLockTimeout      = 2 * time.Second
)

// Why a delete pass stopped.
const (
	// no contract is due
	dueContractReapStopDrained = "drained"
	// the pass spent its budget
	dueContractReapStopBudget = "budget"
	// another writer owns a balance of the batch's escrows
	dueContractReapStopBusy = "busy"
	// a lock wait reached dueContractReapLockTimeout; the batch rolled back
	dueContractReapStopLockTimeout = "lock_timeout"
	// a statement was canceled, by dueContractReapStatementTimeout or an
	// operator; the batch rolled back
	dueContractReapStopStatementTimeout = "statement_timeout"
)

var (
	dueContractReapStopCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_due_contract_reap_stops_total",
		Help: "Due-contract delete passes of RemoveCompletedContracts, by why they stopped",
	}, []string{"stop"})
	dueContractReapDeletedCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_due_contract_reap_deleted_contracts_total",
		Help: "Contracts the due-contract delete pass of RemoveCompletedContracts deleted",
	})
)

func init() {
	for _, stop := range []string{
		dueContractReapStopDrained,
		dueContractReapStopBudget,
		dueContractReapStopBusy,
		dueContractReapStopLockTimeout,
		dueContractReapStopStatementTimeout,
	} {
		dueContractReapStopCounter.WithLabelValues(stop)
	}
	prometheus.MustRegister(dueContractReapStopCounter, dueContractReapDeletedCounter)
}

// Observes each batch inside its transaction, before its deletes, with the
// contracts it is about to delete. Nil in production; tests read the
// transaction's plans and bounds through it.
var dueContractReapBatchObserved func(ctx context.Context, tx server.PgTx, candidateIds []server.Id)

// Deletes the dependents of the batch's deletable contracts, by their ids.
const dueContractDeleteDependentsSql = `
	WITH deleted_report AS (
		DELETE FROM contract_close_report
		WHERE contract_close_report.contract_id = ANY($1::uuid[])
	), deleted_close AS (
		DELETE FROM contract_close
		WHERE contract_close.contract_id = ANY($1::uuid[])
	), deleted_escrow AS (
		DELETE FROM transfer_escrow
		WHERE transfer_escrow.contract_id = ANY($1::uuid[])
	), deleted_sweep AS (
		DELETE FROM transfer_escrow_sweep
		WHERE transfer_escrow_sweep.contract_id = ANY($1::uuid[])
	)
	DELETE FROM contract_extender
	WHERE contract_extender.contract_id = ANY($1::uuid[])
`

// removeDueContractBatches consumes the first bounded slice of the reap_time
// index before doing any payment lookup. Due contracts held by an active or
// ambiguous payment are repaired to reap_time = NULL. So are unpaid contracts
// stamped by the former 90-day rule that have not yet reached the new 300-day
// horizon. All other due contracts and their dependent rows are deleted.
// Classifying only the already-bounded slice avoids turning the safety checks
// into an anti-join over contract history. The payment guard also covers
// completed payments whose queued retention cursor has not finished yet.
//
// Every write names its contracts with contract_id = ANY(ids), which lets each
// statement prune to the partitions holding them. The classification locks the
// due contracts before their dependent rows are deleted. A close report or close
// is written only under its contract's lock, so the deletes see every one
// committed before that lock, and none can follow it. Close reports are deleted
// here because the partition cutover drops their cascade from transfer_contract;
// until then the cascade finds them already gone.
//
// Each batch reads at most batchSize contracts; the pass ends at the reaper's
// run budget.
func removeDueContractBatches(ctx context.Context, minTime time.Time, minStragglerCreateTime time.Time, batchSize int) {
	newDueContractReaper(minTime, minStragglerCreateTime, batchSize, server.NowUtc().Add(reaperRunBudget)).removeDue(ctx)
}

// One delete pass: its due and straggler bounds, batch size, rate and budget.
type dueContractReaper struct {
	minTime                time.Time
	minStragglerCreateTime time.Time
	batchSize              int
	maxRowsPerSecond       float64
	// the pass starts no batch at or after this
	budgetEnd time.Time
}

// A pass ending at the reaper's run budget or at runEnd, whichever is first.
func newDueContractReaper(minTime time.Time, minStragglerCreateTime time.Time, batchSize int, runEnd time.Time) *dueContractReaper {
	budgetEnd := server.NowUtc().Add(reaperRunBudget)
	if runEnd.Before(budgetEnd) {
		budgetEnd = runEnd
	}
	return &dueContractReaper{
		minTime:                minTime.UTC(),
		minStragglerCreateTime: minStragglerCreateTime.UTC(),
		batchSize:              batchSize,
		maxRowsPerSecond:       dueContractReapMaxRowsPerSecond,
		budgetEnd:              budgetEnd,
	}
}

// Runs batches until no contract is due, a batch stops the pass, or the budget
// ends. The budget is checked before each batch, so a pass never starts one
// after it. A raise other than a bound of this pass propagates, as before.
func (self *dueContractReaper) removeDue(ctx context.Context) (stop string) {
	for {
		if !server.NowUtc().Before(self.budgetEnd) {
			stop = dueContractReapStopBudget
			break
		}
		batchStart := time.Now()
		var processedCount int64
		processedCount, stop = self.removeBatch(ctx)
		if stop != "" {
			break
		}
		self.pace(ctx, contractHorizonPaceDelay(processedCount, self.maxRowsPerSecond, time.Since(batchStart)))
	}
	dueContractReapStopCounter.WithLabelValues(stop).Inc()
	return
}

// Waits delay, or until the budget ends or ctx is done. A done ctx stops the
// next batch at its begin, as it did before pacing.
func (self *dueContractReaper) pace(ctx context.Context, delay time.Duration) {
	delay = min(delay, self.budgetEnd.Sub(server.NowUtc()))
	if delay <= 0 {
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// One batch transaction. processedCount is the due contracts it read and
// locked. stop is why the pass ends after it, empty to go on.
func (self *dueContractReaper) removeBatch(ctx context.Context) (processedCount int64, stop string) {
	defer func() {
		if r := recover(); r != nil {
			bound := dueContractReapBoundStop(ctx, r)
			if bound == "" {
				panic(r)
			}
			if bound == dueContractReapStopStatementTimeout {
				glog.Infof("[due_contract_reap]batch canceled, stopping until the next run: %s\n", server.ErrorJsonNoStack(r))
			} else if glog.V(1) {
				glog.Infof("[due_contract_reap]lock wait bound ended, stopping until the next run: %s\n", server.ErrorJsonNoStack(r))
			}
			processedCount = 0
			stop = bound
		}
	}()
	var deletedCount int64
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		processedCount = 0
		deletedCount = 0
		stop = ""
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, dueContractReapStatementTimeout.Milliseconds())))
		// The batch's statements are short, but the misestimated escrow and
		// close counts cost them past jit_above_cost, so each would compile.
		// On Main that compile is most of such statements' time: 70% of the
		// horizon reaper's dependents delete and 89% of its hold check
		// (pg_stat_statements, 2026-11-02).
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL jit = off`))
		// A cached generic plan cannot prune partitions by an array
		// parameter, so plan every statement with its actual ids.
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = force_custom_plan`))
		// Fix the bounded contract slice before shared balance ownership.
		// The mutation below can only revisit these admitted identities;
		// newly due contracts remain for the next pass. This read keeps the
		// default plan: the ordered reap_time index scan needs index scans.
		var contractIds []server.Id
		rows, err := tx.Query(ctx, `SELECT contract_id FROM transfer_contract
 WHERE reap_time IS NOT NULL AND reap_time<$1 ORDER BY reap_time LIMIT $2`, self.minTime, self.batchSize)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				server.Raise(rows.Scan(&id))
				contractIds = append(contractIds, id)
			}
		})
		if len(contractIds) == 0 {
			stop = dueContractReapStopDrained
			return
		}
		// Every later statement names its rows by these ids.
		for _, setting := range contractHorizonBitmapSettings {
			server.RaisePgResult(tx.Exec(ctx, setting))
		}
		// A zero balance id on a due contract's escrow would fail this
		// head-of-index slice on every pass and stop all reaping behind it.
		balanceIds, err := contractTransferBalanceIdsInTx(ctx, tx, contractIds)
		server.Raise(err)
		admitted, err := tryTransferBalanceOwnershipInTx(ctx, tx, ownableTransferBalanceIds(balanceIds))
		server.Raise(err)
		if !admitted {
			stop = dueContractReapStopBusy
			return
		}
		var candidateIds []server.Id
		server.Raise(tx.QueryRow(
			ctx,
			`
				WITH due AS MATERIALIZED (
					SELECT
						transfer_contract.contract_id,
						transfer_contract.create_time
					FROM transfer_contract
					WHERE
						transfer_contract.reap_time IS NOT NULL AND
						transfer_contract.reap_time < $1 AND
						transfer_contract.contract_id=ANY($4::uuid[])
					ORDER BY transfer_contract.reap_time
					LIMIT $3
					FOR UPDATE
				), protected AS MATERIALIZED (
					SELECT due.contract_id
					FROM due
					WHERE
						EXISTS (
							SELECT 1
							FROM transfer_escrow_sweep
							INNER JOIN account_payment ON
								account_payment.payment_id = transfer_escrow_sweep.payment_id
							WHERE
								transfer_escrow_sweep.contract_id = due.contract_id AND
								`+server.ContractPaymentRetentionHoldSql+`
						) OR
						(
							due.create_time >= $2 AND
							NOT EXISTS (
								SELECT 1
								FROM transfer_escrow_sweep
								INNER JOIN account_payment ON
									account_payment.payment_id = transfer_escrow_sweep.payment_id
								WHERE
									transfer_escrow_sweep.contract_id = due.contract_id AND
									account_payment.completed
							)
						)
				), cleared AS (
					UPDATE transfer_contract
					SET reap_time = NULL
					FROM protected
					WHERE
						transfer_contract.contract_id=ANY($4::uuid[]) AND
						transfer_contract.contract_id = protected.contract_id
					RETURNING transfer_contract.contract_id
				)
				SELECT
					(SELECT COUNT(*) FROM due),
					ARRAY(
						SELECT due.contract_id
						FROM due
						WHERE NOT EXISTS (
							SELECT 1
							FROM protected
							WHERE protected.contract_id = due.contract_id
						)
						ORDER BY due.contract_id
					)
				`,
			self.minTime,
			self.minStragglerCreateTime,
			self.batchSize,
			contractIds,
		).Scan(&processedCount, &candidateIds))
		if processedCount == 0 {
			// the window's contracts left the due set since it was read
			stop = dueContractReapStopDrained
			return
		}
		if len(candidateIds) == 0 {
			return
		}
		if dueContractReapBatchObserved != nil {
			dueContractReapBatchObserved(ctx, tx, candidateIds)
		}
		server.RaisePgResult(tx.Exec(ctx, dueContractDeleteDependentsSql, candidateIds))
		result, err := tx.Query(
			ctx,
			`
				DELETE FROM transfer_contract
				WHERE transfer_contract.contract_id = ANY($1::uuid[])
				RETURNING transfer_contract.contract_id, source_id, destination_id,
					outcome IS NULL AS unresolved
				`,
			candidateIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var contractId, sourceId, destinationId server.Id
				var unresolved bool
				server.Raise(result.Scan(&contractId, &sourceId, &destinationId, &unresolved))
				deletedCount += 1
				// Deleting unresolved custody is its terminal lifecycle event.
				// Read the deleted version so a concurrent outcome wins once.
				if unresolved {
					server.AddTxCommitCount(tx, &contractClosedCounter, 1)
				}
				contractHoleEventInTx(ctx, tx, contractId, sourceId, destinationId, "remove")
			}
		})
	}, server.TxReadCommitted, server.OptNoRetry(), server.TxLockTimeout(dueContractReapLockTimeout))
	// committed
	dueContractReapDeletedCounter.Add(float64(deletedCount))
	return
}

// The pass stop for a batch's raise r at one of the pass's bounds, or "" when r
// is not one: a lock wait past dueContractReapLockTimeout (55P03), or a
// statement canceled (57014) while ctx is live, by its
// dueContractReapStatementTimeout or by an operator. Everything else, an end
// of ctx included, propagates as before.
func dueContractReapBoundStop(ctx context.Context, r any) string {
	err, ok := r.(error)
	if !ok || ctx.Err() != nil {
		return ""
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return ""
	}
	switch pgErr.Code {
	case pgerrcode.LockNotAvailable:
		return dueContractReapStopLockTimeout
	case pgerrcode.QueryCanceled:
		return dueContractReapStopStatementTimeout
	default:
		return ""
	}
}
