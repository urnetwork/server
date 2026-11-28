// The unresolved predicate's selectivity for the planner (CPUOPT4 Part D #1).
//
// The payer reads (model GetOpenTransferByteCount and ReadPayerDueContracts)
// filter transfer_contract with the opaque unresolved predicate
// `CASE WHEN outcome IS NULL THEN dispute = false ELSE false END`, the
// planner boundary the 2026-09-08 repair chose so the legacy `open` and
// `outcome IS NULL` partial indexes can never serve them. PostgreSQL has no
// statistics for an opaque boolean expression and estimates it true for half of
// the table. With the open set drained (Main, 2026-11-04: 4.41M entries left in
// the 10-83 GB unresolved partial indexes), that half-table guess times the
// partial index's pages per entry priced transfer_contract_unresolved_payer_
// transfer_byte_count above a PostgreSQL 18 skip scan of (open,
// payer_network_id, transfer_byte_count) that reads every contract the payer
// ever had. The payer open-sum went from about 20 ms to about 590 ms a request
// and was 36% of the primary's execution time; the plan flipped each time a
// transfer_contract vacuum counted the open set below the crossover.
//
// An expression statistics object gives the planner the predicate's real
// selectivity. The predicate stays the boundary: statistics change estimates,
// never which index can serve a query. transfer_contract is a plain table on
// Main (relkind r), so the object's non-inherited data is what the planner
// reads for these scans.
package server

// The name of the statistics object on the unresolved predicate.
const TransferContractUnresolvedPredicateStatisticsName = "transfer_contract_unresolved_predicate"

// The migration's statement. It creates the statistics object only when it is
// missing. CREATE STATISTICS takes SHARE UPDATE EXCLUSIVE on the table even with
// IF NOT EXISTS, and it holds a snapshot while it waits, so a migration queued
// behind transfer_contract's hours-long anti-wraparound vacuum would pin the
// cluster's vacuum horizon. The existence check needs no table lock, so an
// operator can create the object out of band at a vacuum boundary
// (TransferContractUnresolvedPredicateStatisticsOperatorSql) and the migration
// then runs as a no-op; when it does create the object, its wait is bounded at
// five seconds, and a refused lock fails the migration for a later retry.
//
// The migration does not ANALYZE: a transfer_contract sample is the operator's
// separate, time-bounded step (TransferContractUnresolvedPredicateAnalyzeSql),
// and autovacuum's next ANALYZE of the table fills the object too. Until then
// the object holds no data and the planner keeps its default estimate.
const TransferContractUnresolvedPredicateStatisticsSql = `
	DO $statistics$
	BEGIN
		IF NOT EXISTS (
			SELECT 1 FROM pg_catalog.pg_statistic_ext
			WHERE stxname = 'transfer_contract_unresolved_predicate'
			  AND stxnamespace = 'public'::regnamespace
		) THEN
			PERFORM set_config('lock_timeout', '5s', true);
			CREATE STATISTICS public.transfer_contract_unresolved_predicate
				ON (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END)
				FROM public.transfer_contract;
		END IF;
	END
	$statistics$
`

// The operator's out-of-band creation, on a direct session: the same object,
// with the same five-second lock bound. Retry it until a transfer_contract
// vacuum boundary lets it through; it never waits behind a running vacuum.
const TransferContractUnresolvedPredicateStatisticsOperatorSql = `SET lock_timeout = '5s'; CREATE STATISTICS IF NOT EXISTS public.transfer_contract_unresolved_predicate ON (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) FROM public.transfer_contract`

// The operator's sample that fills the object without waiting for autovacuum:
// the predicate's two columns only, so it samples 300 x the columns' default
// target (30,000 rows) and rewrites no other column's statistics. Bound it with
// a statement_timeout on its session; a timeout loses nothing, and autovacuum's
// next ANALYZE fills the object.
const TransferContractUnresolvedPredicateAnalyzeSql = `ANALYZE public.transfer_contract (outcome, dispute)`

// The statistics migration in the migration list.
var transferContractUnresolvedPredicateStatisticsMigration = newSqlMigration(TransferContractUnresolvedPredicateStatisticsSql)

// The version a database reaches once the statistics migration has applied: its
// position in the migration list plus one. Monitors and tests read the version
// through this and never name it by number.
func TransferContractUnresolvedPredicateStatisticsVersion() int {
	for index, migration := range migrations {
		if migration == transferContractUnresolvedPredicateStatisticsMigration {
			return index + 1
		}
	}
	return 0
}
