// The operator blocks the daily points award recorded expired (server
// operator_block_points_expired_schema.go, model/operator_block_points.go)
// belong to the migration catalog.
package monitor

import "github.com/urnetwork/server/v2026"

// Follow the migration's position after merges instead of pinning a number.
var operatorBlockPointsExpiredMigrationContract = migrationCurrentSuffix(
	server.OperatorBlockPointsExpiredVersion(),
	"operator block points expired",
	migrationFp2Table("operator_block_points_expired"),
	snMainnetMigrationColumns("operator_block_points_expired",
		snMainnetMigrationColumn{name: "block_number", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "window_start", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "window_end", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "reliability_min_block_number", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "retention_min_block_number", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "expire_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("operator_block_points_expired", "PRIMARY KEY (block_number)"))
