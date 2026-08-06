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

	"github.com/urnetwork/glog/v2026"
)

const DbReindexEpochs = uint64(8)

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

	// these tables are too large are updated too frequently to reindex regularly
	// each table here should have some alternate management strategy
	skipReindexTables := map[string]bool{
		"client_reliability":                  true,
		"network_client_location_reliability": true,
		"network_client_connection":           true,
	}
	// daily partitions of client_reliability are dropped whole at retention,
	// so their indexes never live long enough to bloat — reindexing them is
	// wasted work that contends with the maintenance task's create/drop locks
	skipReindexTablePattern := regexp.MustCompile(`^client_reliability_p[0-9]{8}$`)

	reindex := func(conn PgConn, tableName string) {
		if !skipReindexTables[tableName] && !skipReindexTablePattern.MatchString(tableName) {
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
	}

	cleanUpIncompleteIndexes := func(conn PgConn, tableName string) {
		incompleteIndexNames := []string{}

		result, err := conn.Query(
			ctx,
			`
				SELECT
				    pg_class.relname AS index_name
				FROM
				    pg_class
				INNER JOIN
				    pg_index ON pg_index.indexrelid = pg_class.oid
				INNER JOIN
				    pg_class t ON t.oid = pg_index.indrelid
				WHERE
				    pg_index.indisvalid = false AND
				    t.relname = $1
			`,
			tableName,
		)
		WithPgResult(result, err, func() {
			for result.Next() {
				var indexName string
				Raise(result.Scan(&indexName))
				if isIncompleteIndexName(indexName) {
					incompleteIndexNames = append(incompleteIndexNames, indexName)
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
	glog.Infof(
		"[db]maintenance %d/%d tables (in random order): %s\n",
		len(reindexTableNames),
		len(tableNames),
		strings.Join(reindexTableNames, ", "),
	)

	mathrand.Shuffle(len(reindexTableNames), func(i int, j int) {
		reindexTableNames[i], reindexTableNames[j] = reindexTableNames[j], reindexTableNames[i]
	})

	if opts.Reindex {
		// reindex concurrently
		for i, reindexTableName := range reindexTableNames {
			glog.Infof(
				"[db]maintenance reindex[%d/%d] %s\n",
				i+1,
				len(reindexTableNames),
				reindexTableName,
			)

			// pg might raise a deadlock or other unrecoverable error during reindex
			HandleError(func() {
				MaintenanceDb(ctx, func(conn PgConn) {
					// reindex
					startTime := time.Now()
					reindex(conn, reindexTableName)
					endTime := time.Now()
					glog.Infof(
						"[db]maintenance reindex[%d/%d] %s reindex took %.2fs\n",
						i+1,
						len(reindexTableNames),
						reindexTableName,
						float64(endTime.Sub(startTime)/time.Millisecond)/1000.0,
					)
				}, OptNoRetry())
			})
		}
	}

	if opts.Cleanup {
		for i, reindexTableName := range reindexTableNames {
			glog.Infof(
				"[db]maintenance reindex[%d/%d] cleanup %s\n",
				i+1,
				len(reindexTableNames),
				reindexTableName,
			)

			HandleError(func() {
				MaintenanceDb(ctx, func(conn PgConn) {
					startTime := time.Now()
					cleanUpIncompleteIndexes(conn, reindexTableName)
					endTime := time.Now()
					glog.Infof(
						"[db]maintenance reindex[%d/%d] cleanup %s took %.2fs\n",
						i+1,
						len(reindexTableNames),
						reindexTableName,
						float64(endTime.Sub(startTime)/time.Millisecond)/1000.0,
					)
				}, OptNoRetry())
			})
		}
	}

	if opts.Analyze {
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
