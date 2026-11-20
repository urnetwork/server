package server

// The chain-side tables of the SN top board (LEADERTOP.md §5.7, §6.1, §6.2):
// the live fleet binding census, the per-tempo on-chain incentive census and
// the validator's signed head summaries.
// New tables only: nothing is rewritten, and no earlier release reads them.
// Epochs, blocks and generations are uint64 stored by bit pattern.
//
// sn_fleet_binding holds one row per binding version, applied in log order
// from the coordinator's FleetBound, FleetBindingRevoked and
// FleetBindingCleaned logs (model/sn_fleet_binding.go); sn_fleet_binding_sync
// is the next block the binding sync reads and the canonical hash of the
// block before it. sn_incentive_snapshot and sn_incentive_uid hold one tempo's
// incentive census each (model/sn_incentive.go). sn_head_summary holds one
// accepted head summary per (netuid, validator hotkey, native epoch) with its
// exact canonical body, and sn_head_summary_fleet one row per fleet identity
// of its eligible UIDs (model/sn_head_summary.go).
const snChainSchemaSql = `
	CREATE TABLE sn_fleet_binding (
		deployment_key varchar(96) NOT NULL,
		client_id uuid NOT NULL,
		generation bigint NOT NULL,
		fleet_id bytea NOT NULL CHECK (octet_length(fleet_id) = 32),
		hotkey bytea NOT NULL CHECK (octet_length(hotkey) = 32),
		uid integer NOT NULL CHECK (uid BETWEEN 0 AND 65535),
		valid_from_epoch bigint NOT NULL,
		signed_valid_to_epoch bigint NOT NULL,
		valid_to_epoch bigint NOT NULL,
		revoked_effective_epoch bigint NULL,
		cleaned_at_epoch bigint NULL,
		cleaned_block bigint NULL,
		cleaned_log_index integer NULL,
		bound_block bigint NOT NULL,
		bound_log_index integer NOT NULL,

		PRIMARY KEY (deployment_key, client_id, generation),
		CHECK ((cleaned_at_epoch IS NULL) = (cleaned_block IS NULL) AND (cleaned_block IS NULL) = (cleaned_log_index IS NULL))
	);
	CREATE UNIQUE INDEX sn_fleet_binding_bound ON sn_fleet_binding (deployment_key, client_id, bound_block, bound_log_index);

	CREATE TABLE sn_fleet_binding_sync (
		deployment_key varchar(96) NOT NULL,
		next_block bigint NOT NULL,
		block_hash varchar(80) NOT NULL,
		update_time timestamp NOT NULL,

		PRIMARY KEY (deployment_key)
	);

	CREATE TABLE sn_incentive_snapshot (
		deployment_key varchar(96) NOT NULL,
		subnet_epoch_index bigint NOT NULL,
		block_number bigint NOT NULL,
		block_hash varchar(80) NOT NULL,
		read_at timestamp NOT NULL,
		netuid integer NOT NULL,
		subnetwork_n integer NOT NULL,
		tempo integer NOT NULL,
		activity_cutoff bigint NOT NULL,
		reference_uid integer NULL,
		reference_last_update bigint NOT NULL,
		reference_validator_trust integer NOT NULL,
		fallback_row boolean NOT NULL,
		stale boolean NOT NULL,
		stale_reasons text NOT NULL,
		validator_rows_complete boolean NOT NULL,
		validator_rows_json text NOT NULL,
		response_bytes bigint NOT NULL,

		PRIMARY KEY (deployment_key, subnet_epoch_index)
	);
	CREATE INDEX sn_incentive_snapshot_read_at ON sn_incentive_snapshot (deployment_key, read_at);

	CREATE TABLE sn_incentive_uid (
		deployment_key varchar(96) NOT NULL,
		subnet_epoch_index bigint NOT NULL,
		uid integer NOT NULL CHECK (uid BETWEEN 0 AND 65535),
		incentive integer NOT NULL,
		last_update bigint NOT NULL,
		validator_permit boolean NOT NULL,
		validator_trust integer NOT NULL,
		hotkey bytea NULL CHECK (hotkey IS NULL OR octet_length(hotkey) = 32),
		block_at_registration bigint NULL,

		PRIMARY KEY (deployment_key, subnet_epoch_index, uid),
		CHECK ((hotkey IS NULL) = (block_at_registration IS NULL))
	);

	CREATE TABLE sn_head_summary (
		netuid integer NOT NULL CHECK (netuid BETWEEN 1 AND 65535),
		validator_hotkey bytea NOT NULL CHECK (octet_length(validator_hotkey) = 32),
		native_epoch bigint NOT NULL,
		native_snapshot_block bigint NOT NULL,
		native_snapshot_hash bytea NOT NULL CHECK (octet_length(native_snapshot_hash) = 32),
		evm_snapshot_block bigint NOT NULL,
		evm_snapshot_hash bytea NOT NULL CHECK (octet_length(evm_snapshot_hash) = 32),
		settlement_epoch bigint NOT NULL,
		policy_id bigint NOT NULL,
		policy_hash bytea NOT NULL CHECK (octet_length(policy_hash) = 32),
		measurement_artifact_hash varchar(71) NOT NULL,
		validator_uid integer NOT NULL CHECK (validator_uid BETWEEN 0 AND 65535),
		maximum_head_fleets integer NOT NULL,
		entry_count integer NOT NULL,
		selected_count integer NOT NULL,
		signed_at timestamp NOT NULL,
		signing_hash varchar(71) NOT NULL,
		received_at timestamp NOT NULL,
		body bytea NOT NULL,

		PRIMARY KEY (netuid, validator_hotkey, native_epoch)
	);
	CREATE INDEX sn_head_summary_received_at ON sn_head_summary (received_at);

	CREATE TABLE sn_head_summary_fleet (
		netuid integer NOT NULL,
		validator_hotkey bytea NOT NULL,
		native_epoch bigint NOT NULL,
		uid integer NOT NULL CHECK (uid BETWEEN 0 AND 65535),
		fleet_id bytea NOT NULL CHECK (octet_length(fleet_id) = 32),
		generation bigint NOT NULL,
		hotkey bytea NOT NULL CHECK (octet_length(hotkey) = 32),
		rank integer NOT NULL,
		score_numerator text NOT NULL,
		score_denominator text NOT NULL,
		score double precision NOT NULL,
		selected boolean NOT NULL,

		PRIMARY KEY (netuid, validator_hotkey, native_epoch, uid, fleet_id, generation)
	)
`

// The catalog monitor follows this immutable DDL wherever merges place it.
func SnChainSchemaMigrationVersion() int {
	return sqlMigrationVersion(snChainSchemaSql)
}
