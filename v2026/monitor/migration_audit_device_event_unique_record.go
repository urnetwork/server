// The record of UNIQUE (event_id)'s drop from audit_device_event (server
// audit_device_event_unique_record.go) belongs to the migration catalog:
// neither the constraint nor a unique index of its name comes back.
package monitor

import "github.com/urnetwork/server/v2026"

var auditDeviceEventUniqueRecordArtifactQuery = `(NOT EXISTS (SELECT 1 FROM constraint_artifact
		WHERE table_name = 'audit_device_event' AND constraint_name = 'audit_device_event_event_id_key') AND ` +
	migrationIndexRemoved("audit_device_event", "audit_device_event_event_id_key") + `)`

// Follow the migration's position after merges instead of pinning a number.
var auditDeviceEventUniqueRecordMigrationContract = migrationCurrentSuffix(
	server.AuditDeviceEventUniqueRecordVersion(),
	"redundant unique event id of the device events removed",
	auditDeviceEventUniqueRecordArtifactQuery)
