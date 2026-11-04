// Regression controls for statistics object identity across the table swap.
package server

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// Replaying a definition captured before renaming binds its unchanged FROM
// clause to the new parent; the original object remains available for rollback.
func TestContractPartitionStatisticsSql(t *testing.T) {
	comment := "planner's \\ predicate"
	statistic := contractStatistics{
		schema: "odd schema", name: "predicate", owner: "owner role", target: 250,
		definition: `CREATE STATISTICS "odd schema".predicate ON (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) FROM public.transfer_contract`,
		comment:    &comment,
	}
	rename, create, err := contractStatisticsSwapSql([]contractStatistics{statistic})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, rename, []string{`ALTER STATISTICS "odd schema"."predicate" RENAME TO "predicate_l"`})
	connect.AssertEqual(t, create, []string{
		statistic.definition,
		`ALTER STATISTICS "odd schema"."predicate" SET STATISTICS 250`,
		`COMMENT ON STATISTICS "odd schema"."predicate" IS E'planner''s \\ predicate'`,
		`ALTER STATISTICS "odd schema"."predicate" OWNER TO "owner role"`,
	})
	parent := statistic
	statistic.name += contractLegacyStatisticsSuffix
	restored, err := contractStatisticsRollbackSql([]contractStatistics{statistic}, []contractStatistics{parent})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, restored, []string{`ALTER STATISTICS "odd schema"."predicate_l" RENAME TO "predicate"`})
	_, err = contractStatisticsRollbackSql([]contractStatistics{statistic}, nil)
	connect.AssertEqual(t, err != nil, true)
	parent.target += 1
	_, err = contractStatisticsRollbackSql([]contractStatistics{statistic}, []contractStatistics{parent})
	connect.AssertEqual(t, err != nil, true)
	statistic.name = strings.Repeat("x", pgMaxIdentifierLength)
	_, _, err = contractStatisticsSwapSql([]contractStatistics{statistic})
	connect.AssertEqual(t, err != nil, true)
}

// LIKE omits dropped attribute slots. Equivalent statistics must match named
// columns even when their physical attribute numbers differ across the swap.
func TestContractPartitionStatisticsDroppedColumns(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		err := contractPartitionTx(ctx, func(tx PgTx) {
			for _, statement := range []string{
				`CREATE TABLE public.contract_statistics_legacy (id uuid, removed integer, "first column" integer, second_column integer, wrong_column integer)`,
				`ALTER TABLE public.contract_statistics_legacy DROP COLUMN removed`,
				`CREATE TABLE public.contract_statistics_parent (LIKE public.contract_statistics_legacy) PARTITION BY RANGE (id)`,
				`CREATE STATISTICS public.contract_statistics_columns_l (dependencies) ON "first column", second_column FROM public.contract_statistics_legacy`,
				`CREATE STATISTICS public.contract_statistics_columns (dependencies) ON "first column", second_column FROM public.contract_statistics_parent`,
			} {
				RaisePgResult(tx.Exec(ctx, statement))
			}
			var legacyKeys, parentKeys string
			var originalOid uint32
			Raise(tx.QueryRow(ctx, `SELECT oid, stxkeys::text FROM pg_statistic_ext WHERE stxnamespace = 'public'::regnamespace AND stxname = 'contract_statistics_columns_l'`).Scan(&originalOid, &legacyKeys))
			Raise(tx.QueryRow(ctx, `SELECT stxkeys::text FROM pg_statistic_ext WHERE stxnamespace = 'public'::regnamespace AND stxname = 'contract_statistics_columns'`).Scan(&parentKeys))
			connect.AssertEqual(t, legacyKeys != parentKeys, true)
			legacy := readContractStatistics(ctx, tx, "contract_statistics_legacy")
			parent := readContractStatistics(ctx, tx, "contract_statistics_parent")
			connect.AssertEqual(t, len(legacy), 1)
			connect.AssertEqual(t, len(parent), 1)
			connect.AssertEqual(t, legacy[0].shape, parent[0].shape)
			statements, err := contractStatisticsRollbackSql(legacy, parent)
			Raise(err)
			connect.AssertEqual(t, statements, []string{`ALTER STATISTICS "public"."contract_statistics_columns_l" RENAME TO "contract_statistics_columns"`})

			// A different column remains a genuine mismatch despite the same
			// published object name, owner, target and statistics kinds.
			RaisePgResult(tx.Exec(ctx, `DROP STATISTICS public.contract_statistics_columns`))
			RaisePgResult(tx.Exec(ctx, `CREATE STATISTICS public.contract_statistics_columns (dependencies) ON "first column", wrong_column FROM public.contract_statistics_parent`))
			wrongParent := readContractStatistics(ctx, tx, "contract_statistics_parent")
			_, err = contractStatisticsRollbackSql(legacy, wrongParent)
			connect.AssertEqual(t, err != nil, true)

			RaisePgResult(tx.Exec(ctx, `DROP STATISTICS public.contract_statistics_columns`))
			RaisePgResult(tx.Exec(ctx, `CREATE STATISTICS public.contract_statistics_columns (dependencies) ON "first column", second_column FROM public.contract_statistics_parent`))
			statements, err = contractStatisticsRollbackSql(legacy, readContractStatistics(ctx, tx, "contract_statistics_parent"))
			Raise(err)
			RaisePgResult(tx.Exec(ctx, `DROP TABLE public.contract_statistics_parent`))
			for _, statement := range statements {
				RaisePgResult(tx.Exec(ctx, statement))
			}
			RaisePgResult(tx.Exec(ctx, `ALTER TABLE public.contract_statistics_legacy RENAME TO contract_statistics_restored`))
			var restoredOid uint32
			Raise(tx.QueryRow(ctx, `SELECT oid FROM pg_statistic_ext WHERE stxnamespace = 'public'::regnamespace AND stxname = 'contract_statistics_columns' AND stxrelid = 'public.contract_statistics_restored'::regclass`).Scan(&restoredOid))
			connect.AssertEqual(t, restoredOid, originalOid)
			RaisePgResult(tx.Exec(ctx, `DROP TABLE public.contract_statistics_restored`))
		})
		connect.AssertEqual(t, err, nil)
	})
}
