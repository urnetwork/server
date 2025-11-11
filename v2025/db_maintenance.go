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

	"github.com/golang/glog"
)

const DbReindexEpochs = uint64(8)

// per the posgres docs, remove indexes that end in _ccnew\d* or _ccold\d*
var incompleteIndexNamePattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile("^(?:.*_ccnew\\d*|.*_ccold\\d*)$")
})

func isIncompleteIndexName(indexName string) bool {
	return incompleteIndexNamePattern().MatchString(indexName)
}

func DbMaintenance(ctx context.Context, epoch uint64) {

	// regularly reindex tables to avoid bloat:
	// 1. tables are reindexed over `DbReindexEpochs` epochs
	//    e.g. `DbReindexEpochs=4` means all tables will be reindexed over 4 maintenance epochs
	// 2. ANALYZE is called after each maintenance to update the planner stats

	// note `REINDEX CONCURRENTLY` can be safely run in the background
	// see https://www.postgresql.org/docs/current/sql-reindex.html

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

		for _, incompleteIndexName := range incompleteIndexNames {
			glog.Infof("[db]maintenance found incomplete index %s on table %s\n", incompleteIndexName, tableName)
			RaisePgResult(conn.Exec(
				ctx,
				`
				DROP INDEX CONCURRENTLY IF EXISTS 
				`+incompleteIndexName,
			))
		}
	}

	reindex := func(conn PgConn, tableName string) {
		RaisePgResult(conn.Exec(
			ctx,
			`
			REINDEX TABLE CONCURRENTLY 
			`+tableName,
		))
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
		"[db]maintenance reindex %d/%d tables (in random order): %s\n",
		len(reindexTableNames),
		len(tableNames),
		strings.Join(reindexTableNames, ", "),
	)

	mathrand.Shuffle(len(reindexTableNames), func(i int, j int) {
		reindexTableNames[i], reindexTableNames[j] = reindexTableNames[j], reindexTableNames[i]
	})

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
					"[db]maintenance reindex[%d/%d] %s reindex took %.2fms\n",
					i+1,
					len(reindexTableNames),
					reindexTableName,
					float64(endTime.Sub(startTime)/time.Microsecond)/1000.0,
				)
			})
		})
	}

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
					"[db]maintenance reindex[%d/%d] %s cleanup took %.2fms\n",
					i+1,
					len(reindexTableNames),
					reindexTableName,
					float64(endTime.Sub(startTime)/time.Microsecond)/1000.0,
				)
			})
		})
	}

	HandleError(func() {
		MaintenanceDb(ctx, func(conn PgConn) {

			// final analyze
			startTime := time.Now()
			RaisePgResult(conn.Exec(
				ctx,
				`ANALYZE`,
			))
			endTime := time.Now()
			glog.Infof(
				"[db]maintenance reindex final analyze took %.2fms\n",
				float64(endTime.Sub(startTime)/time.Microsecond)/1000.0,
			)
		})
	})
}
