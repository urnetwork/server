package model

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// client_reliability partitioning. The table accumulates one row per connected
// client per block (~65k rows/block, ~94M rows/day); row-by-row DELETE
// retention can neither outrun inflow nor return space (see
// xops/db/client_reliability_partition_plan.md). Instead the table is
// range-partitioned by block_number into daily partitions and retention is
// DROP PARTITION: metadata-only, no dead tuples, no vacuum debt, no index
// bloat, and the disk returns to the OS immediately.
//
// Every reader and writer keys on block_number ranges (score windows, the
// drain upsert, min/backlog probes), so partition pruning applies everywhere.
// Do not add queries that filter only by client/network with no block bound —
// those fan out to every partition.
//
// The cutover from the historical plain table is MigrateClientReliabilityToPartitions
// (run via `bringyourctl model migrate client-reliability-partition`), which
// copies only the retained window into a partitioned staging table and
// rename-swaps it in under a brief ACCESS EXCLUSIVE lock. The recurring
// maintenance (MaintainClientReliabilityPartitions, called from the
// remove_old_client_reliability_stats task) creates partitions ahead of the
// drain and drops expired ones.

const clientReliabilityTable = "client_reliability"
const clientReliabilityStagingTable = "client_reliability_new"
const clientReliabilityOldTable = "client_reliability_old"

// one partition per UTC day: block_number is epoch-minutes, so day bounds are
// [day*1440, (day+1)*1440)
const reliabilityBlocksPerDay = int64(24 * time.Hour / ReliabilityBlockDuration)

// partitions created ahead of the drain. An insert for a block with no
// partition fails (there is deliberately no DEFAULT partition — it would break
// pruning and could never be dropped), so the ahead buffer must comfortably
// exceed any maintenance outage.
const reliabilityPartitionAheadDays = int64(3)

// blocks copied per transaction during the cutover bulk copy (~3h of blocks,
// ~12M rows): bounds per-txn WAL and gives resumable progress
const reliabilitySwapCopyChunkBlocks = int64(180)

// once a catch-up pass has to copy fewer than this many blocks, take the lock
// and finish: the locked tail copy stays small (~minutes of inflow)
const reliabilitySwapTailMaxBlocks = int64(120)

const clientReliabilityColumnList = `
    block_number,
    client_address_hash,
    network_id,
    client_id,
    connection_new_count,
    connection_established_count,
    provide_enabled_count,
    provide_changed_count,
    receive_message_count,
    receive_byte_count,
    send_message_count,
    send_byte_count,
    valid`

// matches only partitions this code created; anything else attached to the
// parent is left alone by the drop pass
var clientReliabilityPartitionNamePattern = regexp.MustCompile(`^client_reliability_p([0-9]{8})$`)

func reliabilityPartitionDay(blockNumber int64) int64 {
	return blockNumber / reliabilityBlocksPerDay
}

// reliabilityPartitionName returns client_reliability_pYYYYMMDD for the UTC
// day (block_number is epoch-minutes, so day index is epoch-days)
func reliabilityPartitionName(day int64) string {
	date := time.Unix(day*24*60*60, 0).UTC().Format("20060102")
	return fmt.Sprintf("client_reliability_p%s", date)
}

func reliabilityPartitionDayFromName(name string) (day int64, ok bool) {
	m := clientReliabilityPartitionNamePattern.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	date, err := time.Parse("20060102", m[1])
	if err != nil {
		return 0, false
	}
	return date.Unix() / (24 * 60 * 60), true
}

func pgRelkind(ctx context.Context, table string) (relkind string, exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT relkind::text FROM pg_class WHERE oid = to_regclass($1)`,
			"public."+table,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&relkind))
				exists = true
			}
		})
	})
	return
}

func IsClientReliabilityPartitioned(ctx context.Context) bool {
	relkind, exists := pgRelkind(ctx, clientReliabilityTable)
	return exists && relkind == "p"
}

// clientReliabilityMaxDrainedBlock reads the drain high-water mark. Blocks
// <= the mark are fully drained and immutable in pg (the drain advances the
// mark only after the block upserts commit, and the announce recorder never
// writes a block that old to redis), which is what makes the unlocked cutover
// copy consistent.
func clientReliabilityMaxDrainedBlock(ctx context.Context) (maxDrainedBlock int64, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT max_drained_block FROM client_reliability_rollup WHERE singleton_id = 1`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&maxDrainedBlock))
				ok = true
			}
		})
	})
	return
}

