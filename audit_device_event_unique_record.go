// The record of UNIQUE (event_id)'s drop from audit_device_event
// (AUDIT-DEVICE-EVENT-PARTITION-RUNBOOK §3; owner 2026-11-16). The owner-run
// prepare of the table's partition cutover dropped the constraint on Main
// (bringyourctl model migrate audit-device-event-partition). Nothing reads
// its index: event ids come from server.NewId(), and no ON CONFLICT targets
// it.
//
// This migration makes every other database match. Main's catalog no longer
// holds the constraint, so the migration only reads pg_constraint there and
// takes no table lock. A database that still has it drops it under a
// five-second lock bound. Merge it only after the prepare has run on Main:
// before then, db migrate would take the table's exclusive lock itself and
// drop the 7.8 GiB index inside the migration.
package server

// The migration's statement.
const AuditDeviceEventUniqueRecordSql = `
	DO $audit_device_event_unique_record$
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		IF EXISTS (
			SELECT 1
			FROM pg_constraint
			WHERE
				conrelid = to_regclass('public.audit_device_event') AND
				conname = 'audit_device_event_event_id_key'
		) THEN
			ALTER TABLE audit_device_event DROP CONSTRAINT audit_device_event_event_id_key;
		END IF;
	END
	$audit_device_event_unique_record$
`

var auditDeviceEventUniqueRecordMigration = newSqlMigration(AuditDeviceEventUniqueRecordSql)

// The migration's version: its position in the list, from 1.
func AuditDeviceEventUniqueRecordVersion() int {
	for i, migration := range migrations {
		if migration == auditDeviceEventUniqueRecordMigration {
			return i + 1
		}
	}
	return 0
}
