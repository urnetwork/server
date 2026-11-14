package server

// The demand deposit's durable state (controller/st_demand.go, sn/demand): the
// one row the conversion of USDC into alpha keeps between runs, the steps it took
// for accounting, the last price sheet whose signatures verified, and each settled
// epoch's usage statement. New tables: nothing is rewritten, and older binaries
// never read them. Apply before deploying the taskworker that writes them: under
// settlement v2 the usage statement is written at zero price too, because the first
// priced epoch's deposit prices the last zero-price epoch's usage.
const stDemandSchemaSql = `
	CREATE TABLE st_demand_state (
		deployment_key varchar(96) NOT NULL,
		version bigint NOT NULL,
		state jsonb NOT NULL,
		update_time timestamp NOT NULL,

		PRIMARY KEY (deployment_key)
	);

	CREATE TABLE st_demand_event (
		event_id uuid NOT NULL,
		deployment_key varchar(96) NOT NULL,
		event_time timestamp NOT NULL,
		epoch bigint NOT NULL,
		kind varchar(32) NOT NULL,
		data jsonb NOT NULL,

		PRIMARY KEY (event_id)
	);

	CREATE INDEX st_demand_event_deployment_time
		ON st_demand_event (deployment_key, event_time);

	-- The last published price sheet whose every entry verified under the
	-- deployment's pinned signer, exactly as it was published.
	CREATE TABLE st_demand_price_sheet (
		deployment_key varchar(96) NOT NULL,
		raw bytea NOT NULL,
		update_time timestamp NOT NULL,

		PRIMARY KEY (deployment_key)
	);

	-- A settlement v2 epoch's usage statement (sn/demand/usage). total_users is
	-- captured once as soon as the epoch's boundary is finalized and never
	-- counted again: a client active after the close would raise a later count,
	-- and a changed amount is refused against the deposit's retained calldata.
	-- The rest is written once the epoch's root commit is mirrored, from the
	-- provider chunks of the header it committed.
	CREATE TABLE st_ledger_v2_epoch_usage (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		total_users bigint NOT NULL CHECK (total_users >= 0),
		users_time timestamp NOT NULL,
		total_usage_bytes numeric(20,0) NULL,
		provider_rows bigint NULL,
		content_hash varchar(80) NULL,
		statement bytea NULL,
		statement_time timestamp NULL,

		PRIMARY KEY (deployment_key, no_id, epoch)
	);
`

// The version of the migration that creates the tables in this binary, found by
// its SQL, so the monitor's contract follows the migration to whatever position a
// merge gives it. Zero when the binary lacks it.
func StDemandSchemaMigrationVersion() int {
	return sqlMigrationVersion(stDemandSchemaSql)
}
