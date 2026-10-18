// Committed network reliability scores for a payout window.
//
// The reliability payout reads network_connection_reliability_score for the
// exact subsidy window of its plan. Computing those scores aggregates every
// valid client_reliability row in the window (about 708M rows for a 4-day
// slice on Main, a single-threaded window scan that ran up to 89 minutes) and
// used to run inside the plan transaction, so a drain rolled back the whole
// slice. Here the scores are folded from committed block chunks before the plan
// transaction begins:
//   - payment_plan_reliability_window fixes the window's rollup shift, covered
//     and degraded blocks, and a block cursor
//   - each chunk reads its block range in read-only statements of at most
//     paymentPlanReliabilityReadBlockCount blocks, adds the per-client counts
//     and numeric sums exactly in memory, and adds them to the partials with
//     the cursor in one short transaction; drained blocks are immutable, so
//     reads and chunks commute
//   - the fold joins the client locations once and writes the scores exactly as
//     the single statement did, then marks the window complete
//
// A chunk aggregated in one statement held its snapshot and xid for the whole
// scan: 396 s on average and up to 750 s for six-hour chunks on Main. Reads now
// hold a snapshot for one bounded statement and never take an xid, and a chunk
// still writes one partial row per client, so the partial rows written per
// window are those of six-hour chunks. A drain loses at most the uncommitted
// reads of one chunk.
//
// The plan transaction refuses a window that is not complete, or whose scores
// were replaced by another window's, by rolling back with
// paymentPlanReliabilityPendingError; its caller commits that window and plans
// again. Every writing step takes the payment plan lock, the single writer of
// these scores (see network_client_reliability_model.go). Reads take no lock: a
// chunk commits only while the window's cursor is still where its reads began.
package model

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Blocks per committed chunk: six hours of 60-second blocks, about 28M
// client_reliability rows on Main. A chunk writes one partial row per client
// it saw, about 137k on Main, so this size sets the partial rows written per
// window (about 2.2M for 4 days) and the progress a drain can lose.
var paymentPlanReliabilityChunkBlockCount int64 = 360

// Blocks per read-only aggregate statement: a quarter hour of blocks, about
// 1.2M client_reliability rows on Main, read in about 5-17 s at the measured
// 0.33-1.1 s per block (about 32 s at the worst 2.1 s).
var paymentPlanReliabilityReadBlockCount int64 = 15

// Partial rows per upsert statement when a chunk commits.
const paymentPlanReliabilityFlushBatchSize = 10000

// Observes each committed chunk, after its commit, with the window's next
// shifted block. Nil in production; tests use it to interrupt a window at an
// exact chunk boundary.
var paymentPlanReliabilityChunkCommitted func(nextBlockNumber int64)

// A window whose scores another window replaced is recomputed at most this
// many times in one call.
const paymentPlanReliabilityResetLimit = 1

type paymentPlanReliabilityPendingError struct {
	minTime time.Time
	maxTime time.Time
}

func (self *paymentPlanReliabilityPendingError) Error() string {
	return fmt.Sprintf(
		"payment plan reliability scores are not committed for window [%s, %s]; commit them and plan again",
		self.minTime.Format(time.RFC3339Nano),
		self.maxTime.Format(time.RFC3339Nano),
	)
}

var errPaymentPlanReliabilityReplaced = errors.New("payment plan reliability scores were replaced by another window during preparation")

// The unshifted block window of a subsidy time window, as
// UpdateNetworkReliabilityScoresInTx computes it.
func paymentPlanReliabilityBlockWindow(minTime time.Time, maxTime time.Time) (minBlockNumber int64, maxBlockNumber int64) {
	return reliabilityBlockNumber(minTime), reliabilityBlockNumber(maxTime) + 1
}

