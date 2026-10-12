// Staged running-window checkpoints.
//
// The checkpointed pass of a running lookback used to aggregate its whole
// re-anchored, entering or leaving block range in one statement of one
// transaction. The seven-day network window re-anchor read about 740M
// client_reliability index entries that way (pg_stat_statements on Main: mean
// 55 minutes, max 98), and a rolling pass after a long task gap read the gap
// twice (max 77 and 111 minutes). Each held its snapshot and xid throughout, so
// vacuum could not remove dead tuples anywhere in the database meanwhile. The
// pass now runs as committed steps, each in its own short transaction:
//   - plan: read the applied window, make the decision of the
//     single-transaction pass (newReliabilityRunningStep), fix the degraded
//     blocks of each range, and record a job
//   - chunk: aggregate at most reliabilityRunningChunkBlockCount blocks of the
//     entering (or re-anchored) range, then of the leaving range, into exact
//     per-client partials behind the job's block cursors
//   - apply: verify that the applied window is still the job's previous
//     window, write the running rows from the partials with the statements of
//     the single-transaction pass, and publish the window marker
//
// Equivalence. A job's ranges end at the rollup high-water mark, so every block
// in them is drained, and drained client_reliability rows and block health rows
// do not change: a chunk reads the same rows whenever it commits. The
// shared-address count partitions by block, so a block chunk sees each
// partition whole. Partials keep the row count as bigint and SUM(1.0/count) as
// numeric, whose addition is exact, and the apply converts each total to float8
// once, where the single statement converts its aggregate. The running rows are
// therefore bit for bit those of a single-transaction pass at plan time. A row
// written into a block after its chunk committed (only an out-of-band backfill
// can) is not counted, as that single statement would not count it; rows
// drained into later blocks belong to the next step.
//
// A job completes at the window it was planned for, even when a later run picks
// it up; that run then plans its next step from the window the job applied, as
// two successive single-transaction passes would. A job whose previous window
// was replaced by another writer is discarded with its partials and planned
// again. Jobs and partials are ordinary logged rows, so a job resumes after a
// drain or restart.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Blocks per committed chunk: half an hour of one-minute blocks, about 2.3M
// client_reliability index entries on Main (about 78k clients report per
// block). The seven-day re-anchor read 0.33 s of history per block, so a chunk
// commits in about ten seconds. Each chunk upserts the partials of the clients
// it saw, so smaller chunks write more partial rows: a seven-day re-anchor
// upserts about 30M. Tests lower it to force many chunks.
var reliabilityRunningChunkBlockCount int64 = 30

// Observes each committed chunk, after its commit. Nil in production; tests use
// it to interrupt a job at an exact chunk boundary.
var reliabilityRunningChunkCommitted func(lookbackIndex int, leaving bool, minBlockNumber int64, maxBlockNumber int64)

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

// A block range of a job and its cursor: [minBlockNumber, nextBlockNumber) is
// committed in the partials.
type reliabilityRunningJobRange struct {
	minBlockNumber       int64
	maxBlockNumber       int64
	nextBlockNumber      int64
	degradedBlockNumbers []int64
}

// Every block of the range is in the partials.
func (self *reliabilityRunningJobRange) done() bool {
	return self.maxBlockNumber <= self.nextBlockNumber
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

// Reads and locks the lookback's pending job, or nil.
func getReliabilityRunningJobInTx(ctx context.Context, tx server.PgTx, lookbackIndex int) (job *reliabilityRunningJob) {
	result, err := tx.Query(
		ctx,
		`
		SELECT
			recompute,
			prev_exists,
			prev_min_block_number,
			prev_max_block_number,
			prev_last_recompute_block,
			prev_degraded_classification_version,
			prev_degraded_classification_write_token::text,
			prev_observation_version,
			prev_observation_write_token::text,
			min_block_number,
			max_block_number,
			last_recompute_block,
			enter_min_block_number,
			enter_max_block_number,
			enter_next_block_number,
			enter_degraded_block_numbers,
			leave_min_block_number,
			leave_max_block_number,
			leave_next_block_number,
			leave_degraded_block_numbers
		FROM client_reliability_running_job
		WHERE lookback_index = $1
		FOR UPDATE
		`,
		lookbackIndex,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			job = &reliabilityRunningJob{
				lookbackIndex: lookbackIndex,
			}
			server.Raise(result.Scan(
				&job.recompute,
				&job.prev.exists,
				&job.prev.minBlockNumber,
				&job.prev.maxBlockNumber,
				&job.prev.lastRecomputeBlock,
				&job.prev.degradedClassificationVersion,
				&job.prev.degradedClassificationWriteToken,
				&job.prev.observationVersion,
				&job.prev.observationWriteToken,
				&job.minBlockNumber,
				&job.maxBlockNumber,
				&job.lastRecomputeBlock,
				&job.enter.minBlockNumber,
				&job.enter.maxBlockNumber,
				&job.enter.nextBlockNumber,
				&job.enter.degradedBlockNumbers,
				&job.leave.minBlockNumber,
				&job.leave.maxBlockNumber,
				&job.leave.nextBlockNumber,
				&job.leave.degradedBlockNumbers,
			))
		}
	})
	return
}

