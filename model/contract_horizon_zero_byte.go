// The contract horizon reaper's second horizon (owner, 2026-11-21: "Reap
// zero-byte contracts at 8 days", for every payer). About half the contracts
// Main mints carry no bytes: a window or sequence opened a contract that no
// traffic used. Such a contract adds nothing that any reader sums, pays,
// awards or bills, so the reaper deletes it once it is ZeroByteHorizon old
// rather than at its horizon, with the same batches, locks, deletes and usage
// archive capture as every other contract it deletes.
//
// A contract is zero-byte when every one of these holds:
//   - it settled (outcome settled) and was never disputed, so it is closed
//     and no close is pending; it closed before the zero-byte horizon;
//   - both parties closed it, and every close carries zero bytes, so the
//     settlement billed and credited nothing (one-sided closes wait for the
//     horizon);
//   - its usage snapshot records zero bytes, so the subnet usage readers
//     credit nothing, from the live row or from the archived copy, and the
//     amended epoch reader, which needs a live row only for a contract with
//     bytes, reads it the same either way;
//   - it has no sweep: no payout, payment, payment hold, attribution review or
//     operator block award can read it;
//   - it has no extender party;
//   - every escrow of it is settled and paid out nothing, so no balance,
//     debit or reservation is still open on it.
//
// Every other contract keeps the horizon and its keep rules: one still open
// or in review, disputed, with bytes, with a sweep (the contracts of the
// payments under attribution review among them), one-sided, with an extender
// party, or with an escrow still held. Close evidence is not deleted here
// either: its own retention takes it at the horizon
// (server.RemoveExpiredCloseEvidence).
//
// The zero-byte pass walks the primary key in id order from its own cursor,
// between the horizon's window, which the horizon's pass reads, and its own
// window: ids minted CloseSettleTime before the zero-byte horizon. It never
// starts over. A contract it keeps is read again by the horizon's pass at the
// horizon, which deletes it by the horizon's rules, so a contract the zero-byte
// pass passes over is deleted no later than before. It reads the contract rows
// of a batch, then the closes of the settled ones without usage bytes, then
// the sweeps, extender parties and escrows of those still zero-byte, so a
// contract with usage bytes costs one row read. It locks only the zero-byte
// contracts, with SKIP LOCKED, checks them again after the lock in a new
// snapshot, and deletes them and their rows in the batch's one short
// transaction. Its rate, mode and horizon are its own runtime control fields.
// One run serves both passes, each paced to its own rate, the horizon's batch
// first when both are due; while both have work, each gets about half the
// run's time.
package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

// The shortest zero-byte horizon the reaper applies, whatever its settings or
// control say (owner, 2026-11-21: 8 days). Past the prober shards' eight
// generations, a weekly payout and an epoch close, and within a week of the
// newest contracts every reader of contract counts or bytes reads.
const ContractHorizonZeroByteFloor = 8 * 24 * time.Hour

// The zero-byte pass's control fields, all optional:
//   - `zeroByteMode`: off, dry_run or on, at most the reaper's mode, which it
//     follows when absent;
//   - `zeroByteMaxRowsPerSecond`: contracts the zero-byte pass reads per
//     second, at most the code's MaxRowsPerSecond; the reaper's rate when
//     absent; zero is off;
//   - `zeroByteHorizonDays`: the zero-byte horizon in whole days, between
//     ContractHorizonZeroByteFloor (8) and the reaper's horizon; a value past
//     either end counts as that end. At the reaper's horizon the zero-byte
//     pass is off.
//
// A field that does not parse narrows as far as it can: the zero-byte pass
// off.
const (
	ContractHorizonControlFieldZeroByteMode             = "zeroByteMode"
	ContractHorizonControlFieldZeroByteMaxRowsPerSecond = "zeroByteMaxRowsPerSecond"
	ContractHorizonControlFieldZeroByteHorizonDays      = "zeroByteHorizonDays"
)

