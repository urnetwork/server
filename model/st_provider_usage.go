// Epoch usage consumes immutable completed-work snapshots. Payment balances,
// escrow sweeps, and current provider membership never determine pool weight.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

// Reads live and archived work in one PostgreSQL snapshot and refuses partial
// pre-activation history. A reused live/archive identity fails the whole window.
// The half-open settlement window counts each contract once, regardless of
// how many balances funded it or whether it needed escrow at all.
func GetStEpochProviderUsage(ctx context.Context, startTime time.Time, endTime time.Time) ([]*StProviderUsage, error) {
	return getStEpochProviderUsage(ctx, 0, startTime, endTime)
}

// Only the payout owner's explicit epoch can admit an exact historical debt
// receipt. Generic time-window readers retain strict complete-usage admission.
func GetStEpochProviderUsageAtEpoch(ctx context.Context, epoch uint64, startTime time.Time, endTime time.Time) ([]*StProviderUsage, error) {
	return getStEpochProviderUsage(ctx, epoch, startTime, endTime)
}

// A historical terminal row with no close time cannot be assigned outside the
// requested epoch. Probe at most one such identity per store in the same
// statement snapshot as live/archive usage, including while retention moves it.
// The live partial index excludes ordinary open/canceled rows; archive custody
// contains only credit-bearing outcomes and uses its existing close-time index.
const stEpochProviderUsageSql = `
	WITH missing_time AS MATERIALIZED (
		(SELECT contract_id, NULL::jsonb AS provider_usage, close_time, false AS duplicate
		 FROM transfer_contract WHERE close_time IS NULL
			AND outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
		 ORDER BY contract_id LIMIT 1)
		UNION ALL
		(SELECT contract_id, NULL::jsonb AS provider_usage, close_time, false AS duplicate
		 FROM st_provider_usage_archive WHERE close_time IS NULL
		 ORDER BY close_time, contract_id LIMIT 1)
	)
	SELECT contract_id, provider_usage, close_time, duplicate FROM missing_time
	UNION ALL
	SELECT source.contract_id, source.provider_usage, source.close_time,
		archive.contract_id IS NOT NULL AS duplicate
	FROM transfer_contract AS source
	LEFT JOIN st_provider_usage_archive AS archive ON archive.contract_id=source.contract_id
	WHERE NOT EXISTS (SELECT 1 FROM missing_time)
		AND $1 <= source.close_time AND source.close_time < $2
		AND source.outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
	UNION ALL
	SELECT contract_id, provider_usage, close_time, false FROM st_provider_usage_archive
	WHERE NOT EXISTS (SELECT 1 FROM missing_time)
		AND $1 <= close_time AND close_time < $2
		AND outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
`

// Counts the usage statement's rows, up to $3, in the read's own snapshot. A
// window past the census record bound holds no undated terminal row: one
// would gate the statement down to the one or two probe rows.
const stEpochProviderUsageBoundSql = `SELECT count(*) FROM (SELECT 1 FROM (` + stEpochProviderUsageSql + `) AS usage LIMIT $3) AS bounded`

// A whole window reads the same usage statement in one block-ordered pass. Its
// rows are scattered through the contract heap (Main, 2026-10-30: a 7-day
// epoch closed ~400M contracts on ~35% of ~105M heap pages, ~1.15 rows per
// page in any two minutes), so the close-time index order the planner prefers
// pays a synchronous random page read per row: 10-40K rows/s on cold pages,
// past the close's three hours. A bitmap reads each page once, in block order,
// with asynchronous prefetch: 125-165K rows/s on the same cold pages. The
// bound proves both undated probes empty, so their plans read nothing; the
// archive is hashed in memory, and nothing sorts, materializes or spills. One
// backend keeps the row order deterministic; a forced custom plan keeps a
// session's cached generic plan from ignoring these settings.
var stEpochProviderUsageWholeWindowSettings = []string{
	`SET LOCAL enable_indexscan = off`,
	`SET LOCAL enable_seqscan = off`,
	`SET LOCAL max_parallel_workers_per_gather = 0`,
	`SET LOCAL plan_cache_mode = force_custom_plan`,
}

