// The unresolved predicate's statistics migration
// (db_transfer_contract_unresolved_statistics.go), read back from the
// migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The predicate exactly as the unresolved partial indexes and the payer reads
// write it: the statistics object must describe the same expression.
const unresolvedPredicateTestSql = `(CASE WHEN outcome IS NULL THEN dispute = false ELSE false END)`

// The statistics migration is appended after the unresolved index family it
// serves, its version is its position, and it is a plain SQL migration.
func TestTransferContractUnresolvedPredicateStatisticsMigrationIsAppendedAfterItsIndexes(t *testing.T) {
	version := TransferContractUnresolvedPredicateStatisticsVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != transferContractUnresolvedPredicateStatisticsMigration {
		t.Fatalf("statistics migration version %d is not its position", version)
	}
	for _, index := range []string{
		"transfer_contract_unresolved_source_pair_create_time",
		"transfer_contract_unresolved_destination_pair_create_time",
		"transfer_contract_unresolved_payer_transfer_byte_count",
	} {
		if position := migrationIndex(t, index); version-1 <= position {
			t.Fatalf("statistics migration %d is not after %s at %d", version, index, position+1)
		}
		online, ok := migrations[migrationIndex(t, index)].(*OnlineSqlMigration)
		if !ok || !strings.Contains(online.sql, unresolvedPredicateTestSql) {
			t.Fatalf("%s is not defined on the predicate %s", index, unresolvedPredicateTestSql)
		}
	}
}

// The migration creates the object on the indexes' exact predicate, only when it
// is missing and within a five-second lock bound, and it never samples the
// table. The operator's statements create the same object under the same bound
// and sample only the predicate's two columns.
func TestTransferContractUnresolvedPredicateStatisticsMigrationIsBoundedAndSamplesNothing(t *testing.T) {
	sql := transferContractUnresolvedPredicateStatisticsMigration.sql
	for _, marker := range []string{
		`stxname = '` + TransferContractUnresolvedPredicateStatisticsName + `'`,
		`stxnamespace = 'public'::regnamespace`,
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`CREATE STATISTICS public.` + TransferContractUnresolvedPredicateStatisticsName,
		`ON ` + unresolvedPredicateTestSql,
		`FROM public.transfer_contract;`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("statistics migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "CREATE STATISTICS") {
		t.Error("statistics migration bounds its lock wait after it creates the object")
	}
	if regexp.MustCompile(`(?i)\b(ANALYZE|VACUUM)\b`).MatchString(sql) {
		t.Error("statistics migration samples or vacuums transfer_contract")
	}

	operator := TransferContractUnresolvedPredicateStatisticsOperatorSql
	if !strings.HasPrefix(operator, `SET lock_timeout = '5s'; `) ||
		!strings.Contains(operator, `CREATE STATISTICS IF NOT EXISTS public.`+TransferContractUnresolvedPredicateStatisticsName+` ON `+unresolvedPredicateTestSql+` FROM public.transfer_contract`) {
		t.Errorf("operator statement is not the migration's object under its lock bound: %q", operator)
	}
	if TransferContractUnresolvedPredicateAnalyzeSql != `ANALYZE public.transfer_contract (outcome, dispute)` {
		t.Errorf("operator sample is not the predicate's two columns: %q", TransferContractUnresolvedPredicateAnalyzeSql)
	}
}