// predictPaymentPlanReliabilityWindow returns the reliability window a bounded
// slice selects when its unpaid backlog reaches past the last subsidy epoch:
// the window starts at that epoch's end, which clamps the selection's start,
// and ends at the last settled close before the slice's close-time bound. The
// scores are committed before the plan transaction, so the plan reads them
// without selecting twice. A slice bounded by now, whose end still moves, and an
// unbounded plan are not predicted; like a slice whose selection ends
// elsewhere, its plan reports its exact window and plans again once that window
// is committed.
func predictPaymentPlanReliabilityWindow(
	ctx context.Context,
	subsidyConfig *SubsidyConfig,
	transition *server.ProviderPayoutTransition,
	maxDuration time.Duration,
) (minTime time.Time, maxTime time.Time, ok bool) {
	if maxDuration <= 0 {
		return
	}
	var lastSubsidyEnd *time.Time
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT MAX(end_time) FROM subsidy_payment`).Scan(&lastSubsidyEnd))
	})
	if lastSubsidyEnd == nil {
		return
	}
	now := server.NowUtc()
	upperBound := lastSubsidyEnd.Add(maxDuration)
	if transition != nil {
		upperBound = server.MinTime(upperBound, transition.Cutoff)
	}
	if !upperBound.Before(now) {
		return
	}
	// the closed-usage index finds the last settled close with a short
	// backward scan
	var lastClose *time.Time
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT MAX(close_time) FROM transfer_contract
			WHERE outcome IS NOT NULL AND
				outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination') AND
				$1 <= close_time AND close_time < $2
			`,
			*lastSubsidyEnd,
			upperBound,
		).Scan(&lastClose))
	})
	if lastClose == nil {
		return
	}
	finalLegacyTail := transition != nil && !now.Before(transition.Cutoff)
	if !lastSubsidyEnd.Before(*lastClose) || (lastClose.Sub(*lastSubsidyEnd) < subsidyConfig.MinDurationPerPayout() && !finalLegacyTail) {
		// the slice cannot form a subsidy epoch, so it needs no scores
		return
	}
	return *lastSubsidyEnd, *lastClose, true
}

// The window row and its derived shifted block range.
type paymentPlanReliabilityWindow struct {
	minBlockNumber       int64
	maxBlockNumber       int64
	blockShift           int64
	effectiveBlockCount  int64
	degradedBlockNumbers []int64
	nextBlockNumber      int64
	completeTime         *time.Time
}

func (self *paymentPlanReliabilityWindow) shiftedMinBlockNumber() int64 {
	return self.minBlockNumber - self.blockShift
}

func (self *paymentPlanReliabilityWindow) shiftedMaxBlockNumber() int64 {
	return self.maxBlockNumber - self.blockShift
}

func getPaymentPlanReliabilityWindowInTx(
	ctx context.Context,
	tx server.PgTx,
	minBlockNumber int64,
	maxBlockNumber int64,
	forUpdate bool,
) (window *paymentPlanReliabilityWindow) {
	lock := ""
	if forUpdate {
		lock = "FOR UPDATE"
	}
	result, err := tx.Query(
		ctx,
		`
		SELECT
			block_shift,
			effective_block_count,
			degraded_block_numbers,
			next_block_number,
			complete_time
		FROM payment_plan_reliability_window
		WHERE min_block_number = $1 AND max_block_number = $2
		`+lock,
		minBlockNumber,
		maxBlockNumber,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			window = &paymentPlanReliabilityWindow{
				minBlockNumber: minBlockNumber,
				maxBlockNumber: maxBlockNumber,
			}
			server.Raise(result.Scan(
				&window.blockShift,
				&window.effectiveBlockCount,
				&window.degradedBlockNumbers,
				&window.nextBlockNumber,
				&window.completeTime,
			))
		}
	})
	return
}

// The stored scores were folded for exactly this shifted window.
func paymentPlanReliabilityScoresCurrentInTx(ctx context.Context, tx server.PgTx, window *paymentPlanReliabilityWindow) (current bool) {
	server.Raise(tx.QueryRow(
		ctx,
		`
		SELECT NOT EXISTS (
			SELECT 1 FROM network_connection_reliability_score
			WHERE min_block_number <> $1 OR max_block_number <> $2
		)
		`,
		window.shiftedMinBlockNumber(),
		window.shiftedMaxBlockNumber(),
	).Scan(&current))
	return
}

// paymentPlanReliabilityWindowInTx reports whether the scores of the window
// are committed and current, and the rollup shift they were computed with. A
// window without usable blocks is complete with the previous scores retained,
// as UpdateNetworkReliabilityScoresInTx keeps them.
func paymentPlanReliabilityWindowInTx(
	ctx context.Context,
	tx server.PgTx,
	minTime time.Time,
	maxTime time.Time,
) (blockShift int64, ok bool) {
	minBlockNumber, maxBlockNumber := paymentPlanReliabilityBlockWindow(minTime, maxTime)
	window := getPaymentPlanReliabilityWindowInTx(ctx, tx, minBlockNumber, maxBlockNumber, false)
	if window == nil || window.completeTime == nil {
		return 0, false
	}
	if 0 < window.effectiveBlockCount && !paymentPlanReliabilityScoresCurrentInTx(ctx, tx, window) {
		return 0, false
	}
	return window.blockShift, true
}

