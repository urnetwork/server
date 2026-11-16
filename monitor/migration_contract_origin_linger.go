// The lingering origins' side table (server contract_origin_linger_schema.go;
// OPEN-SOURCE-ID-RETIRE.md §2) belongs to the migration catalog: the side
// table with its key, prune time index and storage options, the one-row state
// table, and the capture function's exact body. The trigger on
// transfer_contract is the operator's on Main, created by hand after this
// migration, so from this version a trigger of that name is either absent or
// exactly the published one: any other trigger under the name, a disabled one,
// or one that runs another function, is drift. The index's drop record
// requires it present.
package monitor

import (
	"fmt"

	"github.com/urnetwork/server"
)

// The trigger as pg_get_triggerdef renders the published definition, in
// either contract table layout (a partitioned parent renders the same).
const contractOriginLingerTriggerDefinition = `CREATE TRIGGER transfer_contract_origin_linger AFTER INSERT OR UPDATE OF outcome, dispute, close_time, expiration_time ON public.transfer_contract FOR EACH ROW WHEN (((new.dispute OR (new.outcome IS NOT NULL)) AND (new.close_time IS NOT NULL) AND (COALESCE(new.expiration_time, (new.create_time + '01:00:00'::interval)) > LEAST(new.close_time, (clock_timestamp() AT TIME ZONE 'UTC'::text))))) EXECUTE FUNCTION contract_origin_linger_capture()`

// The published trigger: enabled, row level, after insert or the update of
// its four columns, on the published function, with the published WHEN.
var contractOriginLingerTriggerPresent = fmt.Sprintf(`EXISTS(SELECT 1 FROM pg_trigger t
 WHERE t.tgrelid=to_regclass('public.transfer_contract') AND t.tgname=%s AND NOT t.tgisinternal
 AND t.tgenabled IN ('O','A') AND t.tgfoid=to_regprocedure('public.contract_origin_linger_capture()')
 AND NOT t.tgdeferrable AND NOT t.tginitdeferred AND t.tgnargs=0
 AND pg_get_triggerdef(t.oid)=%s)`,
	snMainnetMigrationLiteral(server.ContractOriginLingerTriggerName),
	snMainnetMigrationLiteral(contractOriginLingerTriggerDefinition))

// No trigger of the published name: the operator has not created it yet.
var contractOriginLingerTriggerAbsent = fmt.Sprintf(`NOT EXISTS(SELECT 1 FROM pg_trigger t
 WHERE t.tgrelid=to_regclass('public.transfer_contract') AND t.tgname=%s AND NOT t.tgisinternal)`,
	snMainnetMigrationLiteral(server.ContractOriginLingerTriggerName))

// The side table's storage options: vacuum never truncates it, since the
// truncation's exclusive lock would stall every close behind the trigger's
// insert, and autovacuum keeps up with its churn.
const contractOriginLingerStorageArtifactQuery = `EXISTS (SELECT 1 FROM pg_class relation
		WHERE relation.oid = to_regclass('public.contract_origin_linger')
		AND coalesce(relation.reloptions, ARRAY[]::text[]) @> ARRAY['vacuum_truncate=false','autovacuum_vacuum_scale_factor=0.01','autovacuum_vacuum_cost_delay=0','autovacuum_analyze_scale_factor=0.02']::text[])`

var contractOriginLingerMigrationContract = migrationCurrentSuffix(
	server.ContractOriginLingerMigrationVersion(),
	"lingering origins' side table, state and capture",
	migrationFp2Table("contract_origin_linger"),
	snMainnetMigrationColumns("contract_origin_linger",
		snMainnetMigrationColumn{name: "source_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "destination_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "contract_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "prune_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("contract_origin_linger", "PRIMARY KEY (source_id, destination_id, contract_id)"),
	financialIndexArtifact("contract_origin_linger", "contract_origin_linger_prune_time", "prune_time"),
	contractOriginLingerStorageArtifactQuery,
	migrationFp2Table("contract_origin_linger_state"),
	snMainnetMigrationColumns("contract_origin_linger_state",
		snMainnetMigrationColumn{name: "singleton", kind: "boolean", notNull: true, defaultExpression: "true"},
		snMainnetMigrationColumn{name: "complete_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("contract_origin_linger_state", "PRIMARY KEY (singleton)", "CHECK (singleton)"),
	snMainnetMigrationFunction("contract_origin_linger_capture()", "trigger", "plpgsql", "v", false, server.ContractOriginLingerCaptureBodySql()),
	"("+contractOriginLingerTriggerAbsent+" OR "+contractOriginLingerTriggerPresent+")",
)
