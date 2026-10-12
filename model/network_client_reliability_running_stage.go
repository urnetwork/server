// Bounded running-window checkpoints.
//
// The checkpointed pass of a running lookback used to aggregate its whole
// re-anchored, entering or leaving block range in one statement of one
// transaction. The seven-day network window re-anchor read about 740M
// client_reliability index entries that way (pg_stat_statements on Main: mean
// 55 minutes, max 98), and a rolling pass after a long task gap read the gap
// twice (max 77 and 111 minutes). Each held its snapshot and xid throughout, so
// vacuum could not remove dead tuples anywhere in the database meanwhile. A
// pass now runs as bounded steps:
//   - plan: a short transaction reads the applied window and makes the
//     decision of the single-transaction pass (newReliabilityRunningStep),
//     fixing the degraded blocks of each range
//   - read: read-only aggregate statements of at most
//     reliabilityRunningChunkBlockCount blocks, each its own transaction
//     without an xid, read the entering (or re-anchored) range and then the
//     leaving range; their per-client counts and numeric sums add exactly in
//     memory
//   - apply: one transaction verifies that the applied window marker is
//     unchanged, writes the running rows from the totals with the statements
//     of the single-transaction pass, and publishes the marker
//
// No table holds partial sums, so the pass writes the rows the
// single-transaction pass wrote and nothing else.
//
// Equivalence. A step's ranges end at the rollup high-water mark, so every block
// in them is drained, and drained client_reliability rows and block health rows
// do not change: a read sees the same rows whenever it runs. The shared-address
// count partitions by block, so a block read sees each partition whole. Totals
// keep the row count as an integer and SUM(1.0/count) as an exact decimal
// (reliabilityExactSum), and the apply converts each total to float8 in SQL,
// where the single statement converts its aggregate. The running rows are
// therefore bit for bit those of a single-transaction pass at plan time. A row
// written into a block after its read (only an out-of-band backfill can) is
// not counted, as that single statement would not count it; rows drained into
// later blocks belong to the next step.
//
// A drain or restart loses the step's reads, as it lost the single statement;
// the next run plans again from the unchanged window. A step whose window
// another writer replaced while it read is planned again from the published
// window.
package model

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Blocks per read-only aggregate statement: half an hour of one-minute
// blocks, about 2.3M client_reliability index entries on Main (about 78k
// clients report per block). The seven-day re-anchor read 0.33 s of history per
// block, so a read takes about ten seconds. Tests lower it to force many reads.
var reliabilityRunningChunkBlockCount int64 = 30

// Observes each completed read, after its transaction. Nil in production;
// tests use it to interrupt or interleave a step at an exact read boundary.
var reliabilityRunningChunkRead func(lookbackIndex int, leaving bool, minBlockNumber int64, maxBlockNumber int64)

// An applied window marker as stored, write tokens included. Current writers
// rotate both tokens on every write and the guard triggers revoke the versions
// on any other write, so an equal generation means no writer published a
// window in between.
type reliabilityRunningGeneration struct {
	exists                           bool
	minBlockNumber                   int64
	maxBlockNumber                   int64
	lastRecomputeBlock               int64
	degradedClassificationVersion    int
	degradedClassificationWriteToken *string
	observationVersion               int
	observationWriteToken            *string
}

// The decision inputs of the marker.
func (self *reliabilityRunningGeneration) window() reliabilityRunningWindow {
	return reliabilityRunningWindow{
		minBlockNumber:                          self.minBlockNumber,
		maxBlockNumber:                          self.maxBlockNumber,
		lastRecomputeBlock:                      self.lastRecomputeBlock,
		degradedClassificationVersion:           self.degradedClassificationVersion,
		degradedClassificationWriteTokenPresent: self.degradedClassificationWriteToken != nil,
		observationVersion:                      self.observationVersion,
		observationWriteTokenPresent:            self.observationWriteToken != nil,
		exists:                                  self.exists,
	}
}