// A concurrent planner of the same lookback fails on the primary key.
func insertReliabilityRunningJobInTx(ctx context.Context, tx server.PgTx, job *reliabilityRunningJob) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO client_reliability_running_job (
			lookback_index,
			recompute,
			prev_exists,
			prev_min_block_number,
			prev_max_block_number,
			prev_last_recompute_block,
			prev_degraded_classification_version,
			prev_degraded_classification_write_token,
			prev_observation_version,
			prev_observation_write_token,
			min_block_number,
			max_block_number,
			last_recompute_block,
			enter_min_block_number,
			enter_max_block_number,
			enter_next_block_number,
			enter_degraded_block_numbers,
			leave_min_block_number,
			leave_max_block_number,
			leave_next_block_number,
			leave_degraded_block_numbers,
			create_time
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8::uuid, $9, $10::uuid, $11, $12, $13,
			$14, $15, $16, $17, $18, $19, $20, $21, $22
		)
		`,
		job.lookbackIndex,
		job.recompute,
		job.prev.exists,
		job.prev.minBlockNumber,
		job.prev.maxBlockNumber,
		job.prev.lastRecomputeBlock,
		job.prev.degradedClassificationVersion,
		job.prev.degradedClassificationWriteToken,
		job.prev.observationVersion,
		job.prev.observationWriteToken,
		job.minBlockNumber,
		job.maxBlockNumber,
		job.lastRecomputeBlock,
		job.enter.minBlockNumber,
		job.enter.maxBlockNumber,
		job.enter.nextBlockNumber,
		job.enter.degradedBlockNumbers,
		job.leave.minBlockNumber,
		job.leave.maxBlockNumber,
		job.leave.nextBlockNumber,
		job.leave.degradedBlockNumbers,
		server.NowUtc(),
	))
}

// Returns the lookback's pending job, or plans one that moves the applied
// window to [newMin, newMax), or returns nil when the applied window is already
// current. A pending job whose previous window is no longer applied is
// discarded with its partials. The decision, its maintenance deferral and its
// degraded blocks are those of
// updateClientReliabilityRunningLookbackAtBoundsInTx.
func planReliabilityRunningJobInTx(
	ctx context.Context,
	tx server.PgTx,
	lb reliabilityRunningLookback,
	newMin int64,
	newMax int64,
) (job *reliabilityRunningJob) {
	job = getReliabilityRunningJobInTx(ctx, tx, lb.lookbackIndex)
	generation := readReliabilityRunningGenerationInTx(ctx, tx, lb.lookbackIndex, false)
	if job != nil {
		if job.prev.equal(&generation) {
			return job
		}
		glog.Infof(
			"[ncr]discard running-window step for lookback %d planned from a replaced window [%d,%d)\n",
			lb.lookbackIndex,
			job.prev.minBlockNumber,
			job.prev.maxBlockNumber,
		)
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM client_reliability_running_job WHERE lookback_index = $1`,
			lb.lookbackIndex,
		))
		job = nil
	}

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
			nextBlockNumber:      newMin,
			degradedBlockNumbers: reliabilityDegradedBlocks(ctx, tx, newMin, newMax),
		}
		job.leave = reliabilityRunningJobRange{
			minBlockNumber:       newMin,
			maxBlockNumber:       newMin,
			nextBlockNumber:      newMin,
			degradedBlockNumbers: []int64{},
		}
	} else {
		job.lastRecomputeBlock = prev.lastRecomputeBlock
		// add the blocks that entered the window: [prevMax, newMax)
		job.enter = reliabilityRunningJobRange{
			minBlockNumber:       prev.maxBlockNumber,
			maxBlockNumber:       newMax,
			nextBlockNumber:      prev.maxBlockNumber,
			degradedBlockNumbers: reliabilityDegradedBlocks(ctx, tx, prev.maxBlockNumber, newMax),
		}
		// subtract the blocks that left the window: [prevMin, newMin)
		job.leave = reliabilityRunningJobRange{
			minBlockNumber:       prev.minBlockNumber,
			maxBlockNumber:       newMin,
			nextBlockNumber:      prev.minBlockNumber,
			degradedBlockNumbers: reliabilityDegradedBlocks(ctx, tx, prev.minBlockNumber, newMin),
		}
	}
	insertReliabilityRunningJobInTx(ctx, tx, job)
	return job
}