func listClientReliabilityPartitions(ctx context.Context) (names []string) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT c.relname
			FROM pg_inherits i
			INNER JOIN pg_class c ON c.oid = i.inhrelid
			WHERE i.inhparent = to_regclass($1)
			ORDER BY c.relname
			`,
			"public."+clientReliabilityTable,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var name string
				server.Raise(result.Scan(&name))
				names = append(names, name)
			}
		})
	})
	return
}

// ensureClientReliabilityPartition creates the day's partition under parent if
// missing. Creation takes a brief ACCESS EXCLUSIVE on the parent, so it runs
// with a short lock timeout and reports failure instead of waiting behind a
// long score query (the caller retries on its next run).
func ensureClientReliabilityPartition(ctx context.Context, parent string, day int64) (created bool) {
	name := reliabilityPartitionName(day)
	if _, exists := pgRelkind(ctx, name); exists {
		return false
	}
	server.HandleError(func() {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`))
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
				`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM (%d) TO (%d)`,
				name,
				parent,
				day*reliabilityBlocksPerDay,
				(day+1)*reliabilityBlocksPerDay,
			)))
		}, server.OptNoRetry())
		created = true
	})
	return
}

func dropClientReliabilityPartition(ctx context.Context, name string) (dropped bool) {
	server.HandleError(func() {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`))
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
				`DROP TABLE IF EXISTS %s`,
				name,
			)))
		}, server.OptNoRetry())
		dropped = true
	})
	return
}

// MaintainClientReliabilityPartitions is the partition-mode retention pass:
// create partitions from the retention cutoff through now+ahead (idempotent,
// self-healing after outages), drop partitions entirely older than
// ClientExpiration, and trim the small companion tables to the same horizon.
// A lock-busy create or drop is skipped and retried on the next run.
func MaintainClientReliabilityPartitions(ctx context.Context, now time.Time) (created []string, dropped []string) {
	cutoffBlock := reliabilityBlockNumber(now.Add(-ClientExpiration))
	fromDay := reliabilityPartitionDay(cutoffBlock)
	toDay := reliabilityPartitionDay(reliabilityBlockNumber(now)) + reliabilityPartitionAheadDays

	for day := fromDay; day <= toDay; day += 1 {
		if ensureClientReliabilityPartition(ctx, clientReliabilityTable, day) {
			created = append(created, reliabilityPartitionName(day))
		}
	}

	for _, name := range listClientReliabilityPartitions(ctx) {
		day, ok := reliabilityPartitionDayFromName(name)
		if !ok {
			continue
		}
		if (day+1)*reliabilityBlocksPerDay <= cutoffBlock {
			if dropClientReliabilityPartition(ctx, name) {
				dropped = append(dropped, name)
			}
		}
	}

	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		removeExpiredClientReliabilityCompanions(ctx, tx, cutoffBlock-1)
	})

	return
}

// copyClientReliabilityRange bulk-copies [minBlockNumber, maxBlockNumber) from
// the plain table into the staging partitioned table in chunked transactions.
// ON CONFLICT DO NOTHING makes re-copying a boundary chunk (resume, retry)
// idempotent.
func copyClientReliabilityRange(
	ctx context.Context,
	minBlockNumber int64,
	maxBlockNumber int64,
	logf func(string, ...any),
) (copiedRows int64) {
	totalBlocks := maxBlockNumber - minBlockNumber
	for chunkLo := minBlockNumber; chunkLo < maxBlockNumber; {
		chunkHi := min(chunkLo+reliabilitySwapCopyChunkBlocks, maxBlockNumber)
		var rows int64
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = 0`))
			tag, err := tx.Exec(
				ctx,
				fmt.Sprintf(
					`
					INSERT INTO %s (%s)
					SELECT %s
					FROM %s
					WHERE block_number >= $1 AND block_number < $2
					ON CONFLICT DO NOTHING
					`,
					clientReliabilityStagingTable,
					clientReliabilityColumnList,
					clientReliabilityColumnList,
					clientReliabilityTable,
				),
				chunkLo,
				chunkHi,
			)
			server.Raise(err)
			rows = tag.RowsAffected()
		})
		copiedRows += rows
		logf(
			"copy blocks [%d, %d): %d rows (pass %d%%, %d rows so far)",
			chunkLo,
			chunkHi,
			rows,
			(chunkHi-minBlockNumber)*100/totalBlocks,
			copiedRows,
		)
		chunkLo = chunkHi
	}
	return
}

