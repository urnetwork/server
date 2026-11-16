// The device day rollup (server audit_device_day_schema.go,
// model/audit_device_day.go) belongs to the migration catalog.
package monitor

import "github.com/urnetwork/server"

// Follow the migration's position after merges instead of pinning a number.
var auditDeviceDayMigrationContract = migrationCurrentSuffix(
	server.AuditDeviceDayVersion(),
	"device day rollup",
	migrationFp2Table("audit_device_day"),
	snMainnetMigrationColumns("audit_device_day",
		snMainnetMigrationColumn{name: "day", kind: "date", notNull: true},
		snMainnetMigrationColumn{name: "device_count", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "complete", kind: "boolean", notNull: true},
		snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("audit_device_day", "PRIMARY KEY (day)", "CHECK ((device_count >= 0))"))
