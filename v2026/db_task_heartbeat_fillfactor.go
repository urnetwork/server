package server

// Half of each pending_task page stays free for the HOT versions that heartbeats
// write between lease extensions (task.refreshTaskTimestampLeasesInTx). The
// change takes SHARE UPDATE EXCLUSIVE; the lock_timeout keeps it from queueing
// behind a vacuum or an index build.
const taskHeartbeatFillfactorSql = `
	SET LOCAL lock_timeout = '5s';
	ALTER TABLE pending_task SET (fillfactor = 50);
`

// The version of the migration that applies taskHeartbeatFillfactorSql in this
// binary, found by its SQL, so the monitor's contract follows the migration to
// whatever position a merge gives it. Zero when the binary lacks it.
func TaskHeartbeatFillfactorMigrationVersion() int {
	for index, migration := range migrations {
		if sql, ok := migration.(*SqlMigration); ok && sql.sql == taskHeartbeatFillfactorSql {
			return index + 1
		}
	}
	return 0
}