// MigrateClientReliabilityToPartitions converts client_reliability from a
// plain table to a range-partitioned table, retaining only the last
// ClientExpiration of blocks. The bulk of the retained window is copied
// without any lock (rows at or below the drain high-water mark are immutable),
// then a short ACCESS EXCLUSIVE window copies the tail and rename-swaps the
// tables. The old table is kept as client_reliability_old for validation;
// drop it with FinalizeClientReliabilityPartitionMigration (--finalize) to
// reclaim its disk.
//
// Idempotent/resumable: rerunning after an interruption resumes the copy from
// the staging table's high block; rerunning after completion is a no-op.
// Needs free disk for the retained copy (~live heap + indexes), and expects
// the only pg writer to be the drain task (true in prod; the announce hot
// path writes redis).
func MigrateClientReliabilityToPartitions(
	ctx context.Context,
	dryRun bool,
	logf func(string, ...any),
) error {
	if logf == nil {
		logf = func(format string, args ...any) {
			glog.Infof("[crp]"+format+"\n", args...)
		}
	}

	if IsClientReliabilityPartitioned(ctx) {
		logf("client_reliability is already partitioned; nothing to do")
		return nil
	}
	if relkind, exists := pgRelkind(ctx, clientReliabilityOldTable); exists {
		return fmt.Errorf(
			"%s already exists (relkind %s): a previous swap was not finalized — validate and run --finalize (or drop it) first",
			clientReliabilityOldTable,
			relkind,
		)
	}

	now := server.NowUtc()
	cutoffBlock := reliabilityBlockNumber(now.Add(-ClientExpiration))
	fromDay := reliabilityPartitionDay(cutoffBlock)
	toDay := reliabilityPartitionDay(reliabilityBlockNumber(now)) + reliabilityPartitionAheadDays

	copyFrom := cutoffBlock
	stagingRelkind, stagingExists := pgRelkind(ctx, clientReliabilityStagingTable)
	if stagingExists {
		if stagingRelkind != "p" {
			return fmt.Errorf(
				"%s exists but is not a partitioned table (relkind %s) — inspect and drop it, then rerun",
				clientReliabilityStagingTable,
				stagingRelkind,
			)
		}
		// resume: recopy the staging high block (it may be partial); ON
		// CONFLICT DO NOTHING makes the overlap harmless
		var maxCopied int64
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				fmt.Sprintf(`SELECT COALESCE(MAX(block_number), -1) FROM %s`, clientReliabilityStagingTable),
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&maxCopied))
				}
			})
		})
		if cutoffBlock < maxCopied {
			copyFrom = maxCopied
		}
		logf("staging %s exists: resuming copy from block %d", clientReliabilityStagingTable, copyFrom)
	}

	mark, markOk := clientReliabilityMaxDrainedBlock(ctx)
	logf(
		"plan: retain blocks >= %d (30 days), partitions %s..%s (%d days), drain mark %v",
		cutoffBlock,
		reliabilityPartitionName(fromDay),
		reliabilityPartitionName(toDay),
		toDay-fromDay+1,
		map[bool]any{true: mark, false: "none"}[markOk],
	)
	if dryRun {
		logf("dry run: no changes made")
		return nil
	}

	if !stagingExists {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
				`
				CREATE TABLE %s (
				    block_number bigint NOT NULL,
				    client_address_hash bytea NOT NULL,
				    network_id uuid NOT NULL,
				    client_id uuid NOT NULL,
				    connection_new_count bigint NOT NULL DEFAULT 0,
				    connection_established_count bigint NOT NULL DEFAULT 0,
				    provide_enabled_count bigint NOT NULL DEFAULT 0,
				    provide_changed_count bigint NOT NULL DEFAULT 0,
				    receive_message_count bigint NOT NULL DEFAULT 0,
				    receive_byte_count bigint NOT NULL DEFAULT 0,
				    send_message_count bigint NOT NULL DEFAULT 0,
				    send_byte_count bigint NOT NULL DEFAULT 0,
				    valid bool,

				    PRIMARY KEY (block_number, client_address_hash, client_id)
				) PARTITION BY RANGE (block_number)
				`,
				clientReliabilityStagingTable,
			)))
			// same shape as the historical secondary index: the score queries'
			// valid_counts subquery streams off it in index order
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
				`CREATE INDEX %s_valid_block_number_client_address_hash ON %s (valid, block_number, client_address_hash)`,
				clientReliabilityStagingTable,
				clientReliabilityStagingTable,
			)))
		})
		logf("created staging partitioned table %s", clientReliabilityStagingTable)
	}

	createdCount := 0
	for day := fromDay; day <= toDay; day += 1 {
		if ensureClientReliabilityPartition(ctx, clientReliabilityStagingTable, day) {
			createdCount += 1
		}
	}
	logf("ensured %d day partitions (%d new)", toDay-fromDay+1, createdCount)

	// bulk copy without any lock, chasing the drain high-water mark until the
	// remaining tail is small. With no mark (idle/fresh environments) skip
	// straight to the locked tail copy.
	if !markOk {
		logf("no drain high-water mark: copying everything under the final lock (fine for small or idle tables)")
	}
	for markOk {
		mark, markOk = clientReliabilityMaxDrainedBlock(ctx)
		if !markOk || mark < copyFrom {
			break
		}
		width := mark + 1 - copyFrom
		logf("catch-up pass: copying %d blocks [%d, %d)", width, copyFrom, mark+1)
		copyClientReliabilityRange(ctx, copyFrom, mark+1, logf)
		copyFrom = mark + 1
		if width <= reliabilitySwapTailMaxBlocks {
			break
		}
	}

	// locked tail copy + rename swap. Retry lock timeouts: a long-running
	// score query ahead of us in the lock queue aborts the attempt (so we do
	// not wedge every other query behind our lock request).
	attempts := 5
	for attempt := 1; ; attempt += 1 {
		if IsClientReliabilityPartitioned(ctx) {
			break
		}
		err := attemptClientReliabilityPartitionSwap(ctx, copyFrom)
		if err == nil {
			break
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.LockNotAvailable && attempt < attempts {
			logf("swap lock busy (attempt %d/%d): %s; retrying in 20s", attempt, attempts, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Second):
			}
			continue
		}
		return err
	}
	if !IsClientReliabilityPartitioned(ctx) {
		return fmt.Errorf("swap did not complete — rerun to resume")
	}
	logf("swap complete: client_reliability is now partitioned; previous table kept as %s", clientReliabilityOldTable)

	server.HandleError(func() {
		logf("analyzing client_reliability (non-blocking; safe to interrupt)")
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE client_reliability`))
		}, server.OptNoRetry())
	})

	var oldSize string
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT pg_size_pretty(pg_total_relation_size(to_regclass($1)))`,
			"public."+clientReliabilityOldTable,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&oldSize))
			}
		})
	})
	logf("validate the new table, then reclaim %s by running: bringyourctl model migrate client-reliability-partition --finalize", oldSize)
	return nil
}

