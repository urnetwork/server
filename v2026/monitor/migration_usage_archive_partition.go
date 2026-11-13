// The usage archive partitioned by contract week (server/PARTITION-MAINTENANCE-PLAN.md
// C6). The contract reads catalogs only, and its version comes from the
// server's migration list, so it follows its migration wherever the list
// places it. The guards on every partition stay the v746 contract's
// (providerUsageArchiveArtifactQuery).
package monitor

import (
	"github.com/urnetwork/server/v2026"
)

// From the cutover on, the archive is a RANGE (contract_id) parent with its
// DEFAULT partition, and every partition is an ordinary durable table. No
// archive week stands outside the tree: the registry creates and drops a week
// each in one transaction, so a committed standalone week was detached by hand
// and its rows are hidden from every reader.
const providerUsageArchivePartitionArtifactQuery = `(EXISTS (SELECT 1 FROM pg_partitioned_table partitioned
 JOIN pg_class parent ON parent.oid=partitioned.partrelid
 JOIN pg_attribute key_column ON key_column.attrelid=parent.oid AND key_column.attnum=partitioned.partattrs[0]
 WHERE parent.oid=to_regclass('public.st_provider_usage_archive') AND parent.relkind='p' AND parent.relpersistence='p'
 AND partitioned.partstrat='r' AND partitioned.partnatts=1 AND key_column.attname='contract_id')
 AND EXISTS (SELECT 1 FROM pg_inherits inheritance JOIN pg_class leaf ON leaf.oid=inheritance.inhrelid
 WHERE inheritance.inhparent=to_regclass('public.st_provider_usage_archive')
 AND leaf.relname='st_provider_usage_archive_default' AND pg_get_expr(leaf.relpartbound, leaf.oid)='DEFAULT')
 AND NOT EXISTS (SELECT 1 FROM pg_partition_tree(to_regclass('public.st_provider_usage_archive')) tree
 JOIN pg_class leaf ON leaf.oid=tree.relid
 WHERE tree.relid<>to_regclass('public.st_provider_usage_archive')
 AND (NOT tree.isleaf OR leaf.relkind<>'r' OR leaf.relpersistence<>'p'))
 AND NOT EXISTS (SELECT 1 FROM pg_class stray JOIN pg_namespace namespace ON namespace.oid=stray.relnamespace
 WHERE namespace.nspname='public' AND stray.relname ~ '^st_provider_usage_archive_(w[0-9]{8}|default)$'
 AND stray.relkind IN ('r','p') AND NOT stray.relispartition))`

func providerUsageArchivePartitionContracts() []snMainnetMigrationContract {
	return []snMainnetMigrationContract{
		migrationCurrentSuffix(server.ProviderUsageArchivePartitionMigrationVersion(), "st_provider_usage_archive weekly contract-week partitions",
			providerUsageArchivePartitionArtifactQuery),
	}
}