// Plans the rest of tx's statements as whole-window reads.
func stEpochProviderUsageWholeWindowPlanInTx(ctx context.Context, tx server.PgTx) error {
	for _, setting := range stEpochProviderUsageWholeWindowSettings {
		if _, err := tx.Exec(ctx, setting); err != nil {
			return err
		}
	}
	return nil
}

// One usage statement row.
type stEpochUsageRow struct {
	contractId server.Id
	data       []byte
	closedAt   *time.Time
	duplicate  bool
}

// A whole window's rows are checked in batches on workers: decoding a usage
// snapshot costs more than reading its row, so one reader feeds several.
// Rows are still added one at a time, in statement order.
const stEpochUsageBatchRows = 1024

var stEpochUsageCheckWorkers = 4

// A whole-window batch, checked on a worker and then added in order.
type stEpochUsageBatch struct {
	rows      []stEpochUsageRow
	snapshots []*contractUsageSnapshot
	errs      []error
	panicked  any
	// closed once every row is checked
	checked chan struct{}
}

func newStEpochUsageBatch() *stEpochUsageBatch {
	return &stEpochUsageBatch{
		rows:    make([]stEpochUsageRow, 0, stEpochUsageBatchRows),
		checked: make(chan struct{}),
	}
}

// Checks every row. A panic is kept for the reader to raise.
func (self *stEpochUsageBatch) Check(check func(*stEpochUsageRow) (*contractUsageSnapshot, error)) {
	defer close(self.checked)
	defer func() {
		self.panicked = recover()
	}()
	self.snapshots = make([]*contractUsageSnapshot, len(self.rows))
	self.errs = make([]error, len(self.rows))
	for i := range self.rows {
		self.snapshots[i], self.errs[i] = check(&self.rows[i])
	}
}

// The first refusal of the batches added so far, or a panic to raise.
type stEpochUsageAdded struct {
	err      error
	panicked any
}

// Adds the checked rows in order, up to the first refusal.
func (self *stEpochUsageBatch) Add(add func(*stEpochUsageRow, *contractUsageSnapshot, []byte) error) (added stEpochUsageAdded) {
	if self.panicked != nil {
		return stEpochUsageAdded{panicked: self.panicked}
	}
	defer func() {
		if r := recover(); r != nil {
			added = stEpochUsageAdded{panicked: r}
		}
	}()
	for i := range self.rows {
		if self.errs[i] != nil {
			return stEpochUsageAdded{err: self.errs[i]}
		}
		if err := add(&self.rows[i], self.snapshots[i], nil); err != nil {
			return stEpochUsageAdded{err: err}
		}
	}
	return stEpochUsageAdded{}
}

// Reads rows with next on the caller's goroutine, checks them in batches on
// workers and adds them one at a time in read order. The first refusal in
// read order is the result, and cancel then stops next's source; otherwise
// next's own error is. A panic in a check or an add is raised here.
func addStEpochUsageRowsInOrder(
	workers int,
	cancel func(),
	next func(*stEpochUsageRow) (bool, error),
	check func(*stEpochUsageRow) (*contractUsageSnapshot, error),
	add func(*stEpochUsageRow, *contractUsageSnapshot, []byte) error,
) error {
	unchecked := make(chan *stEpochUsageBatch, workers)
	inOrder := make(chan *stEpochUsageBatch, 2*workers)
	closeBatches := sync.OnceFunc(func() {
		close(unchecked)
		close(inOrder)
	})
	defer closeBatches()
	var checking sync.WaitGroup
	for range workers {
		checking.Add(1)
		go func() {
			defer checking.Done()
			for batch := range unchecked {
				batch.Check(check)
			}
		}()
	}
	added := make(chan stEpochUsageAdded, 1)
	go func() {
		result := stEpochUsageAdded{}
		for batch := range inOrder {
			<-batch.checked
			if result.err != nil || result.panicked != nil {
				continue
			}
			result = batch.Add(add)
			if result.err != nil || result.panicked != nil {
				// the remaining rows cannot change a refused read
				cancel()
			}
		}
		added <- result
	}()
	batch := newStEpochUsageBatch()
	var readErr error
	for {
		var row stEpochUsageRow
		more, err := next(&row)
		if err != nil {
			readErr = err
			// ends the source rather than reading the rest of its rows
			cancel()
			break
		}
		if !more {
			break
		}
		batch.rows = append(batch.rows, row)
		if len(batch.rows) == stEpochUsageBatchRows {
			unchecked <- batch
			inOrder <- batch
			batch = newStEpochUsageBatch()
		}
	}
	if 0 < len(batch.rows) {
		unchecked <- batch
		inOrder <- batch
	}
	closeBatches()
	checking.Wait()
	result := <-added
	if result.panicked != nil {
		panic(result.panicked)
	}
	if result.err != nil {
		// rows are read in order, so a refusal precedes the read's own error
		return result.err
	}
	return readErr
}

