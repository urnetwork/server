// The device day rollup (PARTITION-CANDIDATES.md §7.3; owner 2026-11-16):
// one row per UTC day with the day's count of devices, the connected-per-day
// series the public stats export publishes (model/audit_device_day.go). The
// device sweep keeps today's row in its own transaction, and the export reads
// the rows instead of every retained device event, so the raw events need
// only a few days of retention.
//
// The migration only creates the table, empty: no existing table is locked or
// rewritten. The five-second lock bound keeps a catalog wait from stalling
// behind a long reader, and a table that exists already is left as it is, so
// a failed lock wait is safe to retry. Old binaries never read it. Apply
// before deploying the taskworker whose sweep writes it and whose export reads
// it; until then the export keeps computing from the raw events.
package server

// The migration's statement.
const AuditDeviceDaySql = `
	DO $audit_device_day$
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		IF to_regclass('public.audit_device_day') IS NULL THEN
			CREATE TABLE audit_device_day (
				day date NOT NULL,
				device_count bigint NOT NULL CHECK (device_count >= 0),
				complete boolean NOT NULL,
				update_time timestamp NOT NULL,
				PRIMARY KEY (day)
			);
		END IF;
	END
	$audit_device_day$
`

var auditDeviceDayMigration = newSqlMigration(AuditDeviceDaySql)

// The migration's version: its position in the list, from 1.
func AuditDeviceDayVersion() int {
	for i, migration := range migrations {
		if migration == auditDeviceDayMigration {
			return i + 1
		}
	}
	return 0
}
