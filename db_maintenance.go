package server

import (
	"context"
	"hash/maphash"
	mathrand "math/rand"
	"regexp"
	"slices"
	"strings"

	"github.com/golang/glog"
)

const DbReindexEpochs = uint64(4)

func DbMaintenance(ctx context.Context, epoch uint64) {

	// regularly reindex tables to avoid bloat:
	// 1. tables are reindexed over `DbReindexEpochs` epochs
	//    e.g. `DbReindexEpochs=4` means all tables will be reindexed over 4 maintenance epochs
	// 2. ANALYZE is called after each maintenance to update the planner stats

	// note `REINDEX CONCURRENTLY` can be safely run in the background
	// see https://www.postgresql.org/docs/current/sql-reindex.html

	cleanUpIncompleteIdexes := func(conn PgConn, tableName string) {
		// per the posgres docs, remove indexes that end in _ccnew\d* or _ccold\d*
		incompleteIndexNamePattern := regexp.MustCompile("^(?:.*_ccnew\\d*|.*_ccold\\d*)$")

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
			var indexName string
			Raise(result.Scan(&indexName))
			if incompleteIndexNamePattern.MatchString(indexName) {
				incompleteIndexNames = append(incompleteIndexNames, indexName)
			}
		})

		if 0 < len(incompleteIndexNames) {

			for _, incompleteIndexName := range incompleteIndexNames {
				glog.Infof("[db]maintenance found incomplete index %s on table %s\n", incompleteIndexName, tableName)
				// FIXME for now just report the indexex, do not drop
				// RaisePgResult(conn.Exec(
				// 	ctx,
				// 	`
				// 	DROP INDEX $1
				// 	`,
				// 	incompleteIndexName,
				// ))
			}
		}
	}

	Db(ctx, func(conn PgConn) {
		tableNames := []string{}

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

		reindexTableNames := []string{}
		for _, tableName := range tableNames {
			var hash maphash.Hash
			hash.Write([]byte(tableName))
			h := hash.Sum64()
			if h%DbReindexEpochs == epoch%DbReindexEpochs {
				reindexTableNames = append(reindexTableNames, tableName)
			}
		}
		slices.Sort(reindexTableNames)

		glog.Infof("[db]maintenance reindex %d/%d tables (in random order): %s\n", len(reindexTableNames), len(tableNames), strings.Join(reindexTableNames, ", "))

		mathrand.Shuffle(len(reindexTableNames), func(i int, j int) {
			reindexTableNames[i], reindexTableNames[j] = reindexTableNames[j], reindexTableNames[i]
		})

		// reindex concurrently
		for i, reindexTableName := range reindexTableNames {
			glog.Infof("[db]maintenance reindex[%d/%d] %s\n", i+1, len(reindexTableNames), reindexTableName)

			// reindex
			RaisePgResult(conn.Exec(
				ctx,
				`
				REINDEX CONCURRENTLY TABLE $1
				`,
				reindexTableName,
			))

			cleanUpIncompleteIdexes(conn, reindexTableName)
		}

		// final analyze
		RaisePgResult(conn.Exec(
			ctx,
			`ANALYZE`,
		))
	})
}
