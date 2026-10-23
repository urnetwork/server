// The contract tables become partitioned parents at the zero-copy cutover
// (server.MigrateContractsToPartitions, server/CONTRACT-PARTITION.md). Their
// published artifacts are then read through the parent: pg_get_indexdef
// renders each parent index ON ONLY, PostgreSQL clones a foreign key into one
// constraint and trigger pair per referenced partition, storage settings live
// on the leaf partitions, and the cascade foreign key from
// contract_close_report is dropped by owner decision. These expressions accept
// exactly that layout, for these six tables only, without admitting a missing,
// invalid or disabled parent-level artifact.
package monitor

import (
	"fmt"
	"slices"
	"strings"

	"github.com/urnetwork/server"
)

// ('transfer_contract', ..., 'contract_extender')
var migrationContractTablesSql = func() string {
	quoted := make([]string, 0, len(server.ContractPartitionTables))
	for _, table := range server.ContractPartitionTables {
		quoted = append(quoted, "'"+table+"'")
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}()

// The cutover swaps all six tables in one transaction, so transfer_contract's
// kind is the layout of every contract table.
const migrationContractPartitionedSql = `COALESCE((SELECT relkind = 'p' FROM pg_class WHERE oid = to_regclass('public.transfer_contract')), false)`

func isMigrationContractTable(table string) bool {
	return slices.Contains(server.ContractPartitionTables, table)
}

// pg_get_indexdef with its whitespace collapsed, for the index whose pg_class
// row is indexRelation on the table named by the SQL expression tableName. A
// partitioned index of a contract table drops the ONLY of its table clause, so
// it compares equal to the published definition; its validity still requires
// an attached valid index on every partition. Other partitioned tables keep
// ONLY, which their own contracts pin.
func migrationIndexDefinitionSql(indexRelation string, tableName string) string {
	definition := fmt.Sprintf(`regexp_replace(pg_get_indexdef(%s.oid), '[[:space:]]+', ' ', 'g')`, indexRelation)
	return fmt.Sprintf(
		`CASE WHEN %[1]s.relkind = 'I' AND %[2]s IN %[3]s
		       THEN regexp_replace(%[4]s, '^(CREATE (UNIQUE )?INDEX \S+ ON )ONLY ', '\1')
		       ELSE %[4]s END`,
		indexRelation,
		tableName,
		migrationContractTablesSql,
		definition,
	)
}

// v635 bounds the open column's statistics target and analyzes on a fixed
// threshold. A partitioned parent holds no storage settings and no rows, so
// the target is pinned on the parent, which ANALYZE samples across partitions,
// and both settings on every leaf, which autovacuum analyzes.
const transferContractPartitionedOpenStatisticsArtifactQuery = `EXISTS (
		           SELECT 1
		           FROM pg_attribute attribute_record
		           JOIN pg_class relation ON relation.oid = attribute_record.attrelid
		           JOIN pg_namespace namespace ON namespace.oid = relation.relnamespace
		           WHERE namespace.nspname = 'public'
		             AND relation.relname = 'transfer_contract'
		             AND relation.relkind = 'p'
		             AND attribute_record.attname = 'open'
		             AND attribute_record.attstattarget = 300
		             AND NOT EXISTS (
		                 SELECT 1
		                 FROM pg_partition_tree(relation.oid) tree
		                 JOIN pg_class leaf ON leaf.oid = tree.relid
		                 LEFT JOIN pg_attribute leaf_attribute
		                   ON leaf_attribute.attrelid = leaf.oid
		                   AND leaf_attribute.attname = 'open'
		                   AND NOT leaf_attribute.attisdropped
		                 WHERE tree.isleaf
		                   AND NOT (
		                       coalesce(leaf_attribute.attstattarget = 300, false)
		                       AND coalesce(leaf.reloptions, ARRAY[]::text[]) @> ARRAY[
		                           'autovacuum_analyze_scale_factor=0',
		                           'autovacuum_analyze_threshold=1000000'
		                       ]::text[]
		                   )
		             )
		       )`

// A partitioned contract table is durable when every leaf partition is an
// ordinary permanent table; the parent itself holds no rows.
func migrationContractTable(table string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM pg_class relation WHERE relation.oid=to_regclass('public.%s')
		AND relation.relpersistence='p' AND (relation.relkind='r' OR (relation.relkind='p' AND NOT EXISTS (
			SELECT 1 FROM pg_partition_tree(relation.oid) tree JOIN pg_class leaf ON leaf.oid=tree.relid
			WHERE tree.relid<>relation.oid AND (leaf.relkind<>'r' OR leaf.relpersistence<>'p')))))`, table)
}

// The parent's guard covers a partitioned contract table only through its
// leaves: PostgreSQL clones a row trigger onto every partition (tgparentid),
// and the cutover repeats each TRUNCATE guard on every leaf, since a statement
// trigger on the parent does not fire for a statement that names a partition.
// Every leaf must carry each named trigger of the parent, with its function,
// events and enabled state, so a guard disabled or missing on one week's
// partition is drift. A plain table has no leaves to check.
func migrationContractLeafTriggers(table string, triggers ...string) string {
	names := make([]string, 0, len(triggers))
	for _, trigger := range triggers {
		names = append(names, snMainnetMigrationLiteral(trigger))
	}
	return fmt.Sprintf(`NOT EXISTS(SELECT 1 FROM pg_trigger parent_trigger, pg_partition_tree(parent_trigger.tgrelid) tree
 WHERE parent_trigger.tgrelid=to_regclass('public.%s') AND parent_trigger.tgname IN (%s)
 AND NOT parent_trigger.tgisinternal AND tree.isleaf
 AND NOT EXISTS(SELECT 1 FROM pg_trigger leaf_trigger
 WHERE leaf_trigger.tgrelid=tree.relid AND leaf_trigger.tgname=parent_trigger.tgname
 AND leaf_trigger.tgparentid IN (0, parent_trigger.oid) AND NOT leaf_trigger.tgisinternal
 AND leaf_trigger.tgfoid=parent_trigger.tgfoid AND leaf_trigger.tgtype=parent_trigger.tgtype
 AND leaf_trigger.tgenabled=parent_trigger.tgenabled AND leaf_trigger.tgqual IS NULL
 AND leaf_trigger.tgnargs=parent_trigger.tgnargs))`, table, strings.Join(names, ","))
}

// A foreign key into a partitioned table is cloned into one child constraint
// per referenced partition, each with its own action triggers. Every leaf of
// the referenced table must carry an enabled immediate clone of the published
// key, so no partition's rows can leave without the check. A plain referenced
// table has no leaves to check. Each action is a (function, tgtype) row.
func migrationForeignKeyPartitionClones(table, referenced, definition string, actions ...string) string {
	return fmt.Sprintf(`NOT EXISTS(SELECT 1 FROM pg_partition_tree(to_regclass('public.%[2]s')) tree
 CROSS JOIN (VALUES %[4]s) AS expected(function_name,trigger_type)
 WHERE tree.isleaf AND NOT EXISTS(SELECT 1 FROM pg_constraint parent_key
 JOIN pg_constraint k ON k.conparentid=parent_key.oid
 JOIN pg_trigger t ON t.tgconstraint=k.oid
 JOIN pg_proc p ON p.oid=t.tgfoid JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE parent_key.conrelid=to_regclass('public.%[1]s') AND parent_key.contype='f' AND parent_key.conparentid=0
 AND pg_get_constraintdef(parent_key.oid)=%[3]s
 AND k.conrelid=parent_key.conrelid AND k.confrelid=tree.relid AND k.convalidated
 AND NOT k.condeferrable AND NOT k.condeferred
 AND t.tgrelid=tree.relid AND t.tgisinternal
 AND t.tgenabled IN ('O','A') AND t.tgtype=expected.trigger_type AND t.tgqual IS NULL
 AND NOT t.tgdeferrable AND NOT t.tginitdeferred AND t.tgnargs=0
 AND n.nspname='pg_catalog' AND p.proname=expected.function_name))`,
		table, referenced, snMainnetMigrationLiteral(definition), strings.Join(actions, ","))
}