// Narrows the zero-byte fields of control, after its other fields. Pure.
func narrowContractHorizonZeroByteControl(fields map[string]string, settings *ContractHorizonSettings, control *ContractHorizonControl) {
	control.ZeroByteMode = control.Mode
	control.ZeroByteMaxRowsPerSecond = control.MaxRowsPerSecond
	control.ZeroByteHorizon = contractHorizonZeroByte(settings.ZeroByteHorizon, control.Horizon)
	if value, ok := fields[ContractHorizonControlFieldZeroByteMode]; ok {
		// a value that does not parse is off
		mode, _ := ParseContractHorizonMode(value)
		control.ZeroByteMode = min(mode, control.Mode)
	}
	if value, ok := fields[ContractHorizonControlFieldZeroByteMaxRowsPerSecond]; ok {
		rate, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err == nil && !math.IsNaN(rate) {
			control.ZeroByteMaxRowsPerSecond = min(settings.MaxRowsPerSecond, rate)
		} else {
			control.ZeroByteMaxRowsPerSecond = 0
		}
	}
	if value, ok := fields[ContractHorizonControlFieldZeroByteHorizonDays]; ok {
		if days, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
			control.ZeroByteHorizon = contractHorizonZeroByteFromDays(days, control.Horizon)
		} else {
			// a value that does not parse is the horizon: nothing to read
			control.ZeroByteHorizon = control.Horizon
		}
	}
	if !(0 < control.ZeroByteMaxRowsPerSecond) || control.Horizon <= control.ZeroByteHorizon {
		control.ZeroByteMode = ContractHorizonModeOff
	}
}

// The code's zero-byte horizon under horizon: never below the floor, never
// past horizon. Pure.
func contractHorizonZeroByte(zeroByteHorizon time.Duration, horizon time.Duration) time.Duration {
	return min(max(zeroByteHorizon, ContractHorizonZeroByteFloor), horizon)
}

// days as a zero-byte horizon between ContractHorizonZeroByteFloor and
// ceiling, the reaper's horizon, clamped while still in days, so that no count
// of days overflows into a short duration. Pure.
func contractHorizonZeroByteFromDays(days int64, ceiling time.Duration) time.Duration {
	const day = 24 * time.Hour
	switch {
	case int64(ceiling/day) <= days:
		return ceiling
	case days <= int64(ContractHorizonZeroByteFloor/day):
		return contractHorizonZeroByte(ContractHorizonZeroByteFloor, ceiling)
	default:
		return time.Duration(days) * day
	}
}

// Why the zero-byte pass kept a contract it read, leaving it to the horizon.
const (
	// no outcome, open
	contractHorizonZeroByteKeptOpen = "open"
	// no outcome, under dispute: in review
	contractHorizonZeroByteKeptUnresolved = "unresolved"
	// resolved, but its close time is not past the zero-byte horizon
	contractHorizonZeroByteKeptRecentClose = "recent_close"
	// resolved other than settled, or disputed
	contractHorizonZeroByteKeptDisputed = "disputed"
	// a close, a sweep or the usage snapshot carries bytes, or it has no usage
	// snapshot
	contractHorizonZeroByteKeptBytes = "bytes"
	// a party never closed it
	contractHorizonZeroByteKeptOneSided = "one_sided"
	// it has an extender party
	contractHorizonZeroByteKeptExtender = "extender"
	// an escrow is not settled, or paid out bytes
	contractHorizonZeroByteKeptUnsettled = "unsettled"
	// another transaction holds it, or it changed after it was read
	contractHorizonZeroByteKeptBusy = "busy"
)

var contractHorizonZeroByteKeptReasons = []string{
	contractHorizonZeroByteKeptOpen,
	contractHorizonZeroByteKeptUnresolved,
	contractHorizonZeroByteKeptRecentClose,
	contractHorizonZeroByteKeptDisputed,
	contractHorizonZeroByteKeptBytes,
	contractHorizonZeroByteKeptOneSided,
	contractHorizonZeroByteKeptExtender,
	contractHorizonZeroByteKeptUnsettled,
	contractHorizonZeroByteKeptBusy,
}