// attemptClientReliabilityPartitionSwap runs the final cutover transaction:
// under ACCESS EXCLUSIVE (nothing else can write), copy every remaining block
// >= tailFrom, then rename the plain table aside and the staging table into
// place (with its pk constraint and secondary index taking the canonical
// names). Returns rather than panics so the caller can retry lock timeouts.
func attemptClientReliabilityPartitionSwap(ctx context.Context, tailFrom int64) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("client_reliability partition swap: %v", r)
			}
		}
	}()

	readPkConstraint := func(tx server.PgTx, table string) (conname string) {
		result, err := tx.Query(
			ctx,
			`SELECT conname FROM pg_constraint WHERE conrelid = to_regclass($1) AND contype = 'p'`,
			"public."+table,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&conname))
			}
		})
		return
	}

	server.Tx(ctx, func(tx server.PgTx) {
		// another attempt may have committed the swap already
		var relkind string
		result, err := tx.Query(
			ctx,
			`SELECT relkind::text FROM pg_class WHERE oid = to_regclass($1)`,
			"public."+clientReliabilityTable,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&relkind))
			}
		})
		if relkind == "p" {
			return
		}

		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '15s'`))
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = 0`))
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			`LOCK TABLE %s IN ACCESS EXCLUSIVE MODE`,
			clientReliabilityTable,
		)))

		server.RaisePgResult(tx.Exec(
			ctx,
			fmt.Sprintf(
				`
				INSERT INTO %s (%s)
				SELECT %s
				FROM %s
				WHERE block_number >= $1
				ON CONFLICT DO NOTHING
				`,
				clientReliabilityStagingTable,
				clientReliabilityColumnList,
				clientReliabilityColumnList,
				clientReliabilityTable,
			),
			tailFrom,
		))

		oldPk := readPkConstraint(tx, clientReliabilityTable)
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			`ALTER TABLE %s RENAME TO %s`,
			clientReliabilityTable,
			clientReliabilityOldTable,
		)))
		if oldPk != "" && oldPk != clientReliabilityOldTable+"_pkey" {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
				`ALTER TABLE %s RENAME CONSTRAINT %s TO %s_pkey`,
				clientReliabilityOldTable,
				oldPk,
				clientReliabilityOldTable,
			)))
		}
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			`ALTER INDEX IF EXISTS %s_valid_block_number_client_address_hash RENAME TO %s_valid_block_number_client_address_hash`,
			clientReliabilityTable,
			clientReliabilityOldTable,
		)))

		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			`ALTER TABLE %s RENAME TO %s`,
			clientReliabilityStagingTable,
			clientReliabilityTable,
		)))
		newPk := readPkConstraint(tx, clientReliabilityTable)
		if newPk != "" && newPk != clientReliabilityTable+"_pkey" {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
				`ALTER TABLE %s RENAME CONSTRAINT %s TO %s_pkey`,
				clientReliabilityTable,
				newPk,
				clientReliabilityTable,
			)))
		}
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			`ALTER INDEX IF EXISTS %s_valid_block_number_client_address_hash RENAME TO %s_valid_block_number_client_address_hash`,
			clientReliabilityStagingTable,
			clientReliabilityTable,
		)))
	}, server.OptNoRetry())
	return nil
}

// FinalizeClientReliabilityPartitionMigration drops the pre-partition table
// kept aside by the swap, returning its disk to the OS. Run after validating
// the partitioned table.
func FinalizeClientReliabilityPartitionMigration(
	ctx context.Context,
	logf func(string, ...any),
) error {
	if logf == nil {
		logf = func(format string, args ...any) {
			glog.Infof("[crp]"+format+"\n", args...)
		}
	}

	if !IsClientReliabilityPartitioned(ctx) {
		return fmt.Errorf("client_reliability is not partitioned; refusing to finalize")
	}
	if _, exists := pgRelkind(ctx, clientReliabilityOldTable); !exists {
		logf("%s does not exist; already finalized", clientReliabilityOldTable)
		return nil
	}

	var oldSize string
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT pg_size_pretty(pg_total_relation_size(to_regclass($1)))`,
			"public."+clientReliabilityOldTable,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&oldSize))
			}
		})
	})

	logf("dropping %s (%s)", clientReliabilityOldTable, oldSize)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '15s'`))
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			`DROP TABLE IF EXISTS %s`,
			clientReliabilityOldTable,
		)))
	}, server.OptNoRetry())
	logf("dropped %s: %s returned to the OS", clientReliabilityOldTable, oldSize)
	return nil
}
