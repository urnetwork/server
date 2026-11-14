// The unresolved predicate's expression statistics belong to the migration
// catalog. Sample contents and planner choices are separate observations.
package monitor

import "github.com/urnetwork/server/v2026"

// Match the published CASE, including its false branch, without depending on
// the deparser's line breaks. No statistics data or contract rows are read:
// the migration deliberately leaves ANALYZE to a separate operator step.
const transferContractUnresolvedStatisticsArtifactQuery = `EXISTS (
	SELECT 1 FROM pg_catalog.pg_statistic_ext statistics_record
	JOIN pg_catalog.pg_namespace namespace ON namespace.oid = statistics_record.stxnamespace
	WHERE namespace.nspname = 'public'
	  AND statistics_record.stxname = 'transfer_contract_unresolved_predicate'
	  AND statistics_record.stxrelid = to_regclass('public.transfer_contract')
	  AND statistics_record.stxkeys = ''::int2vector
	  AND statistics_record.stxkind = ARRAY['e']::"char"[]
	  AND regexp_replace(pg_get_expr(statistics_record.stxexprs, statistics_record.stxrelid), '[[:space:]]', '', 'g')
	      = 'CASEWHEN(outcomeISNULL)THEN(dispute=false)ELSEfalseEND'
)`

// Follow the migration's position after merges instead of pinning a number.
var transferContractUnresolvedStatisticsMigrationContract = migrationCurrentSuffix(
	server.TransferContractUnresolvedPredicateStatisticsVersion(),
	"transfer_contract unresolved predicate expression statistics",
	transferContractUnresolvedStatisticsArtifactQuery)
