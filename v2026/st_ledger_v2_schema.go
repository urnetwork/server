package server

// Settlement v2 (VERSION2-FINAL.md §6, §8): the operator server's ledger
// records and its mirror of vault2 state. Every row is scoped by the st
// deployment key and the pool's no_id. Ledger objects themselves live in the
// blob store under their sha256 (startifact/ledger_v2.go). New tables only:
// nothing is rewritten.
const stLedgerV2SchemaSql = `
	-- The provider rows of a covered epoch, recorded once at the first root
	-- that covers it (§6.4 fixed inputs): their chunk references ('{}' when
	-- the epoch has no rows), or the operator's explicit declaration that the
	-- epoch's inputs are unrecoverable (providers: null) and who made it.
	-- rebuild_requested_by names who made the operator's explicit request to
	-- build the epoch's rows again; the rows built for it replace the record.
	CREATE TABLE st_ledger_v2_epoch_rows (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		provider_chunks text[] NULL,
		declared_by varchar(256) NULL,
		rebuild_requested_by varchar(256) NULL,
		create_time timestamp NOT NULL,

		PRIMARY KEY (deployment_key, no_id, epoch),
		CHECK ((provider_chunks IS NULL) <> (declared_by IS NULL))
	);

	-- A pool's account index (§6.1): each coldkey's fixed position, appended
	-- when the first finalized root that holds it is recorded.
	CREATE TABLE st_ledger_v2_account (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		account_index bigint NOT NULL CHECK (account_index >= 0),
		coldkey bytea NOT NULL CHECK (octet_length(coldkey) = 32),
		root_index bigint NOT NULL CHECK (root_index >= 0),
		create_time timestamp NOT NULL,

		PRIMARY KEY (deployment_key, no_id, account_index),
		UNIQUE (deployment_key, no_id, coldkey)
	);
	CREATE INDEX st_ledger_v2_account_coldkey ON st_ledger_v2_account (deployment_key, coldkey);

	-- vault2 funding snapshots: fundedThrough per epoch, from EmissionCaptured
	-- or EmissionDeferred.
	CREATE TABLE st_ledger_v2_funding (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL,
		epoch bigint NOT NULL,
		funded_through numeric(78,0) NOT NULL,
		observed numeric(78,0) NOT NULL,
		amount numeric(78,0) NOT NULL,
		deferred boolean NOT NULL,
		block_number bigint NOT NULL,

		PRIMARY KEY (deployment_key, no_id, epoch)
	);

	-- vault2 roots by commit seq: pending, then finalized (with its root
	-- index), replaced by a later commit, or deleted by a fault. recorded_time
	-- is set once a finalized root's new accounts and amounts are written.
	CREATE TABLE st_ledger_v2_root (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL,
		seq bigint NOT NULL CHECK (seq > 0),
		epoch bigint NOT NULL,
		root bytea NOT NULL CHECK (octet_length(root) = 32),
		total numeric(78,0) NOT NULL,
		leaf_count bigint NOT NULL,
		ledger_hash bytea NOT NULL CHECK (octet_length(ledger_hash) = 32),
		commit_block bigint NULL,
		status varchar(16) NOT NULL CHECK (status IN ('pending', 'finalized', 'replaced', 'deleted')),
		status_block bigint NULL,
		root_index bigint NULL,
		recorded_time timestamp NULL,

		PRIMARY KEY (deployment_key, no_id, seq),
		UNIQUE (deployment_key, no_id, root_index),
		CHECK ((status = 'finalized') = (root_index IS NOT NULL)),
		CHECK (recorded_time IS NULL OR status = 'finalized')
	);
	CREATE INDEX st_ledger_v2_root_status ON st_ledger_v2_root (deployment_key, no_id, status, seq);

	-- vault2 path demands, open until answered. A demand names a pending
	-- root's seq and an account of the finalized root root_index.
	CREATE TABLE st_ledger_v2_demand (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL,
		seq bigint NOT NULL,
		root_index bigint NOT NULL,
		account_index bigint NOT NULL,
		demander varchar(42) NOT NULL,
		demand_block bigint NOT NULL,
		answerer varchar(42) NULL,
		answer_block bigint NULL,

		PRIMARY KEY (deployment_key, no_id, seq, root_index, account_index)
	);
	CREATE INDEX st_ledger_v2_demand_open ON st_ledger_v2_demand (deployment_key, no_id, seq) WHERE answer_block IS NULL;

	-- Each covered epoch's amount per coldkey, written when the root that
	-- allocated it finalizes (§8.4 readers).
	CREATE TABLE st_ledger_v2_amount (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL,
		epoch bigint NOT NULL,
		coldkey bytea NOT NULL CHECK (octet_length(coldkey) = 32),
		amount numeric(78,0) NOT NULL CHECK (amount > 0),
		root_index bigint NOT NULL,

		PRIMARY KEY (deployment_key, no_id, epoch, coldkey)
	);
	CREATE INDEX st_ledger_v2_amount_coldkey ON st_ledger_v2_amount (deployment_key, coldkey, epoch);
`
