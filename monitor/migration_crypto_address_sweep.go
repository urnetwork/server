// The sweep of every address a per-address coin's invoices were ever given
// (server crypto_address_sweep_schema.go, controller/crypto_pay_sweep.go)
// belongs to the migration catalog.
package monitor

import "github.com/urnetwork/server"

// Follow the migration's position after merges instead of pinning a number.
var cryptoAddressSweepMigrationContract = migrationCurrentSuffix(
	server.CryptoAddressSweepVersion(),
	"crypto issued address sweep",
	migrationFp2Table("crypto_address_sweep"),
	snMainnetMigrationColumns("crypto_address_sweep",
		snMainnetMigrationColumn{name: "asset", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "address", kind: "text", notNull: true},
		snMainnetMigrationColumn{name: "height", kind: "bigint"},
		snMainnetMigrationColumn{name: "signature", kind: "character varying(128)"},
		snMainnetMigrationColumn{name: "read_at", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "last_error", kind: "text"},
		snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("crypto_address_sweep", "PRIMARY KEY (asset, address)"),
	migrationFp2Table("crypto_address_sweep_position"),
	snMainnetMigrationColumns("crypto_address_sweep_position",
		snMainnetMigrationColumn{name: "asset", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "after_address_index", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("crypto_address_sweep_position", "PRIMARY KEY (asset)"))
