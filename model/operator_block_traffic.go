package model

// The bytes served per operator block, which GET /account/epochs reports next
// to each epoch's points (owner approval 2026-11-14, server#451: "We're adding
// bytes served per epoch to /account/epochs, next to points and amount, with a
// running figure for the current epoch.").
//
// An epoch's points count each operator block's award in the epoch that
// contains the block's end (GetAccountNanoPointsInWindow, owner 2026-11-12).
// Its bytes count the same blocks the same way, so they are the bytes the
// block awards shared their payout points by:
//   - a block whose award has committed: the bytes its payment row for the
//     network records (CommitOperatorBlockPoints), what the award read;
//   - a block without an award yet (the open block, a completed block whose
//     award is still due, or one whose award failed): the bytes read so far
//     with the award's own statement (operatorBlockPointsSweepSql): the sweeps
//     no USDC payment owns that carry payout bytes, so a contract settled
//     without escrow, which has no sweep, counts nothing, of contracts with a
//     credit-bearing outcome that closed at or after the cutoff, by sweep
//     (settlement) time in the block's window past the cutoff.
// The other point rows of an epoch, a payout plan's for traffic before the
// cutoff, have no bytes here: USDC paid that traffic.
//
// No index reads one network's sweeps by sweep time, so a request never reads
// sweeps. A rollup reads each settled stretch of sweep time once, with the
// award's statement and so its access path (transfer_escrow_sweep by sweep
// time, transfer_contract by primary key), in bounded statements outside any
// transaction, and adds each provider network's bytes to its block's row
// (operator_block_traffic_schema.go):
//   - operator_block_traffic: per block, the sweep time its window is read
//     through
//   - operator_block_network_traffic: per block and network, the payout bytes
//     read so far
// The hourly points leaderboard rebuild advances it
// (controller.RebuildPointsLeaderboard). Once a block's award commits, the
// award's rows supersede the block's rollup rows, which the rollup then
// deletes, so the tables hold the open block and the blocks waiting for an
// award. A request reads the award rows and the rollup rows by key.
//
// A rollup figure can differ from the award's only by what changes after the
// rollup read a stretch: a settlement that commits more than the settle time
// after its sweep time, or a contract whose outcome changes later. Each block
// with both logs the two totals when its rollup rows are deleted.

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

// The bounds of one rollup run (AdvanceOperatorBlockTrafficRollup).
type OperatorBlockTrafficRollupSettings struct {
	// A stretch of sweep time is read once this long has passed after its
	// end. A sweep's time is the start of its settlement transaction, which
	// commits after it; the award waits as long after a block ends
	// (OperatorBlockPointsSettleTime).
	SettleTime time.Duration
	// The sweep time one statement reads, as the award reads a block
	// (operatorBlockPointsSweepReadDuration). Non-positive reads none.
	ChunkDuration time.Duration
	// The most statements one run reads. An hour of sweep time is two
	// statements; a run every hour that may read more catches up a backlog.
	// Non-positive reads none.
	MaxChunks int
	// No statement starts after a run has used this long; it is not a
	// statement deadline, so the statement in flight runs to its end.
	// Non-positive starts none.
	RunBudget time.Duration
}

// Eight hours of sweep time a run, each statement about 6 s on Main
// (operatorBlockPointsSweepReadDuration): a week's backlog, such as the open
// block when the rollup first runs, takes about a day of hourly runs.
func DefaultOperatorBlockTrafficRollupSettings() *OperatorBlockTrafficRollupSettings {
	return &OperatorBlockTrafficRollupSettings{
		SettleTime:    OperatorBlockPointsSettleTime,
		ChunkDuration: operatorBlockPointsSweepReadDuration,
		MaxChunks:     16,
		RunBudget:     3 * time.Minute,
	}
}

// Where a rollup run reads: a block, its window past the cutoff, and the
// sweep time the window is read through. existing is false for a block the
// run starts, which has no row yet.
type operatorBlockTrafficCursor struct {
	block       int64
	windowEnd   time.Time
	readThrough time.Time
	existing    bool
}

