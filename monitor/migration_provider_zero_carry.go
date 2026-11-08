// The zero-carry verdict table (server db_provider_zero_carry.go,
// model/provider_zero_carry.go) belongs to the migration catalog.
package monitor

import "github.com/urnetwork/server"

// Follow the migration's position after merges instead of pinning a number.
var providerZeroCarryMigrationContract = migrationCurrentSuffix(
	server.ProviderZeroCarryVersion(),
	"provider zero-carry verdicts",
	migrationFp2Table("provider_zero_carry"),
	snMainnetMigrationColumns("provider_zero_carry",
		snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "create_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "expire_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "client_version", kind: "character varying(64)"},
		snMainnetMigrationColumn{name: "contract_count", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "network_count", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "carried_byte_count", kind: "bigint", notNull: true}),
	snMainnetMigrationConstraints("provider_zero_carry", "PRIMARY KEY (client_id)"))
