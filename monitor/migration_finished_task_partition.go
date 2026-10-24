// finished_task's daily partitions (server/PARTITION-MAINTENANCE-PLAN.md C3)
// rest on three migrations of the plain table: a unique (task_id,
// run_end_time) index built online, the primary key moved onto that index,
// and the failed Post index built online. The operator cutover
// (server.MigrateFinishedTasksToPartitions) makes the table partitioned later,
// so each contract holds alike on the plain table, through a prepared cutover
// (whose staged parent is another table) and on the partitioned parent, where
// pg_get_indexdef reads ON ONLY. The contracts read the catalogs directly,
// never the coherence query's own expressions, so each reads false rather
// than failing before its migration runs. Their versions come from the
// server's migration list, so they follow their migrations wherever the list
// places them.
package monitor

import (
	"github.com/urnetwork/server"
)

// The unique index exists only between its migration and the next, which
// moves the primary key onto it and so renames it to finished_task_pkey.
const finishedTaskIdentityIndexArtifactQuery = `EXISTS (SELECT 1 FROM pg_index i
 WHERE i.indrelid=to_regclass('public.finished_task')
 AND i.indexrelid=to_regclass('public.finished_task_task_id_run_end_time')
 AND i.indisunique AND i.indisvalid AND i.indisready AND i.indpred IS NULL
 AND regexp_replace(pg_get_indexdef(i.indexrelid),'[[:space:]]+',' ','g')='CREATE UNIQUE INDEX finished_task_task_id_run_end_time ON public.finished_task USING btree (task_id, run_end_time)')`

// The primary key by its constraint and columns, with its index valid and
// ready; the partition key run_end_time is part of it in every layout. The
// index's name follows the constraint, so it is not pinned.
var finishedTaskPrimaryKeyArtifactQuery = snMainnetMigrationConstraints("finished_task", "PRIMARY KEY (task_id, run_end_time)")

// The failed Post index on the plain table, or on the partitioned parent as
// a partitioned index, which is valid only once every partition has its own
// attached valid index.
const finishedTaskPostRetryIndexArtifactQuery = `EXISTS (SELECT 1 FROM pg_index i
 WHERE i.indrelid=to_regclass('public.finished_task')
 AND i.indexrelid=to_regclass('public.finished_task_post_retry')
 AND NOT i.indisunique AND i.indisvalid AND i.indisready
 AND regexp_replace(regexp_replace(pg_get_indexdef(i.indexrelid),'[[:space:]]+',' ','g'),'^(CREATE INDEX \S+ ON )ONLY ','\1')='CREATE INDEX finished_task_post_retry ON public.finished_task USING btree (task_id) WHERE ((post_error IS NOT NULL) AND (NOT post_completed))')`

func finishedTaskPartitionContracts() []snMainnetMigrationContract {
	primaryKeyVersion := server.FinishedTaskPrimaryKeyMigrationVersion()
	return []snMainnetMigrationContract{
		migrationCurrentSuffix(server.FinishedTaskIdentityIndexMigrationVersion(), "finished_task unique task id and run end time index",
			finishedTaskIdentityIndexArtifactQuery).removedAt(primaryKeyVersion),
		migrationCurrentSuffix(primaryKeyVersion, "finished_task primary key with its run end time partition key",
			finishedTaskPrimaryKeyArtifactQuery),
		migrationCurrentSuffix(server.FinishedTaskPostRetryIndexMigrationVersion(), "finished_task failed Post index",
			finishedTaskPostRetryIndexArtifactQuery),
	}
}