// The block the rollup reads next, by the end of the settled sweep time: the
// newest block's row while its window is not read through; else the first
// block after it (for an empty rollup, from the block before the one that
// contains settled, which may still wait for its award) whose window past the
// cutoff started before settled and that has no award, from its window start.
// A block with an award needs no rollup. ok is false when no block is due.
func nextOperatorBlockTraffic(ctx context.Context, cutoff time.Time, settled time.Time) (cursor *operatorBlockTrafficCursor, ok bool) {
	var newestBlock int64
	var newestReadThrough time.Time
	hasNewest := false
	server.Db(ctx, func(conn server.PgConn) {
		hasNewest = false
		result, err := conn.Query(ctx, `SELECT block_number, read_through FROM operator_block_traffic ORDER BY block_number DESC LIMIT 1`)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&newestBlock, &newestReadThrough))
				hasNewest = true
			}
		})
	})
	block := max(1, pointsBlockOf(settled)-1)
	if hasNewest {
		if _, windowEnd, ok := OperatorBlockPointsWindow(newestBlock, cutoff); ok && newestReadThrough.Before(windowEnd) {
			return &operatorBlockTrafficCursor{
				block:       newestBlock,
				windowEnd:   windowEnd,
				readThrough: newestReadThrough,
				existing:    true,
			}, true
		}
		block = newestBlock + 1
	}
	for ; operatorBlockStart(block).Before(settled); block += 1 {
		windowStart, windowEnd, ok := OperatorBlockPointsWindow(block, cutoff)
		if !ok || !windowStart.Before(settled) {
			continue
		}
		if GetOperatorBlockPointsAwarded(ctx, []int64{block})[block] {
			continue
		}
		return &operatorBlockTrafficCursor{
			block:       block,
			windowEnd:   windowEnd,
			readThrough: windowStart,
		}, true
	}
	return nil, false
}