// Equal markers, tokens compared by value. Absent markers are equal.
func (self *reliabilityRunningGeneration) equal(other *reliabilityRunningGeneration) bool {
	equalToken := func(a *string, b *string) bool {
		if a == nil || b == nil {
			return a == nil && b == nil
		}
		return *a == *b
	}
	if self.exists != other.exists {
		return false
	}
	if !self.exists {
		return true
	}
	return self.minBlockNumber == other.minBlockNumber &&
		self.maxBlockNumber == other.maxBlockNumber &&
		self.lastRecomputeBlock == other.lastRecomputeBlock &&
		self.degradedClassificationVersion == other.degradedClassificationVersion &&
		equalToken(self.degradedClassificationWriteToken, other.degradedClassificationWriteToken) &&
		self.observationVersion == other.observationVersion &&
		equalToken(self.observationWriteToken, other.observationWriteToken)
}

// forUpdate locks the marker row, ordering the caller after any other writer
// of it.
func readReliabilityRunningGenerationInTx(
	ctx context.Context,
	tx server.PgTx,
	lookbackIndex int,
	forUpdate bool,
) (generation reliabilityRunningGeneration) {
	lock := ""
	if forUpdate {
		lock = "FOR UPDATE"
	}
	result, err := tx.Query(
		ctx,
		`
		SELECT
			min_block_number,
			max_block_number,
			last_recompute_block,
			degraded_classification_version,
			degraded_classification_write_token::text,
			observation_version,
			observation_write_token::text
		FROM client_reliability_running_window
		WHERE lookback_index = $1
		`+lock,
		lookbackIndex,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(
				&generation.minBlockNumber,
				&generation.maxBlockNumber,
				&generation.lastRecomputeBlock,
				&generation.degradedClassificationVersion,
				&generation.degradedClassificationWriteToken,
				&generation.observationVersion,
				&generation.observationWriteToken,
			))
			generation.exists = true
		}
	})
	return
}

// A block range of a step and the degraded blocks it omits.
type reliabilityRunningJobRange struct {
	minBlockNumber       int64
	maxBlockNumber       int64
	degradedBlockNumbers []int64
}

// A planned step of one lookback. A recompute aggregates the new window as its
// entering range and has no leaving range.
type reliabilityRunningJob struct {
	lookbackIndex      int
	recompute          bool
	prev               reliabilityRunningGeneration
	minBlockNumber     int64
	maxBlockNumber     int64
	lastRecomputeBlock int64
	enter              reliabilityRunningJobRange
	leave              reliabilityRunningJobRange
}

// Plans the step that moves the applied window to [newMin, newMax), or returns
// nil when the applied window is already current. The decision, its
// maintenance deferral and its degraded blocks are those of
// updateClientReliabilityRunningLookbackAtBoundsInTx.
func planReliabilityRunningJobInTx(
	ctx context.Context,
	tx server.PgTx,
	lb reliabilityRunningLookback,
	newMin int64,
	newMax int64,
) (job *reliabilityRunningJob) {
	generation := readReliabilityRunningGenerationInTx(ctx, tx, lb.lookbackIndex, false)
	prev := generation.window()
	step := newReliabilityRunningStep(prev, lb, newMin, newMax, reliabilityRunningPeriodicReanchorAllowed(ctx, tx))
	if step.deferred {
		glog.Infof(
			"[ncr]defer optional running-window re-anchor for lookback %d while VACUUM/index-build/logical-backup work is active; rolling [%d,%d) -> [%d,%d)\n",
			lb.lookbackIndex,
			prev.minBlockNumber,
			prev.maxBlockNumber,
			newMin,
			newMax,
		)
	}
	if step.unchanged(prev, newMin, newMax) {
		return nil
	}

	job = &reliabilityRunningJob{
		lookbackIndex:  lb.lookbackIndex,
		recompute:      step.recompute,
		prev:           generation,
		minBlockNumber: newMin,
		maxBlockNumber: newMax,
	}
	if step.recompute {
		job.lastRecomputeBlock = newMax
		job.enter = reliabilityRunningJobRange{
			minBlockNumber:       newMin,
			maxBlockNumber:       newMax,
			degradedBlockNumbers: reliabilityDegradedBlocks(ctx, tx, newMin, newMax),
		}
		job.leave = reliabilityRunningJobRange{
			minBlockNumber:       newMin,
			maxBlockNumber:       newMin,
			degradedBlockNumbers: []int64{},
		}
	} else {
		job.lastRecomputeBlock = prev.lastRecomputeBlock
		// add the blocks that entered the window: [prevMax, newMax)
		job.enter = reliabilityRunningJobRange{
			minBlockNumber:       prev.maxBlockNumber,
			maxBlockNumber:       newMax,
			degradedBlockNumbers: reliabilityDegradedBlocks(ctx, tx, prev.maxBlockNumber, newMax),
		}
		// subtract the blocks that left the window: [prevMin, newMin)
		job.leave = reliabilityRunningJobRange{
			minBlockNumber:       prev.minBlockNumber,
			maxBlockNumber:       newMin,
			degradedBlockNumbers: reliabilityDegradedBlocks(ctx, tx, prev.minBlockNumber, newMin),
		}
	}
	return job
}

