package server

// Capture the flag's durable state (sn CAPTURETHEFLAG.md; model/ctf_model.go),
// for the whole lifecycle: the presence samples, each epoch's census, its
// commitment and draw, the awards, pots and pending awards each covered epoch
// fixes, the ledgers built and the carry contract's returns. One migration,
// so the census, the driver and the API add none of their own.
//
//   - ctf_presence_sample: one row per quarter-hour sample taken (§4.2), the
//     sample's time and block and how many miners it found.
//   - ctf_presence: one row per miner, role and country of an epoch, with the
//     samples that found it present there as a bitmap.
//   - ctf_presence_outage: the platform-wide outages the operator declares.
//     Every sample whose quarter hour overlaps one is excused.
//   - ctf_census: an epoch's census (§4.3), written once: its hash, header and
//     counts.
//   - ctf_census_object: the census's objects (header, row chunks, exclusion
//     object and countries object), kept until they are well past
//     publication in the ledger object store.
//   - ctf_census_member: every miner the samples found in a role during the
//     epoch, with every rule evaluated, its country, unit and payout coldkey.
//   - ctf_census_pot and ctf_census_exclusion: the census's per-country
//     counts, the exclusion rows exactly as the census object holds them.
//   - ctf_census_commitment and ctf_draw: the on-chain commitment of the
//     census and the drand pulse and seed of its draw (§4.4).
//   - ctf_epoch, ctf_award, ctf_pot, ctf_pending_award: what each covered
//     epoch fixed at the first live ledger that covered it (§4.5, §5).
//   - ctf_ledger: every CTF ledger built, a live root's or a shadow run's.
//   - ctf_recycle: the carry contract's Recycled logs (§4.6).
//
// Every table is keyed by the deployment and the operator's v1 id n, never the
// CTF pool id 2^32 + n. The pool's on-chain mirror (roots by seq, accounts,
// funding, demands and amounts) needs no table here: the st_ledger_v2_* tables
// take its pool id as their no_id. Epochs, blocks and rounds are uint64 stored
// by bit pattern, amounts are rao.
//
// New tables only, each created when absent under a five-second lock bound:
// nothing existing is locked, read or rewritten, and no earlier release reads
// them. Apply before deploying the taskworker and api that use them; they stay
// empty while ctf.yml leaves capture the flag off.
const ctfSchemaSql = `
	SET LOCAL lock_timeout = '5s';

	CREATE TABLE IF NOT EXISTS ctf_presence_sample (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		sample_index smallint NOT NULL CHECK (0 <= sample_index AND sample_index < 672),
		sample_time timestamp NOT NULL,
		head_block bigint NOT NULL,
		provider_source_time timestamp NULL,
		providers integer NOT NULL CHECK (providers >= 0),
		extenders integer NOT NULL CHECK (extenders >= 0),

		PRIMARY KEY (deployment_key, no_id, epoch, sample_index)
	);

	CREATE TABLE IF NOT EXISTS ctf_presence (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		role smallint NOT NULL CHECK (role IN (1, 2)),
		client_id uuid NOT NULL,
		country_code varchar(2) NOT NULL CHECK (country_code ~ '^[a-z]{2}$'),
		samples bytea NOT NULL CHECK (octet_length(samples) = 84),
		sample_count smallint NOT NULL CHECK (0 < sample_count AND sample_count <= 672),
		unit bytea NULL CHECK (octet_length(unit) = 32),
		unit_family smallint NULL CHECK (unit_family IN (4, 6)),
		last_sample_index smallint NOT NULL CHECK (0 <= last_sample_index AND last_sample_index < 672),
		update_time timestamp NOT NULL,

		PRIMARY KEY (deployment_key, no_id, epoch, role, client_id, country_code),
		CHECK ((unit IS NULL) = (unit_family IS NULL))
	);

	CREATE TABLE IF NOT EXISTS ctf_presence_outage (
		outage_id uuid NOT NULL,
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		start_time timestamp NOT NULL,
		end_time timestamp NOT NULL,
		reason varchar(1024) NOT NULL,
		declared_by varchar(256) NOT NULL,
		create_time timestamp NOT NULL,

		PRIMARY KEY (outage_id),
		CHECK (start_time < end_time)
	);
	CREATE INDEX IF NOT EXISTS ctf_presence_outage_end ON ctf_presence_outage (deployment_key, no_id, end_time);

	CREATE TABLE IF NOT EXISTS ctf_census (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		census_hash bytea NOT NULL CHECK (octet_length(census_hash) = 32),
		header bytea NOT NULL,
		candidates bigint NOT NULL CHECK (candidates >= 0),
		provider_candidates bigint NOT NULL CHECK (provider_candidates >= 0),
		extender_candidates bigint NOT NULL CHECK (extender_candidates >= 0),
		provider_units bigint NOT NULL CHECK (provider_units >= 0),
		extender_units bigint NOT NULL CHECK (extender_units >= 0),
		provider_members bigint NOT NULL CHECK (provider_members >= 0),
		extender_members bigint NOT NULL CHECK (extender_members >= 0),
		excused_samples smallint[] NOT NULL,
		samples_taken smallint NOT NULL CHECK (0 <= samples_taken AND samples_taken <= 672),
		provider_samples_taken smallint NOT NULL CHECK (0 <= provider_samples_taken AND provider_samples_taken <= 672),
		required_samples smallint NOT NULL CHECK (0 <= required_samples AND required_samples <= 336),
		start_block bigint NOT NULL,
		end_block bigint NOT NULL,
		start_time timestamp NOT NULL,
		end_time timestamp NOT NULL,
		capture_time timestamp NOT NULL,
		provider_source_time timestamp NOT NULL,
		create_time timestamp NOT NULL,
		published_time timestamp NULL,

		PRIMARY KEY (deployment_key, no_id, epoch),
		CHECK (candidates = provider_candidates + extender_candidates)
	);

	CREATE TABLE IF NOT EXISTS ctf_census_object (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		ref bytea NOT NULL CHECK (octet_length(ref) = 32),
		data bytea NOT NULL,

		PRIMARY KEY (deployment_key, no_id, epoch, ref)
	);

	CREATE TABLE IF NOT EXISTS ctf_census_member (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		role smallint NOT NULL CHECK (role IN (1, 2)),
		client_id uuid NOT NULL,
		network_id uuid NULL,
		extender_id uuid NULL,
		candidate boolean NOT NULL,
		reason smallint NOT NULL CHECK (0 <= reason AND reason <= 10),
		failed integer NOT NULL CHECK (0 <= failed AND failed < 2048),
		country_code varchar(2) NOT NULL CHECK (country_code ~ '^[a-z]{2}$'),
		draw_country_code varchar(2) NULL CHECK (draw_country_code ~ '^[a-z]{2}$'),
		present_samples smallint NOT NULL CHECK (0 <= present_samples AND present_samples <= 672),
		unit bytea NULL CHECK (octet_length(unit) = 32),
		unit_family smallint NULL CHECK (unit_family IN (4, 6)),
		coldkey bytea NULL CHECK (octet_length(coldkey) = 32),

		PRIMARY KEY (deployment_key, no_id, epoch, role, client_id),
		CHECK (candidate = (reason = 0)),
		CHECK (candidate = (failed = 0)),
		CHECK ((unit IS NULL) = (unit_family IS NULL)),
		CHECK (NOT candidate OR (unit IS NOT NULL AND draw_country_code IS NOT NULL AND coldkey IS NOT NULL))
	);
	CREATE INDEX IF NOT EXISTS ctf_census_member_client ON ctf_census_member (deployment_key, no_id, client_id, epoch);

	CREATE TABLE IF NOT EXISTS ctf_census_pot (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		country_code varchar(2) NOT NULL CHECK (country_code ~ '^[a-z]{2}$'),
		role smallint NOT NULL CHECK (role IN (1, 2)),
		units integer NOT NULL CHECK (units >= 0),
		candidates integer NOT NULL CHECK (candidates >= units),
		near_misses integer NOT NULL CHECK (near_misses >= 0),

		PRIMARY KEY (deployment_key, no_id, epoch, country_code, role)
	);

	CREATE TABLE IF NOT EXISTS ctf_census_exclusion (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		country_code varchar(2) NOT NULL CHECK (country_code ~ '^[a-z]{2}$'),
		role smallint NOT NULL CHECK (role IN (1, 2)),
		reason smallint NOT NULL CHECK (1 <= reason AND reason <= 10),
		count integer NOT NULL CHECK (count > 0),

		PRIMARY KEY (deployment_key, no_id, epoch, country_code, role, reason)
	);

	CREATE TABLE IF NOT EXISTS ctf_census_commitment (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		census_hash bytea NOT NULL CHECK (octet_length(census_hash) = 32),
		status varchar(16) NOT NULL CHECK (status IN ('pending', 'sent', 'committed', 'missed', 'refused')),
		tx_hash varchar(66) NULL,
		commit_block bigint NULL,
		commit_block_time bigint NULL,
		round bigint NULL,
		create_time timestamp NOT NULL,
		update_time timestamp NOT NULL,

		PRIMARY KEY (deployment_key, no_id, epoch),
		CHECK (status <> 'committed' OR (commit_block IS NOT NULL AND commit_block_time IS NOT NULL))
	);

	CREATE TABLE IF NOT EXISTS ctf_draw (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		census_hash bytea NOT NULL CHECK (octet_length(census_hash) = 32),
		round bigint NOT NULL,
		signature bytea NOT NULL CHECK (octet_length(signature) = 48),
		randomness bytea NOT NULL CHECK (octet_length(randomness) = 32),
		seed bytea NOT NULL CHECK (octet_length(seed) = 32),
		create_time timestamp NOT NULL,

		PRIMARY KEY (deployment_key, no_id, epoch)
	);

	CREATE TABLE IF NOT EXISTS ctf_epoch (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		ledger_epoch bigint NOT NULL,
		ledger_hash bytea NOT NULL CHECK (octet_length(ledger_hash) = 32),
		census_hash bytea NULL CHECK (octet_length(census_hash) = 32),
		census_block bigint NULL,
		census_error text NULL,
		drawn boolean NOT NULL,
		round bigint NOT NULL,
		candidates bigint NOT NULL CHECK (candidates >= 0),
		funding numeric(78,0) NOT NULL CHECK (funding >= 0),
		recycled numeric(78,0) NOT NULL CHECK (recycled >= 0),
		owed numeric(78,0) NOT NULL CHECK (owed >= 0),
		returned numeric(78,0) NOT NULL CHECK (returned >= 0),
		credited numeric(78,0) NOT NULL CHECK (credited >= 0),
		matured boolean NOT NULL,
		fresh numeric(78,0) NOT NULL CHECK (fresh >= 0),
		shortfall numeric(78,0) NOT NULL CHECK (shortfall >= 0),
		slot_base numeric(78,0) NOT NULL CHECK (slot_base >= 0),
		slot_remainder integer NOT NULL CHECK (0 <= slot_remainder AND slot_remainder < 2490),
		awards integer NOT NULL CHECK (0 <= awards AND awards <= 2490),
		paid_slots numeric(78,0) NOT NULL CHECK (paid_slots >= 0),
		paid_pots numeric(78,0) NOT NULL CHECK (paid_pots >= 0),
		paid_pending numeric(78,0) NOT NULL CHECK (paid_pending >= 0),
		pending_total numeric(78,0) NOT NULL CHECK (pending_total >= 0),
		carried numeric(78,0) NOT NULL CHECK (carried >= 0),
		awards_ref bytea NOT NULL CHECK (octet_length(awards_ref) = 32),
		pots_ref bytea NOT NULL CHECK (octet_length(pots_ref) = 32),
		pending_ref bytea NOT NULL CHECK (octet_length(pending_ref) = 32),
		report jsonb NULL,
		create_time timestamp NOT NULL,

		PRIMARY KEY (deployment_key, no_id, epoch),
		CHECK (drawn = (round <> 0)),
		CHECK (NOT drawn OR census_hash IS NOT NULL)
	);

	CREATE TABLE IF NOT EXISTS ctf_award (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		role smallint NOT NULL CHECK (role IN (1, 2)),
		country_code varchar(2) NOT NULL CHECK (country_code ~ '^[a-z]{2}$'),
		rank smallint NOT NULL CHECK (1 <= rank AND rank <= 5),
		unit bytea NOT NULL CHECK (octet_length(unit) = 32),
		unit_family smallint NULL CHECK (unit_family IN (4, 6)),
		client_id uuid NOT NULL,
		coldkey bytea NOT NULL CHECK (octet_length(coldkey) = 32),
		slot_rao numeric(78,0) NOT NULL CHECK (slot_rao >= 0),
		pot_rao numeric(78,0) NOT NULL CHECK (pot_rao >= 0),
		deferred boolean NOT NULL,

		PRIMARY KEY (deployment_key, no_id, epoch, role, country_code, rank)
	);
	CREATE INDEX IF NOT EXISTS ctf_award_client ON ctf_award (deployment_key, no_id, client_id, epoch);
	CREATE INDEX IF NOT EXISTS ctf_award_coldkey ON ctf_award (deployment_key, coldkey, epoch);

	CREATE TABLE IF NOT EXISTS ctf_pot (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		country_code varchar(2) NOT NULL CHECK (country_code ~ '^[a-z]{2}$'),
		role smallint NOT NULL CHECK (role IN (1, 2)),
		units integer NOT NULL CHECK (units >= 0),
		winners smallint NOT NULL CHECK (0 <= winners AND winners <= 5),
		pot_in numeric(78,0) NOT NULL CHECK (pot_in >= 0),
		pot_out numeric(78,0) NOT NULL CHECK (pot_out >= 0),

		PRIMARY KEY (deployment_key, no_id, epoch, country_code, role)
	);
	CREATE INDEX IF NOT EXISTS ctf_pot_country ON ctf_pot (deployment_key, no_id, country_code, epoch);

	CREATE TABLE IF NOT EXISTS ctf_pending_award (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		award_epoch bigint NOT NULL CHECK (award_epoch > 0),
		role smallint NOT NULL CHECK (role IN (1, 2)),
		country_code varchar(2) NOT NULL CHECK (country_code ~ '^[a-z]{2}$'),
		rank smallint NOT NULL CHECK (1 <= rank AND rank <= 5),
		coldkey bytea NOT NULL CHECK (octet_length(coldkey) = 32),
		amount numeric(78,0) NOT NULL CHECK (amount > 0),

		PRIMARY KEY (deployment_key, no_id, epoch, award_epoch, role, country_code, rank)
	);

	CREATE TABLE IF NOT EXISTS ctf_ledger (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		ledger_hash bytea NOT NULL CHECK (octet_length(ledger_hash) = 32),
		shadow boolean NOT NULL,
		first_epoch bigint NOT NULL CHECK (0 < first_epoch AND first_epoch <= epoch),
		root bytea NOT NULL CHECK (octet_length(root) = 32),
		total numeric(78,0) NOT NULL CHECK (total >= 0),
		leaf_count bigint NOT NULL CHECK (leaf_count >= 0),
		header bytea NOT NULL,
		report jsonb NULL,
		status varchar(16) NOT NULL CHECK (status IN ('built', 'committed', 'finalized', 'replaced', 'deleted', 'refused', 'shadow')),
		seq bigint NULL,
		root_index bigint NULL,
		commit_block bigint NULL,
		finalize_block bigint NULL,
		create_time timestamp NOT NULL,
		update_time timestamp NOT NULL,

		PRIMARY KEY (deployment_key, no_id, epoch, ledger_hash),
		CHECK (shadow = (status = 'shadow'))
	);

	CREATE TABLE IF NOT EXISTS ctf_recycle (
		deployment_key varchar(96) NOT NULL,
		no_id bigint NOT NULL CHECK (no_id > 0),
		block_number bigint NOT NULL,
		log_index integer NOT NULL CHECK (log_index >= 0),
		epoch bigint NOT NULL CHECK (epoch > 0),
		amount numeric(78,0) NOT NULL CHECK (amount >= 0),
		tx_hash varchar(66) NOT NULL,

		PRIMARY KEY (deployment_key, no_id, block_number, log_index)
	);
	CREATE INDEX IF NOT EXISTS ctf_recycle_epoch ON ctf_recycle (deployment_key, no_id, epoch);
`

var ctfSchemaMigration = newSqlMigration(ctfSchemaSql)

// The version that creates capture the flag's tables, wherever merges place it.
func CtfSchemaMigrationVersion() int {
	return migrationVersion(ctfSchemaMigration)
}