// One chunk of the valid-only aggregate (reliabilityRunningAggSql) added to the
// job's partials: the same rows and per-row terms, with the count kept as
// bigint and the sum as numeric so that chunks add exactly. The block range and
// degraded blocks take the aggregate's parameter positions.
const reliabilityRunningValidChunkSql = `
	INSERT INTO client_reliability_running_partial (
		lookback_index,
		leaving,
		client_id,
		network_id,
		valid_row_count,
		reliability_sum,
		observed_row_count
	)
	SELECT
		$4,
		$5,
		valid_counts.client_id,
		valid_counts.network_id,
		COUNT(*),
		SUM(1.0/valid_counts.valid_client_count),
		0
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
	ON CONFLICT (lookback_index, leaving, client_id, network_id) DO UPDATE
	SET
		valid_row_count = client_reliability_running_partial.valid_row_count + EXCLUDED.valid_row_count,
		reliability_sum = client_reliability_running_partial.reliability_sum + EXCLUDED.reliability_sum,
		observed_row_count = client_reliability_running_partial.observed_row_count + EXCLUDED.observed_row_count
`

// One chunk of the observed aggregate (reliabilityRunningObservedAggSql): valid
// rows as above plus invalid observations, which count only presence.
const reliabilityRunningObservedChunkSql = `
	INSERT INTO client_reliability_running_partial (
		lookback_index,
		leaving,
		client_id,
		network_id,
		valid_row_count,
		reliability_sum,
		observed_row_count
	)
	SELECT
		$4,
		$5,
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
	ON CONFLICT (lookback_index, leaving, client_id, network_id) DO UPDATE
	SET
		valid_row_count = client_reliability_running_partial.valid_row_count + EXCLUDED.valid_row_count,
		reliability_sum = client_reliability_running_partial.reliability_sum + EXCLUDED.reliability_sum,
		observed_row_count = client_reliability_running_partial.observed_row_count + EXCLUDED.observed_row_count
`