// The valid-only aggregate (reliabilityRunningAggSql) of a block range, with
// the count as bigint and the sum as numeric so that reads add exactly.
const reliabilityRunningValidReadSql = `
	SELECT
		valid_counts.client_id,
		valid_counts.network_id,
		COUNT(*),
		SUM(1.0/valid_counts.valid_client_count),
		0::bigint
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

// The observed aggregate (reliabilityRunningObservedAggSql) of a block range:
// valid rows as above plus invalid observations, which count only presence.
const reliabilityRunningObservedReadSql = `
	SELECT
		observations.client_id,
		observations.network_id,
		SUM(observations.valid_row_count)::bigint,
		SUM(observations.reliability_sum),
		SUM(observations.observed_row_count)::bigint
	FROM (
		SELECT
			valid_counts.network_id,
			valid_counts.client_id,
			COUNT(*) AS valid_row_count,
			SUM(1.0/valid_counts.valid_client_count) AS reliability_sum,
			COUNT(*) AS observed_row_count
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
		UNION ALL
		SELECT
			network_id,
			client_id,
			0,
			0,
			COUNT(*)
		FROM client_reliability
		WHERE
			valid = false AND
			$1 <= block_number AND
			block_number < $2 AND
			NOT (block_number = ANY($3::bigint[]))
		GROUP BY
			network_id,
			client_id
	) observations
	GROUP BY
		observations.network_id,
		observations.client_id
