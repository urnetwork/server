// lag.dev's measurement api probes' migration (server
// network_extender_measurement_probe_schema.go): the latest probe of each
// published family of each extender the lag.dev sample serves, which the
// sample reads to leave out an extender its probes cannot reach.
package monitor

import "github.com/urnetwork/server"

// A durable ordinary table keyed by extender and family. The failures in a
// row default to 0 and the latest success is nullable; the migration publishes
// no check and no index except the key's, which the constraint requires valid
// and ready.
var extenderMeasurementProbeMigrationContract = migrationCurrentSuffix(server.NetworkExtenderMeasurementProbeSchemaMigrationVersion(), "extender measurement api probes",
	migrationFp2Table("network_extender_measurement_probe"),
	snMainnetMigrationColumns("network_extender_measurement_probe",
		snMainnetMigrationColumn{name: "extender_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "ip_version", kind: "smallint", notNull: true},
		snMainnetMigrationColumn{name: "ip", kind: "inet", notNull: true},
		snMainnetMigrationColumn{name: "probe_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "outcome", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "success_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "consecutive_failures", kind: "integer", notNull: true, defaultExpression: "0"}),
	snMainnetMigrationConstraints("network_extender_measurement_probe", "PRIMARY KEY (extender_id, ip_version)"))
