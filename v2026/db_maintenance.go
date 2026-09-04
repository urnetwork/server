package server

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	mathrand "math/rand"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/glog/v2026"
)

const DbReindexEpochs = uint64(8)

// transfer_contract is too large to rebuild wholesale, but these small
// high-churn partial indexes accumulate deleted pages as contracts flip from
// open to closed. Maintain them individually so the planner's relpages cost
// stays representative without rebuilding the table's multi-hundred-GB index
// set.
var priorityReindexIndexes = []string{
	"transfer_contract_open_partial_create_time",
	"transfer_contract_pair_open_create_time",
	"transfer_contract_open_destination_partial",
}

func priorityReindexIndexesForEpoch(epoch uint64) []string {
	names := []string{}
	for _, name := range priorityReindexIndexes {
		h := fnv.New64()
		_, _ = h.Write([]byte(name))
		if h.Sum64()%DbReindexEpochs == epoch%DbReindexEpochs {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// These tables are too large or too frequently updated to rebuild as a whole;
// they use partition turnover, targeted indexes, or tuned autovacuum plus an
// explicitly scheduled one-time pg_repack instead.
var dbMaintenanceSkipReindexTables = map[string]bool{
	"client_reliability":                  true,
	"contract_participant":                true,
	"contract_close":                      true,
	"network_client_location_reliability": true,
	"network_client_connection":           true,
	"transfer_contract":                   true,
	"transfer_escrow":                     true,
	"transfer_escrow_sweep":               true,
}

// Daily reliability partitions are dropped whole at retention, so their
// indexes never live long enough to benefit from reindexing.
var dbMaintenanceSkipReindexTablePattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^client_reliability_p[0-9]{8}$`)
})

func dbMaintenanceShouldReindexTable(tableName string) bool {
	return !dbMaintenanceSkipReindexTables[tableName] &&
		!dbMaintenanceSkipReindexTablePattern().MatchString(tableName)
}

// per the posgres docs, remove indexes that end in _ccnew\d* or _ccold\d*
var incompleteIndexNamePattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile("^(?:.*_ccnew\\d*|.*_ccold\\d*)$")
})

func isIncompleteIndexName(indexName string) bool {
	return incompleteIndexNamePattern().MatchString(indexName)
}

func DefaultDbMaintenanceOptions() *DbMaintenanceOptions {
	return &DbMaintenanceOptions{
		Reindex: true,
		Cleanup: true,
		Analyze: true,
	}
}

type DbMaintenanceOptions struct {
	Reindex bool
	Cleanup bool
	Analyze bool
}

type dbMaintenanceObjectStep string

const (
	dbMaintenanceCleanupBefore dbMaintenanceObjectStep = "cleanup-before"
	dbMaintenanceReindex       dbMaintenanceObjectStep = "reindex"
	dbMaintenanceCleanupAfter  dbMaintenanceObjectStep = "cleanup-after"
)

// dbMaintenanceObjectSteps makes cleanup a guard around every rebuild. A
// timed-out REINDEX CONCURRENTLY can leave an invalid _ccnew/_ccold relation;
// cleaning only after the entire epoch lets a later timeout or task
// cancellation strand it, and the next epoch then creates a numbered sibling.
func dbMaintenanceObjectSteps(cleanup bool, reindex bool) []dbMaintenanceObjectStep {
	steps := []dbMaintenanceObjectStep{}
	if cleanup {
		steps = append(steps, dbMaintenanceCleanupBefore)
	}
	if reindex {
		steps = append(steps, dbMaintenanceReindex)
		if cleanup {
			steps = append(steps, dbMaintenanceCleanupAfter)
		}
	}
	return steps
}

// runDbMaintenanceObjectSteps refuses to start a rebuild when its prerequisite
// cleanup failed. A failed rebuild still reaches cleanup-after so its own
// incomplete artifact is removed before maintenance advances to another
// object.
func runDbMaintenanceObjectSteps(steps []dbMaintenanceObjectStep, run func(dbMaintenanceObjectStep) bool) {
	for _, step := range steps {
		if ok := run(step); !ok && step == dbMaintenanceCleanupBefore {
			return
		}
	}
}

func DbMaintenanceWithDefaults(ctx context.Context, epoch uint64) {
	DbMaintenance(ctx, epoch, DefaultDbMaintenanceOptions())
}

func DbMaintenance(ctx context.Context, epoch uint64, opts *DbMaintenanceOptions) {

	// regularly reindex tables to avoid bloat:
	// 1. tables are reindexed over `DbReindexEpochs` epochs
	//    e.g. `DbReindexEpochs=4` means all tables will be reindexed over 4 maintenance epochs
	// 2. ANALYZE is called after each maintenance to update the planner stats

	// note `REINDEX CONCURRENTLY` can be safely run in the background
	// see https://www.postgresql.org/docs/current/sql-reindex.html

	reindex := func(conn PgConn, tableName string) {
		// note "reindex concurrently" can in some rare cases cause a deadlock with autovacuum
		// use a timeout to recover from these cases
		// any reindex taking longer than the timeout should generally be added to `skipReindexTables`
		timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 2*time.Hour)
		defer timeoutCancel()
		RaisePgResult(conn.Exec(
			timeoutCtx,
			`
			REINDEX TABLE CONCURRENTLY
			`+tableName,
		))
	}
	reindexIndex := func(conn PgConn, indexName string) {
		timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 2*time.Hour)
		defer timeoutCancel()
		RaisePgResult(conn.Exec(
			timeoutCtx,
			`REINDEX INDEX CONCURRENTLY `+indexName,
		))
	}

	cleanUpIncompleteIndexes := func(conn PgConn, tableName string) {
		incompleteIndexNames := []string{}

		result, err := conn.Query(
			ctx,
			`
				WITH public_table AS (
					SELECT table_class.oid, table_class.reltoastrelid
					FROM pg_class table_class
					INNER JOIN pg_namespace table_namespace
						ON table_namespace.oid = table_class.relnamespace
					WHERE table_namespace.nspname = 'public'
					  AND table_class.relname = $1
				)
				SELECT index_namespace.nspname, index_class.relname
				FROM public_table
				INNER JOIN pg_index
					ON pg_index.indrelid IN (public_table.oid, public_table.reltoastrelid)
				INNER JOIN pg_class index_class
					ON index_class.oid = pg_index.indexrelid
				INNER JOIN pg_namespace index_namespace
					ON index_namespace.oid = index_class.relnamespace
				WHERE pg_index.indisvalid = false
				  AND NOT EXISTS (
					SELECT 1
					FROM pg_stat_progress_create_index progress
					WHERE progress.relid IN (public_table.oid, public_table.reltoastrelid)
					   OR progress.index_relid = index_class.oid
				  )
				ORDER BY index_namespace.nspname, index_class.relname
			`,
			tableName,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var indexNamespace string
				var indexName string
				Raise(result.Scan(&indexNamespace, &indexName))
				if isIncompleteIndexName(indexName) {
					incompleteIndexNames = append(
						incompleteIndexNames,
						pgx.Identifier{indexNamespace, indexName}.Sanitize(),
					)
				}
			}
		})

		for i, incompleteIndexName := range incompleteIndexNames {
			glog.Infof(
				"[db]maintenance found incomplete index[%d/%d] %s on table %s\n",
				i+1,
				len(incompleteIndexNames),
				incompleteIndexName,
				tableName,
			)
			RaisePgResult(conn.Exec(
				ctx,
				`
				DROP INDEX CONCURRENTLY IF EXISTS 
				`+incompleteIndexName,
			))
		}
	}

	tableNames := []string{}
	reindexTableNames := []string{}

	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				table_name
			FROM information_schema.tables
			WHERE
				table_schema = 'public' AND
				table_type = 'BASE TABLE'
			`,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var tableName string
				Raise(result.Scan(&tableName))
				tableNames = append(tableNames, tableName)
			}
		})

		for _, tableName := range tableNames {
			hash := fnv.New64()
			hash.Write([]byte(tableName))
			b := make([]byte, 8)
			// cycle the hash each generation
			binary.BigEndian.PutUint64(b, epoch/DbReindexEpochs)
			hash.Write(b)
			h := hash.Sum64()
			if h%DbReindexEpochs == epoch%DbReindexEpochs {
				reindexTableNames = append(reindexTableNames, tableName)
			}
		}
	})

	slices.Sort(reindexTableNames)
	reindexIndexNames := priorityReindexIndexesForEpoch(epoch)
	glog.Infof(
		"[db]maintenance %d/%d tables (in random order): %s; priority indexes: %s\n",
		len(reindexTableNames),
		len(tableNames),
		strings.Join(reindexTableNames, ", "),
		strings.Join(reindexIndexNames, ", "),
	)

	mathrand.Shuffle(len(reindexTableNames), func(i int, j int) {
		reindexTableNames[i], reindexTableNames[j] = reindexTableNames[j], reindexTableNames[i]
	})

	// Cleanup and reindex each selected table as one ordered unit. In
	// particular, a failed rebuild gets its cleanup attempt before maintenance
	// advances to the next table, and a failed prerequisite cleanup prevents a
	// new numbered _ccnew sibling from being created.
	for i, reindexTableName := range reindexTableNames {
		steps := dbMaintenanceObjectSteps(
			opts.Cleanup,
			opts.Reindex && dbMaintenanceShouldReindexTable(reindexTableName),
		)
		runDbMaintenanceObjectSteps(steps, func(step dbMaintenanceObjectStep) bool {
			glog.Infof(
				"[db]maintenance table[%d/%d] %s %s\n",
				i+1,
				len(reindexTableNames),
				step,
				reindexTableName,
			)
			startTime := time.Now()
			recovered := HandleError(func() {
				MaintenanceDb(ctx, func(conn PgConn) {
					switch step {
					case dbMaintenanceCleanupBefore, dbMaintenanceCleanupAfter:
						cleanUpIncompleteIndexes(conn, reindexTableName)
					case dbMaintenanceReindex:
						reindex(conn, reindexTableName)
					}
				}, OptNoRetry())
			})
			glog.Infof(
				"[db]maintenance table[%d/%d] %s %s took %.2fs\n",
				i+1,
				len(reindexTableNames),
				step,
				reindexTableName,
				float64(time.Since(startTime)/time.Millisecond)/1000.0,
			)
			return recovered == nil
		})
	}

	if opts.Reindex {
		for i, indexName := range reindexIndexNames {
			// Every priority index currently belongs to transfer_contract. Keep its
			// incomplete-index cleanup adjacent to the individual rebuild just as
			// it is for a table rebuild; the table's hash-selected epoch may differ.
			const tableName = "transfer_contract"
			steps := dbMaintenanceObjectSteps(opts.Cleanup, true)
			runDbMaintenanceObjectSteps(steps, func(step dbMaintenanceObjectStep) bool {
				glog.Infof(
					"[db]maintenance priority index[%d/%d] %s %s\n",
					i+1,
					len(reindexIndexNames),
					step,
					indexName,
				)
				startTime := time.Now()
				recovered := HandleError(func() {
					MaintenanceDb(ctx, func(conn PgConn) {
						switch step {
						case dbMaintenanceCleanupBefore, dbMaintenanceCleanupAfter:
							cleanUpIncompleteIndexes(conn, tableName)
						case dbMaintenanceReindex:
							reindexIndex(conn, indexName)
						}
					}, OptNoRetry())
				})
				glog.Infof(
					"[db]maintenance priority index[%d/%d] %s %s took %.2fs\n",
					i+1,
					len(reindexIndexNames),
					step,
					indexName,
					float64(time.Since(startTime)/time.Millisecond)/1000.0,
				)
				return recovered == nil
			})
		}
	}

	if opts.Analyze {
		// The task queue is tiny but extremely update-heavy. An explicit daily
		// vacuum bounds its poll-index debt even when normal autovacuum timing is
		// unlucky; the database idle-transaction timeout ensures xmin cannot pin
		// these versions indefinitely.
		HandleError(func() {
			MaintenanceDb(ctx, func(conn PgConn) {
				glog.Infof("[db]maintenance vacuum analyze pending_task\n")
				RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) pending_task`))
			}, OptNoRetry())
		})

		HandleError(func() {
			MaintenanceDb(ctx, func(conn PgConn) {
				glog.Infof("[db]maintenance final analyze\n")
				// final analyze
				startTime := time.Now()
				RaisePgResult(conn.Exec(
					ctx,
					`ANALYZE`,
				))
				endTime := time.Now()
				glog.Infof(
					"[db]maintenance final analyze took %.2fs\n",
					float64(endTime.Sub(startTime)/time.Millisecond)/1000.0,
				)
			}, OptNoRetry())
		})
	}
}

// VacuumFullAllTables runs VACUUM (FULL, ANALYZE) on every ordinary public
// table (including partition leaves) except those matched by excludes. An
// exclude matches a table by exact name OR as an underscore-namespaced parent,
// so `client_reliability` also skips client_reliability_new, its partitions,
// and client_reliability_copy_progress.
//
// VACUUM FULL takes an ACCESS EXCLUSIVE lock and rewrites the table + all its
// indexes into fresh compact files, returning reclaimed bloat to the OS (a
// plain VACUUM does not). It therefore (a) must be run with the system offline
// — it blocks all access to each table for the duration — and (b) needs
// transient free disk of roughly the table's current size while the rewrite is
// in flight. Tables are processed one at a time, smallest first, so peak
// transient use is bounded by the largest single table rather than their sum.
//
// Runs on the maintenance connection in autocommit (VACUUM cannot run inside a
// transaction). A table that cannot be locked within lock_timeout, or that
// errors mid-rewrite (e.g. out of disk — VACUUM FULL rolls back cleanly,
// leaving the table intact), is logged and skipped; the run continues.
func VacuumFullAllTables(ctx context.Context, excludes []string, logf func(string, ...any)) {
	skip := func(t string) bool {
		for _, e := range excludes {
			if t == e || strings.HasPrefix(t, e+"_") {
				return true
			}
		}
		return false
	}

	type tableInfo struct {
		name   string
		size   int64
		pretty string
	}

	all := []tableInfo{}
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT c.relname, pg_total_relation_size(c.oid), pg_size_pretty(pg_total_relation_size(c.oid))
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relkind = 'r'
			ORDER BY pg_total_relation_size(c.oid) ASC
			`,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var ti tableInfo
				Raise(result.Scan(&ti.name, &ti.size, &ti.pretty))
				all = append(all, ti)
			}
		})
	}, OptReadWrite())

	todo := []tableInfo{}
	for _, ti := range all {
		if skip(ti.name) {
			logf("skip %s (%s) — excluded", ti.name, ti.pretty)
		} else {
			todo = append(todo, ti)
		}
	}
	logf("VACUUM FULL plan: %d tables to vacuum, %d skipped", len(todo), len(all)-len(todo))

	MaintenanceDb(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, `SET statement_timeout = 0`))
		// so a table held by another session (e.g. the client_reliability copy)
		// is skipped rather than blocking the whole run
		RaisePgResult(conn.Exec(ctx, `SET lock_timeout = '60s'`))

		for i, ti := range todo {
			startTime := time.Now()
			logf("[%d/%d] VACUUM FULL %s (%s) ...", i+1, len(todo), ti.name, ti.pretty)

			// table name comes from the catalog (not user input); interpolate
			// like the REINDEX path above. VACUUM cannot take bind parameters.
			_, err := conn.Exec(ctx, `VACUUM (FULL, ANALYZE) `+ti.name)
			if err != nil {
				logf("[%d/%d] %s SKIPPED/ERROR after %.1fs: %v", i+1, len(todo), ti.name, time.Since(startTime).Seconds(), err)
				continue
			}

			after := ti.pretty
			r2, e2 := conn.Query(ctx, `SELECT pg_size_pretty(pg_total_relation_size(to_regclass($1)))`, "public."+ti.name)
			WithPgResult(r2, e2, func() {
				if r2.Next() {
					Raise(r2.Scan(&after))
				}
			})
			logf("[%d/%d] %s done: %s -> %s in %.1fs", i+1, len(todo), ti.name, ti.pretty, after, time.Since(startTime).Seconds())
		}
	}, OptReadWrite(), OptNoRetry())

	logf("VACUUM FULL complete")
}
