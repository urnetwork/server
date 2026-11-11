// The SN25 extender pool's durable state (sn WHITEPAPER.md §8.6; model
// st_extender_census.go and st_ledger_v2_model.go, controller
// st_ledger_v2_amendments.go):
//   - st_amendments: the last published amendments file
//     (https://ur.xyz/amendments.yml) whose every entry verified under the
//     deployment's pinned signer, exactly as it was published, as
//     st_demand_price_sheet keeps the price sheet;
//   - st_ledger_v2_extender_census: each epoch's extender census, captured
//     once as soon as its end boundary is finalized, as its users are;
//   - st_ledger_v2_epoch_rows.extender_bytes and extender_census_chunks: an
//     epoch's extender component as it was first built, Z and its census
//     chunks, fixed with its provider rows. Both are NULL for an epoch the
//     amendment does not cover.
//
// The migration creates the two tables only when absent and adds the two
// nullable columns only when absent, under a five-second lock bound, so a
// failed lock wait is safe to retry. Nothing existing is rewritten, and old
// binaries never read them. Apply before deploying the taskworker and api that
// read them.
package server

// The migration's statement.
const StLedgerV2ExtenderSchemaSql = `
	DO $st_ledger_v2_extenders$
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		IF to_regclass('public.st_amendments') IS NULL THEN
			CREATE TABLE st_amendments (
				deployment_key varchar(96) NOT NULL,
				raw bytea NOT NULL,
				update_time timestamp NOT NULL,

				PRIMARY KEY (deployment_key)
			);
		END IF;
		IF to_regclass('public.st_ledger_v2_extender_census') IS NULL THEN
			CREATE TABLE st_ledger_v2_extender_census (
				deployment_key varchar(96) NOT NULL,
				no_id bigint NOT NULL CHECK (no_id > 0),
				epoch bigint NOT NULL CHECK (epoch > 0),
				census bytea NOT NULL,
				census_rows bigint NOT NULL CHECK (census_rows >= 0),
				capture_time timestamp NOT NULL,

				PRIMARY KEY (deployment_key, no_id, epoch)
			);
		END IF;
		ALTER TABLE st_ledger_v2_epoch_rows
			ADD COLUMN IF NOT EXISTS extender_bytes numeric(20,0) NULL,
			ADD COLUMN IF NOT EXISTS extender_census_chunks text[] NULL;
	END
	$st_ledger_v2_extenders$
`

var stLedgerV2ExtenderMigration = newSqlMigration(StLedgerV2ExtenderSchemaSql)
