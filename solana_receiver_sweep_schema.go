// The cursor of the Solana receiver sweep (controller/solana_receiver_sweep.go,
// model/solana_receiver_sweep_model.go): per USDC token account of our Solana
// Pay receivers, the newest transaction the payment reconcile's receiver leg
// has examined. The leg reads each token account's history forward from it,
// so a USDC payment whose Helius delivery was lost after its intent was swept
// is still found, once, and recorded for an operator.
//
// The migration only creates the table, empty: no existing table is locked or
// rewritten. The five-second lock bound keeps a catalog wait from stalling
// behind a long reader, and a table that exists already is left as it is, so
// a failed lock wait is safe to retry. Old binaries never read it. Apply
// before deploying the taskworker that runs the reconcile.
package server

// The migration's statement.
const SolanaReceiverSweepCursorSql = `
	DO $receiver_sweep$
	BEGIN
		IF to_regclass('public.solana_receiver_sweep_cursor') IS NULL THEN
			PERFORM set_config('lock_timeout', '5s', true);
			CREATE TABLE solana_receiver_sweep_cursor (
				token_account varchar(64) NOT NULL,
				receiver varchar(64) NOT NULL,
				signature varchar(128) NOT NULL,
				slot bigint NOT NULL,
				block_time timestamp NULL,
				update_time timestamp NOT NULL,
				PRIMARY KEY (token_account)
			);
		END IF;
	END
	$receiver_sweep$
`

var solanaReceiverSweepCursorMigration = newSqlMigration(SolanaReceiverSweepCursorSql)

// The migration's version: its position in the list, from 1.
func SolanaReceiverSweepCursorVersion() int {
	for i, migration := range migrations {
		if migration == solanaReceiverSweepCursorMigration {
			return i + 1
		}
	}
	return 0
}