// The per-client partials of a shifted block range: the rows and per-row terms
// of the single statement, with the count as bigint and the sum as numeric.
const paymentPlanReliabilityReadSql = `
	SELECT
		valid_counts.network_id,
		valid_counts.client_id,
		COUNT(*),
		SUM(1.0/valid_counts.valid_client_count)
	FROM (
		SELECT
			network_id,
			client_id,
			COUNT(*) OVER (PARTITION BY block_number, client_address_hash) AS valid_client_count
		FROM client_reliability
		WHERE
			valid = true AND
			$1 <= block_number AND
			block_number < $2 AND
			NOT (block_number = ANY($3::bigint[]))
	) valid_counts
	GROUP BY
		valid_counts.network_id,
		valid_counts.client_id
`

// Adds a batch of a chunk's exact partials to the window's partials.
const paymentPlanReliabilityFlushSql = `
	INSERT INTO payment_plan_reliability_partial (
		min_block_number,
		max_block_number,
		network_id,
		client_id,
		valid_block_count,
		reliability_sum
	)
	SELECT
		$1,
		$2,
		partial.network_id,
		partial.client_id,
		partial.valid_block_count,
		partial.reliability_sum
	FROM unnest($3::uuid[], $4::uuid[], $5::bigint[], $6::numeric[])
		AS partial(network_id, client_id, valid_block_count, reliability_sum)
	ON CONFLICT (min_block_number, max_block_number, network_id, client_id) DO UPDATE
	SET
		valid_block_count = payment_plan_reliability_partial.valid_block_count + EXCLUDED.valid_block_count,
		reliability_sum = payment_plan_reliability_partial.reliability_sum + EXCLUDED.reliability_sum
`

// A partial's client under the network its rows carry, as the partials key it.
type paymentPlanReliabilityPartialKey struct {
	networkId server.Id
	clientId  server.Id
}

// A client's exact count and sum over the blocks read so far.
type paymentPlanReliabilityPartial struct {
	validBlockCount int64
	reliabilitySum  reliabilityExactSum
}

// Reads the partials of shifted blocks [minBlockNumber, maxBlockNumber) in
// read-only statements of at most paymentPlanReliabilityReadBlockCount blocks,
// each its own transaction, and adds them exactly. A read joins the totals
// only after its transaction ends, so a retried read is counted once.
func readPaymentPlanReliabilityPartials(
	ctx context.Context,
	minBlockNumber int64,
	maxBlockNumber int64,
	degradedBlockNumbers []int64,
) map[paymentPlanReliabilityPartialKey]*paymentPlanReliabilityPartial {
	partials := map[paymentPlanReliabilityPartialKey]*paymentPlanReliabilityPartial{}
	for readMinBlockNumber := minBlockNumber; readMinBlockNumber < maxBlockNumber; {
		readMaxBlockNumber := min(readMinBlockNumber+paymentPlanReliabilityReadBlockCount, maxBlockNumber)
		type read struct {
			key             paymentPlanReliabilityPartialKey
			validBlockCount int64
			reliabilitySum  pgtype.Numeric
		}
		var reads []read
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			reads = nil
			result, err := tx.Query(
				ctx,
				paymentPlanReliabilityReadSql,
				readMinBlockNumber,
				readMaxBlockNumber,
				degradedBlockNumbers,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var r read
					server.Raise(result.Scan(&r.key.networkId, &r.key.clientId, &r.validBlockCount, &r.reliabilitySum))
					reads = append(reads, r)
				}
			})
		}, server.TxReadCommitted)
		for _, r := range reads {
			partial, ok := partials[r.key]
			if !ok {
				partial = &paymentPlanReliabilityPartial{}
				partials[r.key] = partial
			}
			partial.validBlockCount += r.validBlockCount
			partial.reliabilitySum.add(r.reliabilitySum)
		}
		readMinBlockNumber = readMaxBlockNumber
	}
	return partials
}

