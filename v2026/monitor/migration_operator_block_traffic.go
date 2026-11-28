// The bytes served per operator block of GET /account/epochs (server
// operator_block_traffic_schema.go, model/operator_block_traffic.go) belong to
// the migration catalog.
package monitor

import "github.com/urnetwork/server/v2026"

// Follow the migration's position after merges instead of pinning a number.
// The rollup moves a block's cursor only from where it read it, and adds a
// network's bytes to its one row of the block: both tables are durable
// ordinary tables keyed as published.
var operatorBlockTrafficMigrationContract = migrationCurrentSuffix(
	server.OperatorBlockTrafficVersion(),
	"operator block traffic rollup",
	migrationFp2Table("operator_block_traffic"),
	snMainnetMigrationColumns("operator_block_traffic",
		snMainnetMigrationColumn{name: "block_number", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "read_through", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("operator_block_traffic", "PRIMARY KEY (block_number)"),
	migrationFp2Table("operator_block_network_traffic"),
	snMainnetMigrationColumns("operator_block_network_traffic",
		snMainnetMigrationColumn{name: "block_number", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "payout_byte_count", kind: "bigint", notNull: true}),
	snMainnetMigrationConstraints("operator_block_network_traffic", "PRIMARY KEY (block_number, network_id)"))
