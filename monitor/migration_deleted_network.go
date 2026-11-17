// The record of deleted accounts and the App Store subscriptions moved from
// them (server deleted_network_schema.go; model/deleted_network_model.go,
// model/apple_subscription_move_model.go; owner 2026-11-17, "Restore moves
// it."): two durable ordinary tables, then the backfill of the accounts
// deleted before the first, three migrations with a contract each.
package monitor

import "github.com/urnetwork/server"

// One durable ordinary table keyed by the network, with three NOT NULL
// columns and no defaults. Follow the migration's position after merges
// instead of pinning a number.
var deletedNetworkMigrationContract = migrationCurrentSuffix(
	server.DeletedNetworkSchemaMigrationVersion(),
	"record of deleted accounts",
	migrationFp2Table("deleted_network"),
	snMainnetMigrationColumns("deleted_network",
		snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "delete_time", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "source", kind: "character varying(32)", notNull: true}),
	snMainnetMigrationConstraints("deleted_network", "PRIMARY KEY (network_id)"))

// One durable ordinary table keyed by the original transaction, with five
// NOT NULL columns and no defaults.
var appleSubscriptionMoveMigrationContract = migrationCurrentSuffix(
	server.AppleSubscriptionMoveSchemaMigrationVersion(),
	"App Store subscriptions moved from deleted accounts",
	migrationFp2Table("apple_subscription_move"),
	snMainnetMigrationColumns("apple_subscription_move",
		snMainnetMigrationColumn{name: "original_transaction_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "from_network_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "to_network_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "transaction_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "move_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("apple_subscription_move", "PRIMARY KEY (original_transaction_id)"))

// The backfill only inserts rows: it publishes no schema of its own, and as
// with every data step in this catalog its rows are not monitored. What it
// wrote into is the published artifact: deleted_network, durable, keyed by
// the network, with the source column the backfill sets.
var deletedNetworkBackfillMigrationContract = migrationCurrentSuffix(
	server.DeletedNetworkBackfillMigrationVersion(),
	"accounts deleted before their record, backfilled",
	migrationFp2Table("deleted_network"),
	snMainnetMigrationColumns("deleted_network",
		snMainnetMigrationColumn{name: "source", kind: "character varying(32)", notNull: true}),
	snMainnetMigrationConstraints("deleted_network", "PRIMARY KEY (network_id)"))