`

// A client under the network its rows carry, as the aggregates group them.
type reliabilityRunningPartialKey struct {
	clientId  server.Id
	networkId server.Id
}

// A client's exact totals over the blocks of a range read so far.
type reliabilityRunningPartial struct {
	validRowCount    int64
	reliabilitySum   reliabilityExactSum
	observedRowCount int64
}

// Reads one range of the step in read-only statements of at most
// reliabilityRunningChunkBlockCount blocks, each its own transaction, and adds
// them exactly. A read joins the totals only after its transaction ends, so a
// retried read is counted once.
func readReliabilityRunningPartials(
	ctx context.Context,
	lb reliabilityRunningLookback,
	blockRange reliabilityRunningJobRange,
	leaving bool,
) map[reliabilityRunningPartialKey]*reliabilityRunningPartial {
	readSql := reliabilityRunningValidReadSql
	if newReliabilityRunningShape(lb).observed {
		readSql = reliabilityRunningObservedReadSql
	}
	partials := map[reliabilityRunningPartialKey]*reliabilityRunningPartial{}
	for minBlockNumber := blockRange.minBlockNumber; minBlockNumber < blockRange.maxBlockNumber; {
		server.Raise(ctx.Err())
		maxBlockNumber := min(minBlockNumber+reliabilityRunningChunkBlockCount, blockRange.maxBlockNumber)
		type read struct {
			key              reliabilityRunningPartialKey
			validRowCount    int64
			reliabilitySum   pgtype.Numeric
			observedRowCount int64
		}
		var reads []read
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			reads = nil
			configureReliabilityRunningCheckpoint(ctx, tx)
			result, err := tx.Query(ctx, readSql, minBlockNumber, maxBlockNumber, blockRange.degradedBlockNumbers)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var r read
					server.Raise(result.Scan(&r.key.clientId, &r.key.networkId, &r.validRowCount, &r.reliabilitySum, &r.observedRowCount))
					reads = append(reads, r)
				}
			})
		}, server.TxReadCommitted)
		for _, r := range reads {
			partial, ok := partials[r.key]
			if !ok {
				partial = &reliabilityRunningPartial{}
				partials[r.key] = partial
			}
			partial.validRowCount += r.validRowCount
			partial.reliabilitySum.add(r.reliabilitySum)
			partial.observedRowCount += r.observedRowCount
		}
		if reliabilityRunningChunkRead != nil {
			reliabilityRunningChunkRead(lb.lookbackIndex, leaving, minBlockNumber, maxBlockNumber)
		}
		minBlockNumber = maxBlockNumber
	}
	return partials
}

// The totals of one range as array parameters $2-$6, in key order.
func reliabilityRunningPartialArguments(partials map[reliabilityRunningPartialKey]*reliabilityRunningPartial) []any {
	keys := slices.Collect(maps.Keys(partials))
	slices.SortFunc(keys, func(a reliabilityRunningPartialKey, b reliabilityRunningPartialKey) int {
		if c := a.clientId.Cmp(b.clientId); c != 0 {
			return c
		}
		return a.networkId.Cmp(b.networkId)
	})
	clientIds := make([]server.Id, 0, len(keys))
	networkIds := make([]server.Id, 0, len(keys))
	validRowCounts := make([]int64, 0, len(keys))
	reliabilitySums := make([]pgtype.Numeric, 0, len(keys))
	observedRowCounts := make([]int64, 0, len(keys))
	for _, key := range keys {
		partial := partials[key]
		clientIds = append(clientIds, key.clientId)
		networkIds = append(networkIds, key.networkId)
		validRowCounts = append(validRowCounts, partial.validRowCount)
		reliabilitySums = append(reliabilitySums, partial.reliabilitySum.numeric())
		observedRowCounts = append(observedRowCounts, partial.observedRowCount)
	}
	return []any{clientIds, networkIds, validRowCounts, reliabilitySums, observedRowCounts}
}

// The totals of one range, bound as $2-$6, in the columns and float8
// conversions of the single-transaction aggregate.
const reliabilityRunningPartialAggSql = `
	SELECT
		partial.network_id,
		partial.client_id,
		partial.valid_row_count::float8 AS ind,
		partial.reliability_sum::float8 AS rel,
		partial.observed_row_count
	FROM unnest($2::uuid[], $3::uuid[], $4::bigint[], $5::numeric[], $6::bigint[])
		AS partial(client_id, network_id, valid_row_count, reliability_sum, observed_row_count)
