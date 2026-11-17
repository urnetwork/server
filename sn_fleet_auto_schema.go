package server

// Claiming a Top 200 spot from one tap (apple BTEASY.md, "Claiming a Top 200 spot";
// model/sn_fleet_auto.go). New tables only: nothing is rewritten, and no earlier
// release reads them. Epochs, blocks and generations are uint64 stored by bit pattern.
//
//   - sn_fleet_auto_authorization: the network's coldkey's signature authorizing one
//     hotkey, exactly as signed. A device checks it itself before it signs anything.
//   - sn_fleet_auto_device: the devices that asked for work, only for a network with
//     an authorization. A device on an older release or with signing off never asks,
//     so it is never here and never chosen.
//   - sn_fleet_auto_generation: one manifest, at most one built per settlement epoch
//     for a network, since each needs a new commitment and a fresh mirror.
//   - sn_fleet_auto_member: one chosen device of a generation and its binding, with
//     the signatures as they arrive. A device kept from an earlier generation has no
//     binding here.
//   - sn_fleet_auto_relay: every transaction the relay owes, one row per logical
//     operation, with the signed bytes once it has them, so a restart finishes the
//     one it was sending.
//   - sn_fleet_auto_top_up: the one top-up a network gets.
const snFleetAutoSchemaSql = `
	CREATE TABLE sn_fleet_auto_authorization (
		deployment_key varchar(96) NOT NULL,
		network_id uuid NOT NULL,
		coldkey bytea NOT NULL CHECK (octet_length(coldkey) = 32),
		hotkey bytea NOT NULL CHECK (octet_length(hotkey) = 32),
		fleet_id bytea NOT NULL CHECK (octet_length(fleet_id) = 32),
		message text NOT NULL,
		signature bytea NOT NULL CHECK (octet_length(signature) = 64),
		create_time timestamp NOT NULL,
		update_time timestamp NOT NULL,

		PRIMARY KEY (deployment_key, network_id)
	);
	CREATE UNIQUE INDEX sn_fleet_auto_authorization_hotkey ON sn_fleet_auto_authorization (deployment_key, hotkey);

	CREATE TABLE sn_fleet_auto_device (
		deployment_key varchar(96) NOT NULL,
		network_id uuid NOT NULL,
		client_id uuid NOT NULL,
		last_check_time timestamp NOT NULL,
		left_time timestamp NULL,

		PRIMARY KEY (deployment_key, network_id, client_id)
	);

	CREATE TABLE sn_fleet_auto_generation (
		deployment_key varchar(96) NOT NULL,
		network_id uuid NOT NULL,
		generation bigint NOT NULL,
		built_epoch bigint NOT NULL,
		fleet_id bytea NOT NULL CHECK (octet_length(fleet_id) = 32),
		hotkey bytea NOT NULL CHECK (octet_length(hotkey) = 32),
		manifest text NOT NULL,
		commitment_hash bytea NOT NULL CHECK (octet_length(commitment_hash) = 32),
		mirrored_block bigint NULL,
		create_time timestamp NOT NULL,

		PRIMARY KEY (deployment_key, network_id, generation)
	);
	CREATE UNIQUE INDEX sn_fleet_auto_generation_epoch ON sn_fleet_auto_generation (deployment_key, network_id, built_epoch);

	CREATE TABLE sn_fleet_auto_member (
		deployment_key varchar(96) NOT NULL,
		network_id uuid NOT NULL,
		generation bigint NOT NULL,
		client_id uuid NOT NULL,
		client_key bytea NOT NULL CHECK (octet_length(client_key) = 32),
		prefix_hash bytea NOT NULL CHECK (octet_length(prefix_hash) = 32),
		valid_from_epoch bigint NULL,
		valid_to_epoch bigint NULL,
		digest bytea NULL CHECK (digest IS NULL OR octet_length(digest) = 32),
		binding_json text NULL,
		hotkey_signature bytea NULL CHECK (hotkey_signature IS NULL OR octet_length(hotkey_signature) = 64),
		client_signature bytea NULL CHECK (client_signature IS NULL OR octet_length(client_signature) = 64),
		refusal varchar(32) NULL,
		relay_id uuid NULL,
		update_time timestamp NOT NULL,

		PRIMARY KEY (deployment_key, network_id, generation, client_id),
		CHECK ((digest IS NULL) = (binding_json IS NULL))
	);
	CREATE INDEX sn_fleet_auto_member_client ON sn_fleet_auto_member (deployment_key, client_id, generation DESC);

	CREATE TABLE sn_fleet_auto_relay (
		relay_id uuid NOT NULL,
		deployment_key varchar(96) NOT NULL,
		network_id uuid NOT NULL,
		kind varchar(16) NOT NULL,
		operation varchar(128) NOT NULL,
		from_key varchar(16) NOT NULL,
		to_address varchar(64) NOT NULL,
		calldata bytea NOT NULL,
		value_wei varchar(80) NOT NULL DEFAULT '0',
		status varchar(16) NOT NULL,
		from_address varchar(64) NULL,
		nonce bigint NULL,
		tx_hash varchar(80) NULL,
		raw_transaction bytea NULL,
		error text NULL,
		create_time timestamp NOT NULL,
		update_time timestamp NOT NULL,

		PRIMARY KEY (relay_id)
	);
	CREATE UNIQUE INDEX sn_fleet_auto_relay_operation ON sn_fleet_auto_relay (deployment_key, operation);
	CREATE INDEX sn_fleet_auto_relay_open ON sn_fleet_auto_relay (deployment_key, create_time) WHERE status IN ('queued', 'sent');

	CREATE TABLE sn_fleet_auto_top_up (
		deployment_key varchar(96) NOT NULL,
		network_id uuid NOT NULL,
		coldkey bytea NOT NULL CHECK (octet_length(coldkey) = 32),
		burn_rao bigint NOT NULL,
		fee_rao bigint NOT NULL,
		amount_rao bigint NOT NULL,
		relay_id uuid NOT NULL,
		create_time timestamp NOT NULL,

		PRIMARY KEY (deployment_key, network_id)
	);
`

var snFleetAutoSchemaMigration = newSqlMigration(snFleetAutoSchemaSql)

// The version that creates the claim's tables, wherever merges place it.
func SnFleetAutoSchemaMigrationVersion() int {
	return migrationVersion(snFleetAutoSchemaMigration)
}