// A component census comes from the exact same statement snapshot as usage.
// Existing payout processing can retain unknown evidence when the optional
// original-row component exceeds capacity; it never publishes a partial census.
func GetStEpochProviderUsageCensus(ctx context.Context, epoch uint64, startTime, endTime time.Time) ([]*StProviderUsage, *payoutartifact.ClosedWorkCensus, error) {
	usages, census, _, err := GetStEpochProviderUsageWholeCensus(ctx, epoch, startTime, endTime)
	return usages, census, err
}

// A separate sentinel retains the complete same-statement window even when no
// credited row exists. No invented earning row carries known-empty evidence.
func GetStEpochProviderUsageWholeCensus(ctx context.Context, epoch uint64, startTime, endTime time.Time) ([]*StProviderUsage, *payoutartifact.ClosedWorkCensus, *payoutartifact.ClosedWorkWindow, error) {
	census := &payoutartifact.ClosedWorkCensus{Schema: payoutartifact.ClosedWorkSchema}
	var window *payoutartifact.ClosedWorkWindow
	readStart := time.Now()
	usages, err := getStEpochProviderUsageWithCensus(ctx, epoch, startTime, endTime, census, &window)
	if err != nil {
		return nil, nil, nil, err
	}
	// the statement must finish inside the epoch close's task max time
	glog.Infof("[st]epoch %d census read %d closed contracts for %d providers in %s\n", epoch, census.Count, len(usages), time.Since(readStart).Round(time.Millisecond))
	if census.Records == nil {
		return usages, nil, nil, nil
	}
	census.Sort()
	return usages, census, window, nil
}

func getStEpochProviderUsage(ctx context.Context, epoch uint64, startTime time.Time, endTime time.Time) ([]*StProviderUsage, error) {
	return getStEpochProviderUsageWithCensus(ctx, epoch, startTime, endTime, nil, nil)
}