var (
	contractHorizonZeroByteDeletedCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_contract_horizon_zero_byte_deleted_contracts_total",
		Help: "Zero-byte contracts the contract horizon reaper deleted at the zero-byte horizon, each with its rows; their rows also count in urnetwork_contract_horizon_deleted_rows_total",
	})
	contractHorizonZeroByteScannedCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_contract_horizon_zero_byte_scanned_contracts_total",
		Help: "Contracts the contract horizon reaper's zero-byte pass read, by mode",
	}, []string{"mode"})
	contractHorizonZeroByteKeptCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_contract_horizon_zero_byte_kept_contracts_total",
		Help: "Contracts the contract horizon reaper's zero-byte pass read and left to the horizon, by reason",
	}, []string{"reason"})
	contractHorizonZeroByteLagGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_contract_horizon_zero_byte_lag_seconds",
		Help: "How far the contract horizon reaper's zero-byte pass is behind the zero-byte horizon; a caught-up pass holds it near the close settle time; 0 while the zero-byte pass is off",
	})
	contractHorizonZeroByteHorizonSecondsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_contract_horizon_zero_byte_horizon_seconds",
		Help: "How long past its creation and its close a zero-byte contract is kept, as the contract horizon reaper's last control read set it: from 8 days to the horizon; 0 while the zero-byte pass is off",
	})
)

func init() {
	for _, mode := range []ContractHorizonMode{ContractHorizonModeDryRun, ContractHorizonModeOn} {
		contractHorizonZeroByteScannedCounter.WithLabelValues(mode.String())
	}
	for _, reason := range contractHorizonZeroByteKeptReasons {
		contractHorizonZeroByteKeptCounter.WithLabelValues(reason)
	}
	prometheus.MustRegister(
		contractHorizonZeroByteDeletedCounter,
		contractHorizonZeroByteScannedCounter,
		contractHorizonZeroByteKeptCounter,
		contractHorizonZeroByteLagGauge,
		contractHorizonZeroByteHorizonSecondsGauge,
	)
}

// Observes each committed zero-byte batch, after its commit. Nil in
// production; tests use it to stop a run at an exact batch boundary.
var contractHorizonZeroByteBatchCommitted func(batch int, last server.Id)

// The next zero-byte batch deletes the zero-byte contracts minted and closed
// more than zeroByteHorizon before the run's now, and reads those minted
// CloseSettleTime before that.
func (self *contractHorizonReaper) setZeroByteHorizon(zeroByteHorizon time.Duration) {
	self.zeroByteHorizon = zeroByteHorizon
	self.zeroByteHorizonTime = self.now.Add(-zeroByteHorizon)
	self.zeroByteWindowId = server.ContractIdFloor(self.zeroByteHorizonTime.Add(-max(0, self.settings.CloseSettleTime)))
}

// Publishes the zero-byte horizon a control read set and the zero-byte pass's
// lag behind it, both 0 while the pass is off.
func (self *contractHorizonReaper) observeZeroByteHorizon(mode ContractHorizonMode, cursor server.Id) {
	if mode == ContractHorizonModeOff {
		contractHorizonZeroByteHorizonSecondsGauge.Set(0)
		contractHorizonZeroByteLagGauge.Set(0)
		return
	}
	contractHorizonZeroByteHorizonSecondsGauge.Set(self.zeroByteHorizon.Seconds())
	self.observeZeroByteCursor(cursor)
}

// The zero-byte pass has ids to read: its window is past the horizon's.
func (self *contractHorizonReaper) zeroByteWindowOpen() bool {
	return self.windowId.Less(self.zeroByteWindowId)
}

// Where the zero-byte pass reads from: past its cursor, and at or past the
// horizon's window, which the horizon's pass reads.
func (self *contractHorizonReaper) zeroByteLower(cursor server.Id) server.Id {
	if cursor.Less(self.windowId) {
		return self.windowId
	}
	return cursor
}

// Publishes the zero-byte pass's lag behind its horizon.
func (self *contractHorizonReaper) observeZeroByteCursor(cursor server.Id) {
	cursorTime := server.ContractIdTime(self.zeroByteLower(cursor))
	contractHorizonZeroByteLagGauge.Set(max(0, self.zeroByteHorizonTime.Sub(cursorTime).Seconds()))
}

