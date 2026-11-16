// The extender component's relay chunks migration (server
// db_st_ledger_v2_extender_relay.go, sn WHITEPAPER.md §8.6.2): the relay row
// chunks an epoch's extender component was first built with, beside its census
// chunks on st_ledger_v2_epoch_rows.
package monitor

import "github.com/urnetwork/server"

// One nullable text[] column on st_ledger_v2_epoch_rows, without a default.
// A release from before the column names the columns it writes, so it inserts
// on and leaves the column null, which reads as a component built with no relay
// rows: what such a release built. A default would give an epoch the amendment
// does not cover a component part, and a required column would refuse its rows.
// The migration publishes no index or check. Follow the migration's position
// after merges instead of pinning a number.
var stLedgerV2ExtenderRelayMigrationContract = migrationCurrentSuffix(server.StLedgerV2ExtenderRelayMigrationVersion(), "settlement v2 extender relay chunks",
	snMainnetMigrationColumns("st_ledger_v2_epoch_rows",
		snMainnetMigrationColumn{name: "extender_relay_chunks", kind: "text[]"}))
