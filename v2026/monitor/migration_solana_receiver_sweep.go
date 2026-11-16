// The Solana receiver sweep's cursor (server solana_receiver_sweep_schema.go,
// controller/solana_receiver_sweep.go) belongs to the migration catalog.
package monitor

import "github.com/urnetwork/server/v2026"

// Follow the migration's position after merges instead of pinning a number.
var solanaReceiverSweepCursorMigrationContract = migrationCurrentSuffix(
	server.SolanaReceiverSweepCursorVersion(),
	"Solana receiver sweep cursor",
	migrationFp2Table("solana_receiver_sweep_cursor"),
	snMainnetMigrationColumns("solana_receiver_sweep_cursor",
		snMainnetMigrationColumn{name: "token_account", kind: "character varying(64)", notNull: true},
		snMainnetMigrationColumn{name: "receiver", kind: "character varying(64)", notNull: true},
		snMainnetMigrationColumn{name: "signature", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "slot", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "block_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("solana_receiver_sweep_cursor", "PRIMARY KEY (token_account)"))
