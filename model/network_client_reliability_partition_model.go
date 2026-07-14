package model

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
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

// progress ledger for the parallel bulk copy: one row per copied chunk start,
// written in the SAME transaction as the chunk's INSERT so a chunk is recorded
// atomically or not at all. This makes the parallel copy gap-free and exactly
// resumable no matter which workers finished before an interruption. Lives on
// the maintenance pool and is dropped after the swap.
const reliabilityCopyProgressTable = "client_reliability_copy_progress"

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

func ensureClientReliabilityCopyProgress(ctx context.Context) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (chunk_lo bigint PRIMARY KEY)`, reliabilityCopyProgressTable),
		))
	})
}

func dropClientReliabilityCopyProgress(ctx context.Context) {
	server.HandleError(func() {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, reliabilityCopyProgressTable)))
		}, server.OptNoRetry())
	})
}

// loadClientReliabilityDoneChunks returns the set of chunk-start block numbers
// in [minBlockNumber, maxBlockNumber) already recorded in the progress ledger.
func loadClientReliabilityDoneChunks(ctx context.Context, minBlockNumber int64, maxBlockNumber int64) map[int64]bool {
	done := map[int64]bool{}
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			fmt.Sprintf(`SELECT chunk_lo FROM %s WHERE chunk_lo >= $1 AND chunk_lo < $2`, reliabilityCopyProgressTable),
			minBlockNumber,
			maxBlockNumber,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var lo int64
				server.Raise(result.Scan(&lo))
				done[lo] = true
			}
		})
	})
	return done
}

// copyClientReliabilityParallel bulk-copies [minBlockNumber, maxBlockNumber)
// from the plain table into the staging partitioned table using `parallelism`
// concurrent workers on the maintenance pool. Work is split into fixed
// block-range chunks; each chunk's INSERT and its progress-ledger row commit in
// ONE transaction, so the copy is gap-free and exactly resumable — a rerun
// skips chunks already in the ledger, and an interruption loses at most the
// in-flight chunks (which roll back). ON CONFLICT DO NOTHING makes a re-copied
// chunk idempotent. Size the maintenance pool >= parallelism (db_maintenance.yml).
func copyClientReliabilityParallel(
	ctx context.Context,
	minBlockNumber int64,
	maxBlockNumber int64,
	parallelism int,
	logf func(string, ...any),
) error {
	done := loadClientReliabilityDoneChunks(ctx, minBlockNumber, maxBlockNumber)
	pending := []int64{}
	for lo := minBlockNumber; lo < maxBlockNumber; lo += reliabilitySwapCopyChunkBlocks {
		if !done[lo] {
			pending = append(pending, lo)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	logf(
		"parallel copy: %d chunks to copy (%d already done), %d workers, blocks [%d, %d)",
		len(pending), len(done), parallelism, minBlockNumber, maxBlockNumber,
	)

	copySql := fmt.Sprintf(
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
	)
	progressSql := fmt.Sprintf(`INSERT INTO %s (chunk_lo) VALUES ($1) ON CONFLICT DO NOTHING`, reliabilityCopyProgressTable)

	// cancel-on-first-error: a failed chunk stops the other workers so we do not
	// pile more load onto a struggling db, and the ledger lets a rerun resume.
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	work := make(chan int64, len(pending))
	for _, lo := range pending {
		work <- lo
	}
	close(work)

	total := int64(len(pending))
	var completed, copiedRows int64
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup

	for w := 0; w < parallelism; w += 1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for lo := range work {
				if workCtx.Err() != nil {
					return
				}
				hi := min(lo+reliabilitySwapCopyChunkBlocks, maxBlockNumber)

				// server.* raise via panic; recover per chunk so one failure
				// becomes firstErr + cancel rather than crashing the process.
				var rows int64
				err := func() (err error) {
					defer func() {
						if r := recover(); r != nil {
							if e, ok := r.(error); ok {
								err = e
							} else {
								err = fmt.Errorf("%v", r)
							}
						}
					}()
					server.MaintenanceTx(workCtx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(workCtx, `SET LOCAL statement_timeout = 0`))
						tag, e := tx.Exec(workCtx, copySql, lo, hi)
						server.Raise(e)
						rows = tag.RowsAffected() // last attempt wins on retry
						server.RaisePgResult(tx.Exec(workCtx, progressSql, lo))
					})
					return nil
				}()
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					cancel()
					return
				}

				atomic.AddInt64(&copiedRows, rows)
				n := atomic.AddInt64(&completed, 1)
				if n == total || n%25 == 0 {
					logf("parallel copy: %d/%d chunks, %d rows", n, total, atomic.LoadInt64(&copiedRows))
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// copyClientReliabilityOneshot finishes the copy with a SINGLE sequential-scan
// INSERT of the whole retained range, requiring no index on the source. It is
// the disk-emergency path: after the operator drops client_reliability's primary
// key to reclaim its (bloated, ~TB) space, a chunked/indexed copy would seq-scan
// the heap once per chunk, whereas this reads it a single time. The drain must be
// offline so the source is static and there is no live tail to chase — the
// returned copiedThrough is source-max + 1, leaving the swap's locked tail empty.
// ON CONFLICT DO NOTHING skips rows a prior (killed) chunked run already copied.
//
// This is one large transaction (bounded by max_wal_size + checkpointing, no
// replica); it is not resumable mid-INSERT, but re-running re-scans and skips
// duplicates via ON CONFLICT, so it is idempotent.
func copyClientReliabilityOneshot(ctx context.Context, cutoffBlock int64, logf func(string, ...any)) (copiedThrough int64) {
	// ON CONFLICT DO NOTHING (needed only to skip rows a prior partial run
	// already copied) is parallel-restricted in Postgres — it forces a serial
	// INSERT. When the staging table is empty (the common case: a fresh copy,
	// e.g. after dropping the source PK to reclaim disk) there is nothing to
	// conflict with, so drop the clause and let Postgres parallelize the scan
	// and insert. Only a non-empty (resume) staging keeps ON CONFLICT.
	onConflict := ""
	if clientReliabilityStagingEmpty(ctx) {
		logf("oneshot: staging is empty -> parallel INSERT (no ON CONFLICT)")
	} else {
		onConflict = "ON CONFLICT DO NOTHING"
		logf("oneshot: staging is non-empty -> serial INSERT with ON CONFLICT (idempotent resume)")
	}
	logf("oneshot: single sequential-scan copy of blocks >= %d (one pass, no source PK needed)", cutoffBlock)
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = 0`))
		// parallelize the seq scan that feeds the insert
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL max_parallel_workers_per_gather = 8`))
		tag, err := tx.Exec(
			ctx,
			fmt.Sprintf(
				`
				INSERT INTO %s (%s)
				SELECT %s
				FROM %s
				WHERE block_number >= $1
				%s
				`,
				clientReliabilityStagingTable,
				clientReliabilityColumnList,
				clientReliabilityColumnList,
				clientReliabilityTable,
				onConflict,
			),
			cutoffBlock,
		)
		server.Raise(err)
		logf("oneshot: inserted %d rows", tag.RowsAffected())
	}, server.OptNoRetry())

	// static source (drain offline): a copiedThrough past the max block makes the
	// swap's locked tail copy empty
	if maxBlock, ok := maxClientReliabilitySourceBlock(ctx); ok {
		return maxBlock + 1
	}
	return cutoffBlock
}

// clientReliabilityStagingEmpty reports whether the staging table has no rows.
// Cheap: it stops at the first row. Used by the one-shot copy to decide whether
// ON CONFLICT is needed (only a resume into a non-empty staging needs it).
func clientReliabilityStagingEmpty(ctx context.Context) (empty bool) {
	empty = true
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, fmt.Sprintf(`SELECT 1 FROM %s LIMIT 1`, clientReliabilityStagingTable))
		server.WithPgResult(result, err, func() {
			if result.Next() {
				empty = false
			}
		})
	})
	return
}

// buildClientReliabilitySecondaryIndex builds the deferred secondary index on
// the staging table after the bulk load. Deferring it (vs. creating it up front)
// turns per-row index maintenance across the whole copy into a single sorted
// build, which is far cheaper. Idempotent: a prior run's index is left in place
// (the CREATE runs in one transaction, so it is all-or-nothing — a present index
// is a complete one).
func buildClientReliabilitySecondaryIndex(ctx context.Context, logf func(string, ...any)) {
	indexName := clientReliabilityStagingTable + "_valid_block_number_client_address_hash"
	if _, exists := pgRelkind(ctx, indexName); exists {
		return
	}
	logf("building deferred secondary index %s (sorted one-shot build)", indexName)
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = 0`))
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL maintenance_work_mem = '2GB'`))
		// INCLUDE (network_id, client_id) makes the window-score scans index-only:
		// UpdateClientReliabilityScores / UpdateNetworkReliability(Window)Scores now
		// read client_reliability in a single pass (COUNT(*) OVER the valid rows) and
		// need network_id + client_id as payload, which are otherwise heap fetches.
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %s ON %s (valid, block_number, client_address_hash) INCLUDE (network_id, client_id)`,
			indexName,
			clientReliabilityStagingTable,
		)))
	}, server.OptNoRetry())
}

