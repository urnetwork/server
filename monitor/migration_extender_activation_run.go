// The extender activation run's migration (server
// extender_activation_run_schema.go): which run of the extender process sent
// each activation (connect/EXTENDER.md G3).
package monitor

import "github.com/urnetwork/server"

// Three nullable columns on the activation history with no default, in the
// catalog's own text on PostgreSQL 18. The migration publishes no index or
// check.
var extenderActivationRunMigrationContract = migrationCurrentSuffix(server.ExtenderActivationRunSchemaMigrationVersion(), "extender activation run id, run start and previous exit",
	snMainnetMigrationColumns("network_extender_activation",
		snMainnetMigrationColumn{name: "run_id", kind: "character varying(32)"},
		snMainnetMigrationColumn{name: "run_start_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "previous_exit", kind: "character varying(16)"}))