// Commits the partials of shifted blocks [cursor, nextBlockNumber) and moves
// the window's cursor to nextBlockNumber, when the window is still pending at
// cursor. Otherwise another preparer committed or replaced it, and the reads
// are discarded.
func commitPaymentPlanReliabilityChunk(
	ctx context.Context,
	minBlockNumber int64,
	maxBlockNumber int64,
	cursor int64,
	nextBlockNumber int64,
	partials map[paymentPlanReliabilityPartialKey]*paymentPlanReliabilityPartial,
) (committed bool) {
	keys := slices.Collect(maps.Keys(partials))
	slices.SortFunc(keys, func(a paymentPlanReliabilityPartialKey, b paymentPlanReliabilityPartialKey) int {
		if c := a.networkId.Cmp(b.networkId); c != 0 {
			return c
		}
		return a.clientId.Cmp(b.clientId)
	})
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		committed = false
		server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, paymentPlanLockKey))
		window := getPaymentPlanReliabilityWindowInTx(ctx, tx, minBlockNumber, maxBlockNumber, true)
		if window == nil || window.completeTime != nil || window.nextBlockNumber != cursor {
			return
		}
		for batch := range slices.Chunk(keys, paymentPlanReliabilityFlushBatchSize) {
			networkIds := make([]server.Id, 0, len(batch))
			clientIds := make([]server.Id, 0, len(batch))
			validBlockCounts := make([]int64, 0, len(batch))
			reliabilitySums := make([]pgtype.Numeric, 0, len(batch))
			for _, key := range batch {
				partial := partials[key]
				networkIds = append(networkIds, key.networkId)
				clientIds = append(clientIds, key.clientId)
				validBlockCounts = append(validBlockCounts, partial.validBlockCount)
				reliabilitySums = append(reliabilitySums, partial.reliabilitySum.numeric())
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				paymentPlanReliabilityFlushSql,
				minBlockNumber,
				maxBlockNumber,
				networkIds,
				clientIds,
				validBlockCounts,
				reliabilitySums,
			))
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE payment_plan_reliability_window
			SET next_block_number = $3
			WHERE min_block_number = $1 AND max_block_number = $2
			`,
			minBlockNumber,
			maxBlockNumber,
			nextBlockNumber,
		))
		committed = true
	}, server.TxReadCommitted)
	return
}

// Folds the partials into the scores exactly as the single statement in
// UpdateNetworkReliabilityScoresInTx writes them: one row per network and
// country, weights normalized by the effective block count.
const paymentPlanReliabilityFoldSql = `
	INSERT INTO network_connection_reliability_score (
		network_id,
		country_location_id,
		independent_reliability_score,
		independent_reliability_weight,
		reliability_score,
		reliability_weight,
		min_block_number,
		max_block_number
	)
	SELECT
		partial.network_id,
		network_client_location_reliability.country_location_id,
		SUM(partial.valid_block_count) AS independent_reliability_score,
		SUM(partial.valid_block_count) / $5::bigint AS independent_reliability_weight,
		SUM(partial.reliability_sum) AS reliability_score,
		SUM(partial.reliability_sum) / $5::bigint AS reliability_weight,
		$3 AS min_block_number,
		$4 AS max_block_number
	FROM payment_plan_reliability_partial partial
	INNER JOIN network_client_location_reliability ON
		network_client_location_reliability.client_id = partial.client_id AND
		network_client_location_reliability.valid = true
	WHERE
		partial.min_block_number = $1 AND
		partial.max_block_number = $2
	GROUP BY
		partial.network_id,
		network_client_location_reliability.country_location_id
	ON CONFLICT (network_id, country_location_id) DO UPDATE
	SET
		independent_reliability_score = EXCLUDED.independent_reliability_score,
		independent_reliability_weight = EXCLUDED.independent_reliability_weight,
		reliability_score = EXCLUDED.reliability_score,
		reliability_weight = EXCLUDED.reliability_weight,
		min_block_number = EXCLUDED.min_block_number,
		max_block_number = EXCLUDED.max_block_number