// Aggregates the next chunk of the lookback's pending job, entering (or
// re-anchored) blocks first and then leaving blocks, and advances that range's
// cursor. ok is false when no job is pending or it has no blocks left.
func runReliabilityRunningChunkInTx(
	ctx context.Context,
	tx server.PgTx,
	lb reliabilityRunningLookback,
) (leaving bool, minBlockNumber int64, maxBlockNumber int64, ok bool) {
	job := getReliabilityRunningJobInTx(ctx, tx, lb.lookbackIndex)
	if job == nil {
		return
	}
	blockRange := &job.enter
	cursorColumn := "enter_next_block_number"
	if blockRange.done() {
		leaving = true
		blockRange = &job.leave
		cursorColumn = "leave_next_block_number"
	}
	if blockRange.done() {
		return
	}
	minBlockNumber = blockRange.nextBlockNumber
	maxBlockNumber = min(minBlockNumber+reliabilityRunningChunkBlockCount, blockRange.maxBlockNumber)

	chunkSql := reliabilityRunningValidChunkSql
	if newReliabilityRunningShape(lb).observed {
		chunkSql = reliabilityRunningObservedChunkSql
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		chunkSql,
		minBlockNumber,
		maxBlockNumber,
		blockRange.degradedBlockNumbers,
		lb.lookbackIndex,
		leaving,
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE client_reliability_running_job SET `+cursorColumn+` = $2 WHERE lookback_index = $1`,
		lb.lookbackIndex,
		maxBlockNumber,
	))
	ok = true
	return
}

// The committed partials of one range, in the columns and float8 conversions of
// the single-transaction aggregate.
func reliabilityRunningPartialAggSql(leaving bool) string {
	return fmt.Sprintf(
		`
		SELECT
			network_id,
			client_id,
			valid_row_count::float8 AS ind,
			reliability_sum::float8 AS rel,
			observed_row_count
		FROM client_reliability_running_partial
		WHERE lookback_index = $1 AND leaving = %t
		`,
		leaving,
	)
}

// Publishes the lookback's completed job when its previous window is still
// applied, with the statements of
// updateClientReliabilityRunningLookbackAtBoundsInTx over the partials, and
// returns it. It returns nil when no completed job is pending, and discards a
// job whose previous window was replaced.
func applyReliabilityRunningJobInTx(
	ctx context.Context,
	tx server.PgTx,
	lb reliabilityRunningLookback,
) (applied *reliabilityRunningJob) {
	job := getReliabilityRunningJobInTx(ctx, tx, lb.lookbackIndex)
	if job == nil || !job.enter.done() || !job.leave.done() {
		return nil
	}
	// the window row lock orders this publication after any other writer's
	generation := readReliabilityRunningGenerationInTx(ctx, tx, lb.lookbackIndex, true)
	if !job.prev.equal(&generation) {
		glog.Infof(
			"[ncr]discard running-window step for lookback %d planned from a replaced window [%d,%d)\n",
			lb.lookbackIndex,
			job.prev.minBlockNumber,
			job.prev.maxBlockNumber,
		)
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM client_reliability_running_job WHERE lookback_index = $1`,
			lb.lookbackIndex,
		))
		return nil
	}
	shape := newReliabilityRunningShape(lb)

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
				FROM (`+reliabilityRunningPartialAggSql(false)+`) agg
				`,
			lb.lookbackIndex,
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
				FROM (`+reliabilityRunningPartialAggSql(false)+`) agg
				ON CONFLICT (client_id, lookback_index) DO UPDATE
				SET
					independent_sum = client_reliability_running.independent_sum + EXCLUDED.independent_sum,
					reliability_sum = client_reliability_running.reliability_sum + EXCLUDED.reliability_sum,
					observed_row_count = client_reliability_running.observed_row_count + EXCLUDED.observed_row_count,
					network_id = EXCLUDED.network_id
				`,
			lb.lookbackIndex,
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
				FROM (`+reliabilityRunningPartialAggSql(true)+`) agg
				WHERE r.client_id = agg.client_id AND r.lookback_index = $1
				`,
			lb.lookbackIndex,
		))

		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM client_reliability_running WHERE lookback_index = $1 AND `+shape.prunePredicate,
			lb.lookbackIndex,
		))
	}
	writeReliabilityRunningWindow(ctx, tx, lb.lookbackIndex, job.minBlockNumber, job.maxBlockNumber, job.lastRecomputeBlock)
	// the partials cascade
	server.RaisePgResult(tx.Exec(
		ctx,
		`DELETE FROM client_reliability_running_job WHERE lookback_index = $1`,
		lb.lookbackIndex,
	))
	return job
}

// Moves one lookback's applied window to the window ending at maxTime in
// committed steps: plan, chunks, apply. A job left pending by an interrupted
// run is completed first, and the loop then plans from the window it applied.
// The window bounds are those of the single-transaction pass: the rollup shift
// is read when each step is planned.
func advanceReliabilityRunningLookback(ctx context.Context, maxTime time.Time, lb reliabilityRunningLookback) {
	baseMaxBlockNumber := (maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)) + 1
	baseMinBlockNumber := maxTime.Add(-lb.lookback).UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond)
	for {
		server.Raise(ctx.Err())
		var job *reliabilityRunningJob
		var newMin int64
		var newMax int64
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			job = nil
			configureReliabilityRunningCheckpoint(ctx, tx)
			shift := reliabilityRollupBlockShift(ctx, tx, baseMaxBlockNumber)
			newMin = baseMinBlockNumber - shift
			newMax = baseMaxBlockNumber - shift
			job = planReliabilityRunningJobInTx(ctx, tx, lb, newMin, newMax)
		}, server.TxReadCommitted)
		if job == nil {
			return
		}

		for {
			server.Raise(ctx.Err())
			var leaving bool
			var minBlockNumber int64
			var maxBlockNumber int64
			var ok bool
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				ok = false
				configureReliabilityRunningCheckpoint(ctx, tx)
				leaving, minBlockNumber, maxBlockNumber, ok = runReliabilityRunningChunkInTx(ctx, tx, lb)
			}, server.TxReadCommitted)
			if !ok {
				break
			}
			if reliabilityRunningChunkCommitted != nil {
				reliabilityRunningChunkCommitted(lb.lookbackIndex, leaving, minBlockNumber, maxBlockNumber)
			}
		}

		var applied *reliabilityRunningJob
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			applied = nil
			configureReliabilityRunningCheckpoint(ctx, tx)
			applied = applyReliabilityRunningJobInTx(ctx, tx, lb)
		}, server.TxReadCommitted)
		if applied != nil && applied.minBlockNumber == newMin && applied.maxBlockNumber == newMax {
			return
		}
		// another step remains: the applied job was planned for an earlier
		// window, or it was discarded
	}
}