func (self *contractHorizonReaper) observeZeroByte(batch *contractHorizonZeroByteBatch, mode ContractHorizonMode) {
	contractHorizonZeroByteScannedCounter.WithLabelValues(mode.String()).Add(float64(batch.scanned))
	for reason, count := range batch.kept {
		contractHorizonZeroByteKeptCounter.WithLabelValues(reason).Add(float64(count))
	}
	if mode != ContractHorizonModeOn {
		return
	}
	contractHorizonZeroByteDeletedCounter.Add(float64(batch.deleted["transfer_contract"]))
	for table, count := range batch.deleted {
		contractHorizonDeletedRowsCounter.WithLabelValues(table).Add(float64(count))
	}
	contractHorizonArchivedCounter.Add(float64(batch.archived))
}

// One zero-byte batch's reads and deletes.
type contractHorizonZeroByteBatch struct {
	// contracts read
	scanned int64
	// the last contract id read
	lastId server.Id
	// zero-byte contracts that passed every check after the lock, deleted in
	// on mode
	eligibleIds []server.Id
	kept        map[string]int64
	deleted     map[string]int64
	archived    int64
}

// The zero-byte pass's next contract ids: after its cursor, at or past its
// lower bound, below its window, in id order. An index-only range of the
// primary key, as the horizon's window is (contractHorizonWindowSql). $1 the
// cursor, $2 the lower bound, the horizon's window id, $3 the zero-byte
// window id, $4 the batch size.
const contractHorizonZeroByteWindowSql = `
	SELECT ARRAY(
		SELECT contract_id
		FROM transfer_contract
		WHERE $1 < contract_id AND $2 <= contract_id AND contract_id < $3
		ORDER BY contract_id
		LIMIT $4
	)
`

// The zero-byte checks of the window's contracts, each table read by the
// batch's ids under the bitmap settings (contractHorizonBitmapSettings), and
// each later table only for the contracts still zero-byte: the contract rows,
// then the closes of the settled contracts without usage bytes, then the
// sweeps, extender parties and escrows of those whose closes carry no bytes.
// Each contract read is counted once, by the first reason that keeps it. In
// on mode the zero-byte contracts are locked, skipping any another
// transaction holds; the lock rereads each row's latest version. $1 the ids,
// $2 the zero-byte horizon time.
const contractHorizonZeroByteScanSqlFormat = `
	WITH scanned AS MATERIALIZED (
		SELECT contract_id, open, outcome, dispute, close_time, provider_usage
		FROM transfer_contract
		WHERE contract_id = ANY($1::uuid[])
	), closed AS MATERIALIZED (
		SELECT
			contract_id,
			outcome = 'settled' AND NOT dispute AS settled,
			coalesce(provider_usage->'byte_count' = '0'::jsonb, false) AS usage_zero
		FROM scanned
		WHERE outcome IS NOT NULL AND close_time < $2
	), close_party AS MATERIALIZED (
		SELECT
			contract_id,
			bool_or(party = 'source') AS source_closed,
			bool_or(party = 'destination') AS destination_closed,
			bool_or(used_transfer_byte_count <> 0) AS bytes
		FROM contract_close
		WHERE contract_id = ANY(ARRAY(SELECT contract_id FROM closed WHERE settled AND usage_zero))
		GROUP BY contract_id
	), quiet AS MATERIALIZED (
		SELECT contract_id
		FROM close_party
		WHERE source_closed AND destination_closed AND NOT bytes
	), swept AS MATERIALIZED (
		SELECT DISTINCT contract_id
		FROM transfer_escrow_sweep
		WHERE contract_id = ANY(ARRAY(SELECT contract_id FROM quiet))
	), extended AS MATERIALIZED (
		SELECT DISTINCT contract_id
		FROM contract_extender
		WHERE contract_id = ANY(ARRAY(SELECT contract_id FROM quiet))
	), held AS MATERIALIZED (
		SELECT DISTINCT contract_id
		FROM transfer_escrow
		WHERE
			contract_id = ANY(ARRAY(SELECT contract_id FROM quiet)) AND
			(settled IS NOT TRUE OR coalesce(payout_byte_count, 0) <> 0)
	), classified AS MATERIALIZED (
		SELECT
			closed.contract_id,
			CASE
				WHEN NOT closed.settled THEN '` + contractHorizonZeroByteKeptDisputed + `'
				WHEN NOT closed.usage_zero OR close_party.bytes IS TRUE OR swept.contract_id IS NOT NULL THEN '` + contractHorizonZeroByteKeptBytes + `'
				WHEN close_party.contract_id IS NULL OR NOT (close_party.source_closed AND close_party.destination_closed) THEN '` + contractHorizonZeroByteKeptOneSided + `'
				WHEN extended.contract_id IS NOT NULL THEN '` + contractHorizonZeroByteKeptExtender + `'
				WHEN held.contract_id IS NOT NULL THEN '` + contractHorizonZeroByteKeptUnsettled + `'
			END AS kept
		FROM closed
		LEFT JOIN close_party ON close_party.contract_id = closed.contract_id
		LEFT JOIN swept ON swept.contract_id = closed.contract_id
		LEFT JOIN extended ON extended.contract_id = closed.contract_id
		LEFT JOIN held ON held.contract_id = closed.contract_id
	), candidate AS MATERIALIZED (
		SELECT contract_id FROM classified WHERE kept IS NULL
	)%s
	SELECT
		(SELECT count(*) FROM scanned WHERE outcome IS NULL AND open),
		(SELECT count(*) FROM scanned WHERE outcome IS NULL AND open IS NOT TRUE),
		(SELECT count(*) FROM scanned WHERE outcome IS NOT NULL AND (close_time IS NULL OR $2 <= close_time)),
		(SELECT count(*) FROM classified WHERE kept = '` + contractHorizonZeroByteKeptDisputed + `'),
		(SELECT count(*) FROM classified WHERE kept = '` + contractHorizonZeroByteKeptBytes + `'),
		(SELECT count(*) FROM classified WHERE kept = '` + contractHorizonZeroByteKeptOneSided + `'),
		(SELECT count(*) FROM classified WHERE kept = '` + contractHorizonZeroByteKeptExtender + `'),
		(SELECT count(*) FROM classified WHERE kept = '` + contractHorizonZeroByteKeptUnsettled + `'),
		(SELECT count(*) FROM candidate),
		ARRAY(SELECT contract_id FROM %s ORDER BY contract_id)
`

