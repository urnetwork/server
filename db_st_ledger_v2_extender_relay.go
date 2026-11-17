// The relay chunks of an epoch's extender component (sn WHITEPAPER.md §8.6.2;
// owner 2026-11-16: half of the extender pool is split by relay bytes; model
// st_ledger_v2_model.go): st_ledger_v2_epoch_rows.extender_relay_chunks, the
// references of the relay row chunks the component was first built with,
// fixed with its census and provider rows. NULL for an epoch the amendment does
// not cover, and for a component recorded before the column, which was built
// with no relay rows.
//
// The migration adds the nullable column only when absent, under a five-second
// lock bound, so a failed lock wait is safe to retry. Nothing existing is
// rewritten, and old binaries never read it: they insert their epoch rows
// without it, which leaves it null. Apply before deploying the taskworker and
// api that read it.
package server

// The migration's statement.
const StLedgerV2ExtenderRelaySchemaSql = `
	DO $st_ledger_v2_extender_relay$
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		ALTER TABLE st_ledger_v2_epoch_rows
			ADD COLUMN IF NOT EXISTS extender_relay_chunks text[] NULL;
	END
	$st_ledger_v2_extender_relay$
`

var stLedgerV2ExtenderRelayMigration = newSqlMigration(StLedgerV2ExtenderRelaySchemaSql)

// The version that adds the relay chunks, wherever merges place it.
func StLedgerV2ExtenderRelayMigrationVersion() int {
	return migrationVersion(stLedgerV2ExtenderRelayMigration)
}
