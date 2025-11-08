package server

import (
	"context"
	"hash/maphash"
	mathrand "math/rand"
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
			RaisePgResult(conn.Exec(
				ctx,
				`
				REINDEX CONCURRENTLY TABLE $1
				`,
				reindexTableName,
			))
		}

		// final analyze
		RaisePgResult(conn.Exec(
			ctx,
			`ANALYZE`,
		))
	})
}