// The row lock of the on mode, over the zero-byte contracts only, with the
// row's own checks reread under the lock.
const contractHorizonZeroByteLockSql = `, locked AS MATERIALIZED (
		SELECT transfer_contract.contract_id
		FROM transfer_contract
		WHERE transfer_contract.contract_id = ANY(ARRAY(SELECT contract_id FROM candidate))
			AND transfer_contract.outcome = 'settled'
			AND NOT transfer_contract.dispute
			AND transfer_contract.close_time < $2
			AND transfer_contract.provider_usage->'byte_count' = '0'::jsonb
		FOR UPDATE OF transfer_contract SKIP LOCKED
	)`

var contractHorizonZeroByteScanSql = fmt.Sprintf(contractHorizonZeroByteScanSqlFormat, "", "candidate")

var contractHorizonZeroByteScanLockSql = fmt.Sprintf(contractHorizonZeroByteScanSqlFormat, contractHorizonZeroByteLockSql, "locked")

var errContractHorizonZeroByteChanged = errors.New("contract horizon reaper's zero-byte pass deleted rows a zero-byte contract does not have, or a different number of contracts than it locked")

// One zero-byte batch transaction from start. In dry run mode it reads only,
// in a read only transaction, and takes no lock.
func (self *contractHorizonReaper) removeZeroByteBatch(
	ctx context.Context,
	start server.Id,
	batchSize int,
	mode ContractHorizonMode,
) (batch *contractHorizonZeroByteBatch, returnErr error) {
	dryRun := mode != ContractHorizonModeOn
	options := []any{server.TxReadCommitted, server.TxLockTimeout(self.settings.LockTimeout)}
	if dryRun {
		options = append(options, pgx.ReadOnly)
	}
	if r := server.HandleError(func() {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// a retried transaction starts over
			batch = &contractHorizonZeroByteBatch{
				kept:    map[string]int64{},
				deleted: map[string]int64{},
			}
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, self.settings.StatementTimeout.Milliseconds())))
			// as the horizon's batch (removeBatch): no JIT, a plan for the
			// actual ids
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL jit = off`))
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = force_custom_plan`))

			var windowIds []server.Id
			server.Raise(tx.QueryRow(ctx, contractHorizonZeroByteWindowSql, start, self.windowId, self.zeroByteWindowId, batchSize).Scan(&windowIds))
			batch.scanned = int64(len(windowIds))
			if batch.scanned == 0 {
				return
			}
			batch.lastId = windowIds[len(windowIds)-1]
			for _, setting := range contractHorizonBitmapSettings {
				server.RaisePgResult(tx.Exec(ctx, setting))
			}

			scanSql := contractHorizonZeroByteScanLockSql
			if dryRun {
				scanSql = contractHorizonZeroByteScanSql
			}
			var candidateCount int64
			var heldIds []server.Id
			counts := make([]int64, 8)
			server.Raise(tx.QueryRow(ctx, scanSql, windowIds, self.zeroByteHorizonTime).Scan(
				&counts[0],
				&counts[1],
				&counts[2],
				&counts[3],
				&counts[4],
				&counts[5],
				&counts[6],
				&counts[7],
				&candidateCount,
				&heldIds,
			))
			for i, reason := range []string{
				contractHorizonZeroByteKeptOpen,
				contractHorizonZeroByteKeptUnresolved,
				contractHorizonZeroByteKeptRecentClose,
				contractHorizonZeroByteKeptDisputed,
				contractHorizonZeroByteKeptBytes,
				contractHorizonZeroByteKeptOneSided,
				contractHorizonZeroByteKeptExtender,
				contractHorizonZeroByteKeptUnsettled,
			} {
				batch.kept[reason] = counts[i]
			}
			if dryRun {
				batch.eligibleIds = heldIds
				return
			}
			busyCount := candidateCount - int64(len(heldIds))
			if 0 < len(heldIds) {
				// Checked again in a snapshot taken after the lock: a writer
				// that held a contract when the scan read its rows has
				// committed by now, and closes and close reports are written
				// only under the contract lock.
				var recheckIds []server.Id
				recheck := make([]int64, 9)
				server.Raise(tx.QueryRow(ctx, contractHorizonZeroByteScanSql, heldIds, self.zeroByteHorizonTime).Scan(
					&recheck[0],
					&recheck[1],
					&recheck[2],
					&recheck[3],
					&recheck[4],
					&recheck[5],
					&recheck[6],
					&recheck[7],
					&recheck[8],
					&recheckIds,
				))
				busyCount += int64(len(heldIds) - len(recheckIds))
				batch.eligibleIds = recheckIds
			}
			batch.kept[contractHorizonZeroByteKeptBusy] = busyCount
			if len(batch.eligibleIds) == 0 {
				return
			}

			var reportCount, closeCount, escrowCount, sweepCount, extenderCount int64
			server.Raise(tx.QueryRow(ctx, contractHorizonDeleteDependentsSql, batch.eligibleIds).Scan(
				&reportCount,
				&closeCount,
				&escrowCount,
				&sweepCount,
				&extenderCount,
			))
			var contractCount int64
			server.Raise(tx.QueryRow(ctx, contractHorizonDeleteContractsSql, batch.eligibleIds).Scan(
				&contractCount,
				&batch.archived,
			))
			if contractCount != int64(len(batch.eligibleIds)) || sweepCount != 0 || extenderCount != 0 {
				// every eligible contract is locked by this transaction and has
				// neither a sweep nor an extender party
				panic(errContractHorizonZeroByteChanged)
			}
			batch.deleted["contract_close_report"] = reportCount
			batch.deleted["contract_close"] = closeCount
			batch.deleted["transfer_escrow"] = escrowCount
			batch.deleted["transfer_contract"] = contractCount
		}, options...)
	}); r != nil {
		batch = nil
		if err, ok := r.(error); ok {
			returnErr = err
		} else {
			returnErr = fmt.Errorf("%v", r)
		}
	}
	return
}