`

// Commits the network reliability scores of the window [minTime, maxTime] in
// bounded steps: the window definition, the bounded reads and one transaction
// per block chunk, and the fold. A call resumes the committed cursor of an
// earlier interrupted call. progress reports whether any step committed.
func preparePaymentPlanReliabilityWindow(ctx context.Context, minTime time.Time, maxTime time.Time) (progress bool, returnErr error) {
	defer func() {
		if value := recover(); value != nil {
			if err, ok := value.(error); ok {
				returnErr = err
				return
			}
			panic(value)
		}
	}()
	minBlockNumber, maxBlockNumber := paymentPlanReliabilityBlockWindow(minTime, maxTime)
	resetCount := 0
	for {
		if err := ctx.Err(); err != nil {
			return progress, err
		}
		var done bool
		var committed bool
		var reset bool
		var pending *paymentPlanReliabilityWindow
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			done, committed, reset = false, false, false
			pending = nil
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, paymentPlanLockKey))

			window := getPaymentPlanReliabilityWindowInTx(ctx, tx, minBlockNumber, maxBlockNumber, true)
			if window == nil {
				// at most one window accumulates partials; an abandoned one, such
				// as a prediction its plan did not select, is dropped
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					DELETE FROM payment_plan_reliability_window
					WHERE complete_time IS NULL AND NOT (min_block_number = $1 AND max_block_number = $2)
					`,
					minBlockNumber,
					maxBlockNumber,
				))
				blockShift := reliabilityRollupBlockShift(ctx, tx, maxBlockNumber)
				window = &paymentPlanReliabilityWindow{
					minBlockNumber: minBlockNumber,
					maxBlockNumber: maxBlockNumber,
					blockShift:     blockShift,
				}
				coveredBlockCount := reliabilityCoveredBlockCount(ctx, tx, window.shiftedMinBlockNumber(), window.shiftedMaxBlockNumber())
				window.degradedBlockNumbers = reliabilityDegradedBlocks(ctx, tx, window.shiftedMinBlockNumber(), window.shiftedMaxBlockNumber())
				window.effectiveBlockCount = coveredBlockCount - int64(len(window.degradedBlockNumbers))
				window.nextBlockNumber = window.shiftedMinBlockNumber()
				if window.effectiveBlockCount <= 0 {
					// a window with nothing left to judge keeps the previous scores
					glog.Infof("[plan]no usable blocks in reliability window [%d, %d); keeping previous scores\n", window.shiftedMinBlockNumber(), window.shiftedMaxBlockNumber())
					now := server.NowUtc()
					window.completeTime = &now
					done = true
				}
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO payment_plan_reliability_window (
						min_block_number,
						max_block_number,
						block_shift,
						effective_block_count,
						degraded_block_numbers,
						next_block_number,
						create_time,
						complete_time
					) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
					`,
					window.minBlockNumber,
					window.maxBlockNumber,
					window.blockShift,
					window.effectiveBlockCount,
					window.degradedBlockNumbers,
					window.nextBlockNumber,
					server.NowUtc(),
					window.completeTime,
				))
				committed = true
				return
			}

			if window.completeTime != nil {
				if 0 < window.effectiveBlockCount && !paymentPlanReliabilityScoresCurrentInTx(ctx, tx, window) {
					// another window's scores replaced these; recompute from scratch
					server.RaisePgResult(tx.Exec(
						ctx,
						`DELETE FROM payment_plan_reliability_window WHERE min_block_number = $1 AND max_block_number = $2`,
						window.minBlockNumber,
						window.maxBlockNumber,
					))
					committed = true
					reset = true
					return
				}
				done = true
				return
			}

			if window.nextBlockNumber < window.shiftedMaxBlockNumber() {
				// read the next chunk outside this transaction
				pending = window
				return
			}

			server.RaisePgResult(tx.Exec(
				ctx,
				paymentPlanReliabilityFoldSql,
				window.minBlockNumber,
				window.maxBlockNumber,
				window.shiftedMinBlockNumber(),
				window.shiftedMaxBlockNumber(),
				window.effectiveBlockCount,
			))
			// Both bounds identify this fold; a later start can exclude a provider
			// even when the previous window had the same end.
			server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM network_connection_reliability_score WHERE min_block_number <> $1 OR max_block_number <> $2`,
				window.shiftedMinBlockNumber(),
				window.shiftedMaxBlockNumber(),
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM payment_plan_reliability_partial WHERE min_block_number = $1 AND max_block_number = $2`,
				window.minBlockNumber,
				window.maxBlockNumber,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE payment_plan_reliability_window
				SET complete_time = $3
				WHERE min_block_number = $1 AND max_block_number = $2
				`,
				window.minBlockNumber,
				window.maxBlockNumber,
				server.NowUtc(),
			))
			glog.Infof("[plan]reliability window [%d, %d) committed\n", window.shiftedMinBlockNumber(), window.shiftedMaxBlockNumber())
			committed = true
			done = true
		}, server.TxReadCommitted)
		progress = progress || committed
		if pending != nil {
			chunkEndBlockNumber := min(pending.nextBlockNumber+paymentPlanReliabilityChunkBlockCount, pending.shiftedMaxBlockNumber())
			partials := readPaymentPlanReliabilityPartials(ctx, pending.nextBlockNumber, chunkEndBlockNumber, pending.degradedBlockNumbers)
			if commitPaymentPlanReliabilityChunk(ctx, minBlockNumber, maxBlockNumber, pending.nextBlockNumber, chunkEndBlockNumber, partials) {
				progress = true
				if paymentPlanReliabilityChunkCommitted != nil {
					paymentPlanReliabilityChunkCommitted(chunkEndBlockNumber)
				}
			}
			continue
		}
		if done {
			return progress, nil
		}
		if reset {
			resetCount += 1
			if paymentPlanReliabilityResetLimit < resetCount {
				return progress, errPaymentPlanReliabilityReplaced
			}
		}
	}
}