`

// Publishes the planned step when its previous window is still applied, with
// the statements of updateClientReliabilityRunningLookbackAtBoundsInTx over
// the read totals. Returns false, writing nothing, when another writer
// replaced the window.
func applyReliabilityRunningJobInTx(
	ctx context.Context,
	tx server.PgTx,
	lb reliabilityRunningLookback,
	job *reliabilityRunningJob,
	enter map[reliabilityRunningPartialKey]*reliabilityRunningPartial,
	leave map[reliabilityRunningPartialKey]*reliabilityRunningPartial,
) (applied bool) {
	// the window row lock orders this publication after any other writer's
	generation := readReliabilityRunningGenerationInTx(ctx, tx, lb.lookbackIndex, true)
	if !job.prev.equal(&generation) {
		glog.Infof(
			"[ncr]replan running-window step for lookback %d: window [%d,%d) was replaced while it read\n",
			lb.lookbackIndex,
			job.prev.minBlockNumber,
			job.prev.maxBlockNumber,
		)
		return false
	}
	shape := newReliabilityRunningShape(lb)
	enterArguments := append([]any{lb.lookbackIndex}, reliabilityRunningPartialArguments(enter)...)

	if job.recompute {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM client_reliability_running WHERE lookback_index = $1`,
			lb.lookbackIndex,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO client_reliability_running (
					client_id, lookback_index, network_id, independent_sum, reliability_sum, observed_row_count
				)
				SELECT agg.client_id, $1, agg.network_id, agg.ind, agg.rel, agg.observed_row_count
				FROM (`+reliabilityRunningPartialAggSql+`) agg
				`,
			enterArguments...,
		))
	} else {
		// ADD the blocks that entered the window
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO client_reliability_running (
					client_id, lookback_index, network_id, independent_sum, reliability_sum, observed_row_count
				)
				SELECT agg.client_id, $1, agg.network_id, agg.ind, agg.rel, agg.observed_row_count
				FROM (`+reliabilityRunningPartialAggSql+`) agg
				ON CONFLICT (client_id, lookback_index) DO UPDATE
				SET
					independent_sum = client_reliability_running.independent_sum + EXCLUDED.independent_sum,
					reliability_sum = client_reliability_running.reliability_sum + EXCLUDED.reliability_sum,
					observed_row_count = client_reliability_running.observed_row_count + EXCLUDED.observed_row_count,
					network_id = EXCLUDED.network_id
				`,
			enterArguments...,
		))

		// SUBTRACT the blocks that left the window. The UPDATE only touches
		// existing rows, which is correct: a leaving block's clients were added
		// when that block entered.
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE client_reliability_running r
				SET
					independent_sum = r.independent_sum - agg.ind,
					reliability_sum = CASE WHEN r.independent_sum = agg.ind THEN 0
						ELSE r.reliability_sum - agg.rel END,
					observed_row_count = r.observed_row_count - agg.observed_row_count
				FROM (`+reliabilityRunningPartialAggSql+`) agg
				WHERE r.client_id = agg.client_id AND r.lookback_index = $1
				`,
			append([]any{lb.lookbackIndex}, reliabilityRunningPartialArguments(leave)...)...,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM client_reliability_running WHERE lookback_index = $1 AND `+shape.prunePredicate,
			lb.lookbackIndex,
		))
	}
	writeReliabilityRunningWindow(ctx, tx, lb.lookbackIndex, job.minBlockNumber, job.maxBlockNumber, job.lastRecomputeBlock)
	return true
}

// Moves one lookback's applied window to the window ending at maxTime in
// bounded steps: plan, reads, apply. The window bounds are those of the
// single-transaction pass: the rollup shift is read when the step is planned.
func advanceReliabilityRunningLookback(ctx context.Context, maxTime time.Time, lb reliabilityRunningLookback) {
	baseMaxBlockNumber := (maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)) + 1
	baseMinBlockNumber := maxTime.Add(-lb.lookback).UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)
	for {
		server.Raise(ctx.Err())
		var job *reliabilityRunningJob
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			job = nil
			configureReliabilityRunningCheckpoint(ctx, tx)
			shift := reliabilityRollupBlockShift(ctx, tx, baseMaxBlockNumber)
			job = planReliabilityRunningJobInTx(ctx, tx, lb, baseMinBlockNumber-shift, baseMaxBlockNumber-shift)
		}, server.TxReadCommitted)
		if job == nil {
			return
		}

		enter := readReliabilityRunningPartials(ctx, lb, job.enter, false)
		leave := readReliabilityRunningPartials(ctx, lb, job.leave, true)

		applied := false
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			configureReliabilityRunningCheckpoint(ctx, tx)
			applied = applyReliabilityRunningJobInTx(ctx, tx, lb, job, enter, leave)
		}, server.TxReadCommitted)
		if applied {
			return
		}
		// another writer published a window while the step read; plan again
	}
}
