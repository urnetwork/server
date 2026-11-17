// The per-cycle Pro grant schema (server db_pro_grant_cycle.go,
// model/pro_grant_cycle_model.go) belongs to the migration catalog.
package monitor

import "github.com/urnetwork/server"

// Follow the migration's position after merges instead of pinning a number.
// Without paid_end_time every credit path fails, and a default would read every
// hold as paid. Without the cycle key one cycle could be granted twice.
var proGrantCycleMigrationContract = migrationCurrentSuffix(
	server.ProGrantCycleVersion(),
	"per-cycle Pro grants",
	snMainnetMigrationColumns("subscription_renewal",
		snMainnetMigrationColumn{name: "paid_end_time", kind: "timestamp without time zone"}),
	migrationFp2Table("pro_grant_cycle"),
	snMainnetMigrationColumns("pro_grant_cycle",
		snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "cycle_start_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "cycle_end_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "balance_id", kind: "uuid"},
		snMainnetMigrationColumn{name: "ended_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true, defaultExpression: "now()"}),
	snMainnetMigrationConstraints("pro_grant_cycle", "PRIMARY KEY (network_id, cycle_start_time)"),
	migrationFp2Index("pro_grant_cycle", "pro_grant_cycle_cycle_end_time", "cycle_end_time", "", false))
