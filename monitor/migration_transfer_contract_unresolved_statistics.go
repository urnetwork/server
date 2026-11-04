// CPUOPT4 Part D #1's published expression statistics are schema knowledge,
// not proof of an ANALYZE sample, selectivity estimate or chosen query plan.
package monitor

import "github.com/urnetwork/server"

// Pin the public expression-only object on the exact unresolved CASE. Reading
// its definition needs neither private statistic data nor a populated sample.
// Whitespace alone is normalized, as in the existing index-definition probes.
const transferContractUnresolvedStatisticsArtifactQuery = `EXISTS (
 SELECT 1 FROM pg_catalog.pg_statistic_ext statistic
 JOIN pg_catalog.pg_namespace namespace ON namespace.oid=statistic.stxnamespace
 WHERE namespace.nspname='public' AND statistic.stxname='transfer_contract_unresolved_predicate'
 AND statistic.stxrelid=to_regclass('public.transfer_contract')
 AND statistic.stxkind=ARRAY['e']::"char"[] AND statistic.stxkeys=''::int2vector
 AND btrim(regexp_replace(pg_catalog.pg_get_expr(statistic.stxexprs,statistic.stxrelid,false),'\s+',' ','g'))=
  'CASE WHEN (outcome IS NULL) THEN (dispute = false) ELSE false END'
)`

// This explicit predecessor contract closes the catalog gap at the already
// published statistics migration; no new DDL or historical identity is made.
var transferContractUnresolvedStatisticsMigrationContract = migrationCurrentSuffix(server.TransferContractUnresolvedPredicateStatisticsVersion(),
	"unresolved contract predicate expression statistics", transferContractUnresolvedStatisticsArtifactQuery)
