// The SN25 extender pool's durable state (sn WHITEPAPER.md §8.6; model
// st_extender_census.go and st_ledger_v2_model.go, controller
// st_ledger_v2_amendments.go):
//   - st_amendments: the last published amendments file
//     (https://ur.xyz/amendments.yml) whose every entry verified under the
//     deployment's pinned signer, exactly as it was published, as
//     st_demand_price_sheet keeps the price sheet;
//   - st_ledger_v2_extender_census: each epoch's extender census, captured
//     once as soon as its end boundary is finalized, as its users are;
//   - st_ledger_v2_extender_shadow: the shadow report of each epoch the
//     amendment does not cover yet (§8.6.7: epochs 6 and 7), what the extender
//     pool would have paid, recorded once for review and never paid;
//   - st_platform_endpoint: the operator's registered platform endpoints
//     (§8.6.2), each registration kept with the time it began and, once
//     revoked, ended, so a contract's class follows its create time;
//   - st_ledger_v2_epoch_rows.extender_bytes, extender_census_chunks,
//     extender_intra_platform_bytes, extender_intra_network_bytes,
//     extender_contracts and extender_payers: an epoch's extender component as
//     it was first built, Z, its census chunks, Z's parts and the contracts and
//     payer networks behind it, fixed with its provider rows. All are NULL for
//     an epoch the amendment does not cover, and all are set for one it covers.
//
// The migration creates the tables only when absent and adds the nullable
// columns only when absent, under a five-second lock bound, so a failed lock
// wait is safe to retry. Nothing existing is rewritten, and old binaries never
// read them. Apply before deploying the taskworker and api that read them.
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
		IF to_regclass('public.st_ledger_v2_extender_shadow') IS NULL THEN
			CREATE TABLE st_ledger_v2_extender_shadow (
				deployment_key varchar(96) NOT NULL,
				no_id bigint NOT NULL CHECK (no_id > 0),
				epoch bigint NOT NULL CHECK (epoch > 0),
				report_ref varchar(71) NOT NULL,
				report bytea NOT NULL,
				create_time timestamp NOT NULL,

				PRIMARY KEY (deployment_key, no_id, epoch)
			);
		END IF;
		IF to_regclass('public.st_platform_endpoint') IS NULL THEN
			CREATE TABLE st_platform_endpoint (
				client_id uuid NOT NULL,
				network_id uuid NOT NULL,
				kind varchar(16) NOT NULL CHECK (kind IN ('tlsedge', 'smtpedge', 'mxedge')),
				register_time timestamp NOT NULL,
				registered_by varchar(256) NOT NULL CHECK (registered_by <> ''),
				revoke_time timestamp NULL,
				revoked_by varchar(256) NULL,

				PRIMARY KEY (client_id, register_time),
				CHECK (revoke_time IS NULL OR register_time < revoke_time),
				CHECK ((revoke_time IS NULL) = (revoked_by IS NULL))
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
			ADD COLUMN IF NOT EXISTS extender_census_chunks text[] NULL,
			ADD COLUMN IF NOT EXISTS extender_intra_platform_bytes numeric(20,0) NULL,
			ADD COLUMN IF NOT EXISTS extender_intra_network_bytes numeric(20,0) NULL,
			ADD COLUMN IF NOT EXISTS extender_contracts bigint NULL,
			ADD COLUMN IF NOT EXISTS extender_payers bigint NULL;
	END
	$st_ledger_v2_extenders$
`

var stLedgerV2ExtenderMigration = newSqlMigration(StLedgerV2ExtenderSchemaSql)

// The version that creates the extender pool's state, wherever merges place
// it.
func StLedgerV2ExtenderMigrationVersion() int {
	return migrationVersion(stLedgerV2ExtenderMigration)
}