// The optional recorder borrows each validated row only until it clones the
// original jsonb bytes. Every statement reads one snapshot, so no retention or
// terminal writer can race the read. A window past the census record bound
// reads the usage statement alone, planned as one block-ordered pass; any
// other window reads exactly as before.
func getStEpochProviderUsageWithCensus(ctx context.Context, epoch uint64, startTime time.Time, endTime time.Time, census *payoutartifact.ClosedWorkCensus, window **payoutartifact.ClosedWorkWindow) ([]*StProviderUsage, error) {
	if !startTime.Before(endTime) {
		return nil, fmt.Errorf("invalid subnet usage window")
	}
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, err
	}
	earningStart, earningEnd := transition.SnWindow(startTime, endTime)
	if !earningStart.Before(earningEnd) {
		return []*StProviderUsage{}, nil
	}
	// Whole-work evidence retains the original epoch interval, including every
	// pre-cutoff original. Only the earning aggregate uses the asset boundary.
	queryStart, queryEnd := earningStart, earningEnd
	originalBytes := 0
	if census != nil {
		queryStart, queryEnd = startTime, endTime
		census.WindowStart, census.WindowEnd = startTime.UTC().Format(time.RFC3339Nano), endTime.UTC().Format(time.RFC3339Nano)
		if transition != nil {
			census.EarningPolicyHash = "sha256:" + transition.ConfigSha256
			selection, err := providerPayoutEarningSelection(transition)
			if err != nil {
				return nil, err
			}
			census.EarningStart = selection.StartTime.UTC().Format(time.RFC3339Nano)
			census.EarningSelectionHash = selection.PolicyHash
		}
		census.Records = []payoutartifact.ClosedWorkRecord{}
	}
	usagesByClientId := map[server.Id]*StProviderUsage{}
	usageRows := 0
	var returnErr error
	// The refusals one row raises alone, in the read's order. Safe to run
	// concurrently: it reads only the row.
	check := func(row *stEpochUsageRow) (*contractUsageSnapshot, error) {
		if row.closedAt == nil {
			return nil, fmt.Errorf("subnet contract %s has terminal usage without a close time; epoch completeness is unknown", row.contractId)
		}
		if row.duplicate {
			return nil, fmt.Errorf("subnet contract %s has both live and archived usage", row.contractId)
		}
		snapshot, err := decodeContractUsageSnapshot(row.data)
		if err != nil {
			return nil, fmt.Errorf("subnet contract %s: %w", row.contractId, err)
		}
		if legacy := snapshot.LegacyExclusion; legacy != nil && (legacy.ContractId != row.contractId || !legacy.ClosedAt.Equal(*row.closedAt) || legacy.Epoch != epoch) {
			return nil, fmt.Errorf("subnet contract %s: legacy usage exclusion differs from its terminal owner", row.contractId)
		}
		return snapshot, nil
	}
	// Records a checked row in the census and the earning totals. Rows are
	// added one at a time, in statement order.
	add := func(row *stEpochUsageRow, snapshot *contractUsageSnapshot, originalReports []byte) error {
		if census != nil {
			census.Count++
			if census.Records != nil {
				if len(originalReports) > payoutartifact.MaxClosedWorkRecordBytes {
					originalReports = nil // Preserve original usage, never a truncated proof.
				}
				if len(census.Records) == payoutartifact.MaxClosedWorkRecords || len(row.data) > payoutartifact.MaxClosedWorkRecordBytes || len(row.data)+len(originalReports) > payoutartifact.MaxClosedWorkOriginalBytes-originalBytes {
					census.Records = nil // No prefix may claim the complete query.
				} else {
					originalBytes += len(row.data) + len(originalReports)
					census.Records = append(census.Records, payoutartifact.ClosedWorkRecord{ContractId: [16]byte(row.contractId), ClosedAt: row.closedAt.UTC().Format(time.RFC3339Nano), Original: bytes.Clone(row.data), OriginalReports: bytes.Clone(originalReports)})
				}
			}
		}
		if row.closedAt.Before(earningStart) || !row.closedAt.Before(earningEnd) {
			return nil
		}
		for _, provider := range snapshot.Providers {
			usage := usagesByClientId[provider.ClientId]
			if usage == nil {
				usage = &StProviderUsage{ClientId: provider.ClientId, NetworkId: provider.NetworkId}
				usagesByClientId[provider.ClientId] = usage
			}
			if usage.NetworkId != provider.NetworkId || int64(provider.ByteCount) > math.MaxInt64-usage.PayoutByteCount {
				return fmt.Errorf("subnet provider usage has ambiguous network or overflowing total")
			}
			usage.PayoutByteCount += int64(provider.ByteCount)
		}
		return nil
	}
	// originals: the census statement, with its report and window columns
	read := func(conn server.PgCanQuery, query string, originals bool) {
		rows, err := conn.Query(ctx, query, queryStart, queryEnd)
		if err != nil {
			returnErr = fmt.Errorf("read epoch provider usage: %w", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var row stEpochUsageRow
			var originalReports []byte
			var windowOnly bool
			columns := []any{&row.contractId, &row.data, &row.closedAt, &row.duplicate}
			if originals {
				columns = append(columns, &originalReports, &windowOnly)
			}
			if err := rows.Scan(columns...); err != nil {
				returnErr = err
				return
			}
			if windowOnly {
				if window != nil && len(originalReports) != 0 {
					if err := json.Unmarshal(originalReports, window); err != nil {
						returnErr = err
						return
					}
				}
				continue
			}
			usageRows++
			snapshot, err := check(&row)
			if err != nil {
				returnErr = err
				return
			}
			if err := add(&row, snapshot, originalReports); err != nil {
				returnErr = err
				return
			}
		}
		returnErr = rows.Err()
	}
	// A whole window checks its rows on workers, the costly decode, and adds
	// them in statement order, so every refusal and total is the one the
	// sequential read of the same rows returns.
	readWhole := func(tx server.PgTx) {
		queryCtx, cancelQuery := context.WithCancel(ctx)
		defer cancelQuery()
		rows, err := tx.Query(queryCtx, stEpochProviderUsageSql, queryStart, queryEnd)
		if err != nil {
			returnErr = fmt.Errorf("read epoch provider usage: %w", err)
			return
		}
		defer rows.Close()
		next := func(row *stEpochUsageRow) (bool, error) {
			if !rows.Next() {
				return false, rows.Err()
			}
			if err := rows.Scan(&row.contractId, &row.data, &row.closedAt, &row.duplicate); err != nil {
				return false, err
			}
			usageRows++
			return true, nil
		}
		returnErr = addStEpochUsageRowsInOrder(stEpochUsageCheckWorkers, cancelQuery, next, check, add)
	}
	// Every statement of a read shares one REPEATABLE READ snapshot, so the
	// read never pages across commits. A seven-day epoch outlasts the
	// application role's 15-minute transaction bound, so it runs on a direct
	// maintenance session that lifts the bound for itself; the caller's MaxTime
	// still bounds it, and its cancel reaches that session. A replica takes
	// this read once one exists.
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.WithoutTransactionTimeout(ctx, conn, func() {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				returnErr = fmt.Errorf("read epoch provider usage: %w", err)
				return
			}
			// read only, so a rollback ends it
			defer tx.Rollback(context.WithoutCancel(ctx))
			if census != nil {
				// The census and the check for close evidence that a partition
				// cutover holds outside the evidence table read one snapshot, so
				// the census never misses a moved report: it waits for the swap.
				moved, err := server.CloseEvidenceMovedOutsideTableInTx(ctx, tx)
				if err != nil {
					returnErr = fmt.Errorf("read epoch provider usage: %w", err)
					return
				}
				if moved {
					returnErr = server.ErrCloseEvidenceCutoverMoving
					return
				}
			}
			var bounded int
			if err := tx.QueryRow(ctx, stEpochProviderUsageBoundSql, queryStart, queryEnd, payoutartifact.MaxClosedWorkRecords+1).Scan(&bounded); err != nil {
				returnErr = fmt.Errorf("read epoch provider usage: %w", err)
				return
			}
			if bounded <= payoutartifact.MaxClosedWorkRecords {
				if census != nil {
					read(tx, stEpochProviderOriginalUsageSql, true)
				} else {
					read(tx, stEpochProviderUsageSql, false)
				}
				return
			}
			// Past MaxClosedWorkRecords rows the census can hold no record (the
			// row that overflows it discards every record before it), so the
			// census statement's report and window columns would all be
			// discarded. The usage statement alone returns the same rows.
			if err := stEpochProviderUsageWholeWindowPlanInTx(ctx, tx); err != nil {
				returnErr = fmt.Errorf("read epoch provider usage: %w", err)
				return
			}
			glog.Infof("[st]usage window [%s, %s) holds over %d closed contracts; reading it whole, without close reports\n", queryStart.UTC().Format(time.RFC3339), queryEnd.UTC().Format(time.RFC3339), payoutartifact.MaxClosedWorkRecords)
			if census != nil {
				census.Records = nil
			}
			readWhole(tx)
			if returnErr == nil && usageRows <= payoutartifact.MaxClosedWorkRecords {
				// one snapshot cannot return fewer rows than it counted
				returnErr = fmt.Errorf("read epoch provider usage: %d rows read after %d were counted", usageRows, bounded)
			}
		})
	})
	if returnErr != nil {
		return nil, returnErr
	}
	usages := make([]*StProviderUsage, 0, len(usagesByClientId))
	for _, usage := range usagesByClientId {
		usages = append(usages, usage)
	}
	slices.SortFunc(usages, func(a, b *StProviderUsage) int { return a.ClientId.Cmp(b.ClientId) })
	return usages, nil
}