// maxClientReliabilitySourceBlock returns the highest block_number in the plain
// table, or ok=false when it is empty. Used for the idle/test path where there
// is no drain high-water mark to chase.
func maxClientReliabilitySourceBlock(ctx context.Context) (maxBlock int64, ok bool) {
	maxBlock = -1
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, fmt.Sprintf(`SELECT COALESCE(MAX(block_number), -1) FROM %s`, clientReliabilityTable))
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&maxBlock))
			}
		})
	})
	return maxBlock, maxBlock >= 0
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
// The bulk copy runs `parallelism` concurrent workers on the maintenance pool
// (size it >= parallelism in db_maintenance.yml); the drain and app pool are
// untouched. Idempotent/resumable: a progress ledger records each copied chunk
// atomically with its rows, so rerunning after an interruption skips finished
// chunks and rerunning after completion is a no-op. The secondary index is
// built once after the copy (deferred) rather than maintained per row. Needs
// free disk for the retained copy (~live heap + indexes), and expects the only
// pg writer to be the drain task (true in prod; the announce hot path writes
// redis).
//
// --oneshot (parameter oneshot) finishes the copy with a single sequential-scan
// INSERT instead of the parallel chunked copy. It requires no index on the
// source, so it is the path to use after DROP CONSTRAINT frees the source PK's
// space in a disk emergency. It must run with the drain offline (static source).
func MigrateClientReliabilityToPartitions(
	ctx context.Context,
	parallelism int,
	oneshot bool,
	dryRun bool,
	logf func(string, ...any),
) error {
	if logf == nil {
		logf = func(format string, args ...any) {
			glog.Infof("[crp]"+format+"\n", args...)
		}
	}
	if parallelism < 1 {
		parallelism = 1
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

	stagingRelkind, stagingExists := pgRelkind(ctx, clientReliabilityStagingTable)
	if stagingExists && stagingRelkind != "p" {
		return fmt.Errorf(
			"%s exists but is not a partitioned table (relkind %s) — inspect and drop it, then rerun",
			clientReliabilityStagingTable,
			stagingRelkind,
		)
	}

	mark, markOk := clientReliabilityMaxDrainedBlock(ctx)
	logf(
		"plan: retain blocks >= %d (30 days), partitions %s..%s (%d days), drain mark %v, %d copy workers",
		cutoffBlock,
		reliabilityPartitionName(fromDay),
		reliabilityPartitionName(toDay),
		toDay-fromDay+1,
		map[bool]any{true: mark, false: "none"}[markOk],
		parallelism,
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
			// The secondary index (valid, block_number, client_address_hash) —
			// which the score queries' valid_counts subquery streams off in index
			// order — is built AFTER the bulk copy (deferred), not here: a sorted
			// one-shot build is far cheaper than maintaining it row-by-row across
			// the whole copy. See buildClientReliabilitySecondaryIndex.
		})
		logf("created staging partitioned table %s (secondary index deferred)", clientReliabilityStagingTable)
	}

	createdCount := 0
	for day := fromDay; day <= toDay; day += 1 {
		if ensureClientReliabilityPartition(ctx, clientReliabilityStagingTable, day) {
			createdCount += 1
		}
	}
	logf("ensured %d day partitions (%d new)", toDay-fromDay+1, createdCount)

	var copiedThrough int64
	if oneshot {
		// One-shot: a single sequential-scan INSERT of the whole retained range,
		// needing no source index. Use after DROP CONSTRAINT has freed the source
		// PK in a disk emergency. Drain offline => static source, no live tail.
		copiedThrough = copyClientReliabilityOneshot(ctx, cutoffBlock, logf)
	} else {
		// Parallel bulk copy into the staging partitions, chasing the drain
		// high-water mark until the remaining tail is small. Everything at or below
		// the mark is immutable, so this runs with no lock; the small tail above it
		// is copied under the swap lock below. Resumable via the progress ledger.
		ensureClientReliabilityCopyProgress(ctx)

		copiedThrough = cutoffBlock
		if markOk {
			for {
				mark, markOk = clientReliabilityMaxDrainedBlock(ctx)
				if !markOk {
					break
				}
				if mark+1-copiedThrough <= reliabilitySwapTailMaxBlocks {
					break
				}
				if err := copyClientReliabilityParallel(ctx, cutoffBlock, mark+1, parallelism, logf); err != nil {
					return err
				}
				copiedThrough = mark + 1
			}
		} else {
			// no drain mark (idle/test env): the table is static, so copy the whole
			// present range in parallel; the locked tail below is then empty.
			logf("no drain high-water mark: copying the full present range (idle/small table)")
			if maxBlock, ok := maxClientReliabilitySourceBlock(ctx); ok && cutoffBlock <= maxBlock {
				if err := copyClientReliabilityParallel(ctx, cutoffBlock, maxBlock+1, parallelism, logf); err != nil {
					return err
				}
				copiedThrough = maxBlock + 1
			}
		}
	}

	// Build the deferred secondary index now that the bulk is loaded.
	buildClientReliabilitySecondaryIndex(ctx, logf)

	// locked tail copy + rename swap. Retry lock timeouts: a long-running
	// score query ahead of us in the lock queue aborts the attempt (so we do
	// not wedge every other query behind our lock request).
	attempts := 5
	for attempt := 1; ; attempt += 1 {
		if IsClientReliabilityPartitioned(ctx) {
			break
		}
		err := attemptClientReliabilityPartitionSwap(ctx, copiedThrough)
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

	// the copy ledger is no longer needed now that staging is live
	dropClientReliabilityCopyProgress(ctx)

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
