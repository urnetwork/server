// Regression controls for statistics object identity across the table swap.
package server

import (
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
