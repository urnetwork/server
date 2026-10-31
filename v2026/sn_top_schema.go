package server

// The SN Top board's storage (LEADERTOP.md §8). Owner decision 2026-10-29
// (LEADERTOP). New tables only: nothing is rewritten, and a release that
// predates them never reads or writes them, so the migration applies before
// the api and taskworker that use them.
//
//   - sn_trail_hop: one row per hop of a complete verify trail, written with
//     the trail's terminal row. verify_trail keeps hops as JSON with no time
//     index; this table is what the board's replica reads by time.
//   - sn_top_snapshot: one per tempo the board folded.
//   - sn_top_network: each ranked network's row in a snapshot. Rows past the
//     paged top 400 are kept for the newest snapshots only.
//   - sn_top_fleet: each claimed fleet's row in a snapshot.
//   - sn_top_ema_state: each unit's EMA and the tempo it was last folded.
//   - sn_top_network_daily: each network's rank and score at the end of each
//     UTC day, kept 90 days for the progress chart.
const snTopSchemaSql = `
	CREATE TABLE sn_trail_hop (
		trail_id uuid NOT NULL,
		hop_index smallint NOT NULL,
		tempo_index bigint NOT NULL,
		head_window varchar(32) NOT NULL,
		client_id uuid NOT NULL,
		network_id uuid NULL,
		prefix_hash bytea NOT NULL CHECK (octet_length(prefix_hash) = 32),
		seed bool NOT NULL,
		complete bool NOT NULL,
		confirm_time timestamp NOT NULL,

		PRIMARY KEY (trail_id, hop_index)
	);
	CREATE INDEX sn_trail_hop_confirm_time ON sn_trail_hop (confirm_time);

	CREATE TABLE sn_top_snapshot (
		snapshot_id uuid NOT NULL,
		tempo_index bigint NOT NULL,
		tempo_source varchar(16) NOT NULL,
		block bigint NOT NULL,
		tempo_start_time timestamp NOT NULL,
		tempo_end_time timestamp NOT NULL,
		computed_at timestamp NOT NULL,
		settlement_epoch bigint NOT NULL,
		total_ranked int NOT NULL,
		head_seats int NOT NULL,
		claimed_fleets int NOT NULL,
		open_seats int NOT NULL,
		projected_cutoff double precision NOT NULL,
		claimed_cutoff double precision NULL,
		binding_open bool NOT NULL,
		binding_note text NOT NULL,
		eligible_providers int NOT NULL,
		measured_providers int NOT NULL,
		cohort_capacity int NOT NULL,
		presence_source varchar(16) NOT NULL,
		validator_summary_at timestamp NULL,
		incentive_at timestamp NULL,
		precision_at_200 double precision NULL,

		PRIMARY KEY (snapshot_id)
	);
	CREATE INDEX sn_top_snapshot_computed_at ON sn_top_snapshot (computed_at);

	CREATE TABLE sn_top_network (
		snapshot_id uuid NOT NULL,
		network_id uuid NOT NULL,
		position int NOT NULL,
		projected_score double precision NOT NULL,
		raw_last_tempo double precision NOT NULL,
		solo_score double precision NOT NULL,
		margin double precision NOT NULL,
		trend int NOT NULL,
		prefixes int NOT NULL,
		shared_prefixes int NOT NULL,
		shared_with_units int NOT NULL,
		presence double precision NOT NULL,
		label varchar(16) NOT NULL,
		measurable bool NOT NULL,
		verify_eligible bool NOT NULL,
		claim_prompt bool NOT NULL,
		claimed bool NOT NULL,
		not_measurable_reason varchar(16) NOT NULL,

		PRIMARY KEY (snapshot_id, network_id)
	);
	CREATE INDEX sn_top_network_snapshot_position ON sn_top_network (snapshot_id, position);

	CREATE TABLE sn_top_fleet (
		snapshot_id uuid NOT NULL,
		fleet_key varchar(160) NOT NULL,
		fleet_id bytea NOT NULL,
		hotkey bytea NOT NULL,
		generation bigint NOT NULL,
		uid int NOT NULL,
		valid_to_epoch bigint NOT NULL,
		network_ids uuid[] NOT NULL,
		replica_score double precision NOT NULL,
		replica_raw double precision NOT NULL,
		replica_selected bool NOT NULL,
		validator_score double precision NULL,
		validator_rank int NULL,
		validator_rank_of int NULL,
		selected bool NULL,
		incentive double precision NULL,
		incentive_stale bool NOT NULL,

		PRIMARY KEY (snapshot_id, fleet_key)
	);

	CREATE TABLE sn_top_ema_state (
		unit_kind varchar(8) NOT NULL,
		unit_key varchar(160) NOT NULL,
		ema double precision NOT NULL,
		ema_exact text NULL,
		solo_ema double precision NOT NULL,
		last_tempo_index bigint NOT NULL,
		last_position int NOT NULL,
		measurable_bits int NOT NULL,
		update_time timestamp NOT NULL,

		PRIMARY KEY (unit_kind, unit_key)
	);
	CREATE INDEX sn_top_ema_state_update_time ON sn_top_ema_state (update_time);

	CREATE TABLE sn_top_network_daily (
		network_id uuid NOT NULL,
		day date NOT NULL,
		rank int NOT NULL,
		score double precision NOT NULL,

		PRIMARY KEY (network_id, day)
	);
	CREATE INDEX sn_top_network_daily_day ON sn_top_network_daily (day);
`