// Adds the traffic the run read, [cursor.readThrough, readThrough), to the
// cursor's block, and moves its row to readThrough, in one transaction. The
// row moves only from where the run read it, or is created only when absent,
// so a concurrent run that moved it first wins and this run writes nothing:
// moved is false then.
func writeOperatorBlockTraffic(
	ctx context.Context,
	cursor *operatorBlockTrafficCursor,
	readThrough time.Time,
	traffic *operatorBlockTraffic,
	now time.Time,
) (moved bool) {
	networkIds := slices.SortedFunc(maps.Keys(traffic.payees), server.Id.Cmp)
	byteCounts := make([]int64, 0, len(networkIds))
	for _, networkId := range networkIds {
		byteCounts = append(byteCounts, traffic.payees[networkId].payoutByteCount)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		moved = false
		var tag server.PgTag
		if cursor.existing {
			tag = server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE operator_block_traffic
				SET read_through = $3, update_time = $4
				WHERE block_number = $1 AND read_through = $2
				`,
				cursor.block,
				cursor.readThrough,
				readThrough,
				now,
			))
		} else {
			tag = server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO operator_block_traffic (block_number, read_through, update_time)
				VALUES ($1, $2, $3)
				ON CONFLICT (block_number) DO NOTHING
				`,
				cursor.block,
				readThrough,
				now,
			))
		}
		if tag.RowsAffected() != 1 {
			return
		}
		if 0 < len(networkIds) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO operator_block_network_traffic (block_number, network_id, payout_byte_count)
				SELECT $1, read.network_id, read.payout_byte_count
				FROM unnest($2::uuid[], $3::bigint[]) AS read(network_id, payout_byte_count)
				ON CONFLICT (block_number, network_id) DO UPDATE
				SET payout_byte_count = operator_block_network_traffic.payout_byte_count + EXCLUDED.payout_byte_count
				`,
				cursor.block,
				networkIds,
				byteCounts,
			))
		}
		moved = true
	}, server.TxReadCommitted)
	return moved
}

// AdvanceOperatorBlockTrafficRollup reads the settled sweep time after the
// rollup's cursor with the award's statement, block by block, at most
// settings.MaxChunks statements within settings.RunBudget, and adds each
// provider network's bytes to its block. A block's reads of one run commit in
// one transaction with its cursor (writeOperatorBlockTraffic); reads a
// concurrent run beat are dropped, and this run stops. It then deletes the
// rollup rows of one block whose award has committed
// (removeAwardedOperatorBlockTraffic). Without an earning boundary there are
// no operator block awards, and it reads nothing. Returns the statements it
// read.
func AdvanceOperatorBlockTrafficRollup(
	ctx context.Context,
	settings *OperatorBlockTrafficRollupSettings,
	now time.Time,
) (chunkCount int, returnErr error) {
	defer recoverOperatorBlockPointsError(&returnErr)
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return 0, err
	}
	if transition == nil {
		return 0, nil
	}
	cutoff := transition.Cutoff
	// whole seconds, so a run's last bound is the next run's first exactly
	settled := now.Add(-settings.SettleTime).Truncate(time.Second)
	deadline := server.NowUtc().Add(settings.RunBudget)
	readable := func() bool {
		return chunkCount < settings.MaxChunks && 0 < settings.ChunkDuration && server.NowUtc().Before(deadline)
	}
	for readable() {
		cursor, ok := nextOperatorBlockTraffic(ctx, cutoff, settled)
		if !ok {
			break
		}
		traffic := newOperatorBlockTraffic()
		readThrough := cursor.readThrough
		for readable() {
			if err := ctx.Err(); err != nil {
				return chunkCount, err
			}
			chunkEnd := server.MinTime(server.MinTime(readThrough.Add(settings.ChunkDuration), cursor.windowEnd), settled)
			if !readThrough.Before(chunkEnd) {
				break
			}
			// a statement of its own, outside any transaction, as the award
			// reads (readOperatorBlockTraffic)
			var rows []*operatorBlockSweepRow
			server.MaintenanceDb(ctx, func(conn server.PgConn) {
				rows = nil
				result, err := conn.Query(ctx, operatorBlockPointsSweepSql, readThrough, chunkEnd, cutoff)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						row := &operatorBlockSweepRow{}
						server.Raise(result.Scan(
							&row.payeeNetworkId,
							&row.payoutByteCount,
							&row.minSweepTime,
							&row.sweepCount,
						))
						rows = append(rows, row)
					}
				})
			})
			for _, row := range rows {
				traffic.add(row)
			}
			readThrough = chunkEnd
			chunkCount += 1
		}
		if !cursor.readThrough.Before(readThrough) {
			break
		}
		if !writeOperatorBlockTraffic(ctx, cursor, readThrough, traffic, now) {
			glog.Infof("[obt]block %d moved past %s by another run; dropped the reads to %s\n", cursor.block, cursor.readThrough.Format(time.RFC3339), readThrough.Format(time.RFC3339))
			break
		}
		glog.Infof("[obt]block %d read through %s: %d sweeps of %d networks added\n", cursor.block, readThrough.Format(time.RFC3339), traffic.sweepCount, len(traffic.payees))
		if readThrough.Before(cursor.windowEnd) {
			// settled, or the run's bounds
			break
		}
	}
	removeAwardedOperatorBlockTraffic(ctx)
	return chunkCount, nil
}

// Deletes the rollup rows of the oldest block, other than the newest, whose
// award has committed: the award's rows supersede them, and a request reads
// the award's once it exists (GetAccountServedBytesInWindows). The newest row
// stays, the rollup's cursor. One block a run, about one row per provider
// network of the block. It logs the block's two totals first: the rollup's
// check against the award.
func removeAwardedOperatorBlockTraffic(ctx context.Context) {
	blocks := []int64{}
	server.Db(ctx, func(conn server.PgConn) {
		blocks = []int64{}
		result, err := conn.Query(ctx, `SELECT block_number FROM operator_block_traffic ORDER BY block_number`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var block int64
				server.Raise(result.Scan(&block))
				blocks = append(blocks, block)
			}
		})
	})
	if len(blocks) < 2 {
		return
	}
	older := blocks[:len(blocks)-1]
	awarded := GetOperatorBlockPointsAwarded(ctx, older)
	for _, block := range older {
		if !awarded[block] {
			continue
		}
		var rollupByteCount ByteCount
		var awardByteCount ByteCount
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`
				SELECT
					(SELECT COALESCE(SUM(payout_byte_count), 0)::bigint FROM operator_block_network_traffic WHERE block_number = $1),
					(SELECT COALESCE(SUM(payout_byte_count), 0)::bigint FROM account_payment WHERE payment_plan_id = $2)
				`,
				block,
				OperatorBlockPointsPlanId(block),
			).Scan(&rollupByteCount, &awardByteCount))
		})
		var networkCount int64
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(ctx, `DELETE FROM operator_block_network_traffic WHERE block_number = $1`, block))
			networkCount = tag.RowsAffected()
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM operator_block_traffic WHERE block_number = $1`, block))
		}, server.TxReadCommitted)
		glog.Infof("[obt]block %d awarded %d bytes; its rollup read %d bytes of %d networks, rows deleted\n", block, awardByteCount, rollupByteCount, networkCount)
		return
	}
}

// The bytes served in one window of GET /account/epochs
// (GetAccountServedBytesInWindows).
type AccountServedBytes struct {
	// the network's bytes in the operator blocks that end in the window
	ByteCount ByteCount
	// the blocks' traffic is counted through this time, the earliest over the
	// blocks that have started: a block's end once its award committed, else
	// the sweep time the rollup read it through, or its start before the
	// rollup read any of it. Nil when no block of the window has started.
	AsOf *time.Time
}

