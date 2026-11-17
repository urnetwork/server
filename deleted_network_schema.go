package server

// The record that an account was deleted (model/deleted_network_model.go) and
// the App Store subscriptions moved from a deleted account to the account its
// owner restores them from (model/apple_subscription_move_model.go; owner
// 2026-11-17, "Restore moves it."): two tables and then the backfill of the
// accounts deleted before the first (DeletedNetworkBackfillSql), three
// migrations in db_migrations.go. The texts are those migrations' exact DDL,
// named so the catalog monitor follows each to its version wherever merges
// place it.
const deletedNetworkSchemaSql = `
		CREATE TABLE deleted_network (
			network_id uuid NOT NULL,
			delete_time timestamp NOT NULL,
			source varchar(32) NOT NULL,

			PRIMARY KEY (network_id)
		)
	`

const appleSubscriptionMoveSchemaSql = `
		CREATE TABLE apple_subscription_move (
			original_transaction_id varchar(128) NOT NULL,
			from_network_id uuid NOT NULL,
			to_network_id uuid NOT NULL,
			transaction_id varchar(128) NOT NULL,
			move_time timestamp NOT NULL,

			PRIMARY KEY (original_transaction_id)
		)
	`

// DeletedNetworkSchemaMigrationVersion is the version that creates deleted_network.
func DeletedNetworkSchemaMigrationVersion() int {
	return sqlMigrationVersion(deletedNetworkSchemaSql)
}

// AppleSubscriptionMoveSchemaMigrationVersion is the version that creates
// apple_subscription_move.
func AppleSubscriptionMoveSchemaMigrationVersion() int {
	return sqlMigrationVersion(appleSubscriptionMoveSchemaSql)
}

// DeletedNetworkBackfillMigrationVersion is the version that backfills
// deleted_network with the accounts deleted before it existed.
func DeletedNetworkBackfillMigrationVersion() int {
	return sqlMigrationVersion(DeletedNetworkBackfillSql)
}
