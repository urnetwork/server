// Per-cycle Pro grants (PRO-GRANT-CYCLE.md, model/pro_grant_cycle_model.go).
//
// subscription_renewal.paid_end_time is the end of the period a renewal row's
// payment or trial covers, without the market's grace: the credit paths set
// it, a billing grace hold leaves it NULL, and rows written before it are
// read by their market's grace. pro_grant_cycle holds one row per cycle of a
// network: its end, the Pro grant written for it (NULL when a legacy calendar
// grant already covered the cycle), and ended_time when a refund, revocation
// or reconcile cut ended the cycle early with its paid period (cleared again
// when a won dispute gives the period back). The primary key is the grant's
// idempotency key, network and cycle start; the end-time index serves the
// hourly sweep's scan of the running cycles.
//
// The migration adds a nullable column with no default, which rewrites
// nothing, and one new empty table with its index. The five-second lock
// bound keeps the column's brief catalog lock from stalling behind a long
// reader of subscription_renewal; both statements are skipped when already
// applied, so a failed lock wait is safe to retry. Old binaries neither read
// nor write either. Apply before deploying the api, alt and taskworker that
// write paid_end_time.
package server

// The migration's statement.
const ProGrantCycleSql = `
	DO $pro_grant_cycle$
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		ALTER TABLE subscription_renewal ADD COLUMN IF NOT EXISTS paid_end_time timestamp NULL;
		IF to_regclass('public.pro_grant_cycle') IS NULL THEN
			CREATE TABLE pro_grant_cycle (
				network_id uuid NOT NULL,
				cycle_start_time timestamp NOT NULL,
				cycle_end_time timestamp NOT NULL,
				balance_id uuid NULL,
				ended_time timestamp NULL,
				create_time timestamp NOT NULL DEFAULT now(),
				PRIMARY KEY (network_id, cycle_start_time)
			);
			CREATE INDEX pro_grant_cycle_cycle_end_time ON pro_grant_cycle (cycle_end_time);
		END IF;
	END
	$pro_grant_cycle$
`

var proGrantCycleMigration = newSqlMigration(ProGrantCycleSql)

// The migration's version: its position in the list, from 1.
func ProGrantCycleVersion() int {
	for i, migration := range migrations {
		if migration == proGrantCycleMigration {
			return i + 1
		}
	}
	return 0
}