// One block's bytes of a network, as GetAccountServedBytesInWindows reads them.
type operatorBlockServedBytes struct {
	awarded         bool
	awardByteCount  ByteCount
	rollupByteCount ByteCount
	// nil before the rollup read any of the block
	readThrough *time.Time
}

// Each listed block, its award's bytes for the network and its rollup's. Each
// lookup reads only its own keys, so each is its own index scan:
// payment_report and the rollup tables by primary key, and the network's
// payments by network_id, which has few rows a network.
const operatorBlockServedBytesSql = `
	WITH report AS MATERIALIZED (
		SELECT payment_plan_id
		FROM payment_report
		WHERE payment_plan_id = ANY($3::uuid[])
	), award AS MATERIALIZED (
		SELECT payment_plan_id, SUM(payout_byte_count)::bigint AS payout_byte_count
		FROM account_payment
		WHERE network_id = $1 AND payment_plan_id = ANY($3::uuid[])
		GROUP BY payment_plan_id
	), progress AS MATERIALIZED (
		SELECT block_number, read_through
		FROM operator_block_traffic
		WHERE block_number = ANY($2::bigint[])
	), traffic AS MATERIALIZED (
		SELECT block_number, payout_byte_count
		FROM operator_block_network_traffic
		WHERE block_number = ANY($2::bigint[]) AND network_id = $1
	)
	SELECT
		block.block_number,
		report.payment_plan_id IS NOT NULL,
		COALESCE(award.payout_byte_count, 0)::bigint,
		COALESCE(traffic.payout_byte_count, 0)::bigint,
		progress.read_through
	FROM unnest($2::bigint[], $3::uuid[]) AS block(block_number, payment_plan_id)
	LEFT JOIN report ON report.payment_plan_id = block.payment_plan_id
	LEFT JOIN award ON award.payment_plan_id = block.payment_plan_id
	LEFT JOIN progress ON progress.block_number = block.block_number
	LEFT JOIN traffic ON traffic.block_number = block.block_number
`

// GetAccountServedBytesInWindows sums the network's bytes served in each
// window as GetAccountNanoPointsInWindow sums its points: the bytes of the
// operator blocks that end in the window, the award's for a block whose award
// has committed, else the rollup's so far. A block that has not started by
// now counts nothing yet. One statement for every window, in order.
func GetAccountServedBytesInWindows(ctx context.Context, networkId server.Id, windows []PointsEpochWindow, now time.Time) []*AccountServedBytes {
	windowBlocks := make([][]int64, len(windows))
	blockSet := map[int64]bool{}
	for i, window := range windows {
		windowBlocks[i] = operatorBlocksEndingIn(window.Start, window.End)
		for _, block := range windowBlocks[i] {
			blockSet[block] = true
		}
	}
	blocks := slices.Sorted(maps.Keys(blockSet))
	planIds := make([]server.Id, 0, len(blocks))
	for _, block := range blocks {
		planIds = append(planIds, OperatorBlockPointsPlanId(block))
	}
	blockBytes := map[int64]*operatorBlockServedBytes{}
	if 0 < len(blocks) {
		server.Db(ctx, func(conn server.PgConn) {
			blockBytes = map[int64]*operatorBlockServedBytes{}
			result, err := conn.Query(ctx, operatorBlockServedBytesSql, networkId, blocks, planIds)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var block int64
					served := &operatorBlockServedBytes{}
					server.Raise(result.Scan(
						&block,
						&served.awarded,
						&served.awardByteCount,
						&served.rollupByteCount,
						&served.readThrough,
					))
					blockBytes[block] = served
				}
			})
		})
	}

	servedBytes := make([]*AccountServedBytes, 0, len(windows))
	for i := range windows {
		served := &AccountServedBytes{}
		for _, block := range windowBlocks[i] {
			blockStart := operatorBlockStart(block)
			if now.Before(blockStart) {
				continue
			}
			countedThrough := blockStart
			if blockServed, ok := blockBytes[block]; ok {
				switch {
				case blockServed.awarded:
					served.ByteCount += blockServed.awardByteCount
					countedThrough = blockStart.Add(SubnetBlockDuration)
				default:
					served.ByteCount += blockServed.rollupByteCount
					if blockServed.readThrough != nil {
						countedThrough = *blockServed.readThrough
					}
				}
			}
			if served.AsOf == nil || countedThrough.Before(*served.AsOf) {
				served.AsOf = &countedThrough
			}
		}
		servedBytes = append(servedBytes, served)
	}
	return servedBytes
}
