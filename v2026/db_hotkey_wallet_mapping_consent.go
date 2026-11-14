// Migration 789 retains the global hotkey wallet consent chains, one per
// (subnet, hotkey), with the networks that submitted them, and the per-network
// hotkey delegations with their issued challenges, one chain per (domain,
// network) beside the network consents of migration 786. A global chain needs
// no challenge and its keys are free to mint, so each submitting network links
// the hotkeys it stores and may link only a few. It widens the settled earning
// wallets of st_payout_wallet_resolution to mode hotkey: the delegation chain
// fills the existing consent and head columns, and the new hotkey columns name
// the delegation's hotkey and the global consent selected at the epoch. Every
// new table is append-only through the guard of migration 772. The added
// columns are nullable, so rows written before 789 and by older binaries stay
// valid.
package server

const hotkeyWalletMappingConsentSchemaSql = `
CREATE TABLE hotkey_wallet_mapping_consent (
 subnet_hash bytea NOT NULL CHECK (octet_length(subnet_hash)=32),
 hotkey bytea NOT NULL CHECK (octet_length(hotkey)=32),
 generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 4096),
 original_hash bytea NOT NULL UNIQUE CHECK (octet_length(original_hash)=32),
 original bytea NOT NULL CHECK (octet_length(original) BETWEEN 1 AND 12288),
 accepted_at timestamp NOT NULL,
 PRIMARY KEY(subnet_hash,hotkey,generation)
);
CREATE TABLE hotkey_wallet_mapping_submitter (
 network_id uuid NOT NULL,
 hotkey bytea NOT NULL CHECK (octet_length(hotkey)=32),
 create_time timestamp NOT NULL,
 PRIMARY KEY(network_id,hotkey)
);
CREATE TABLE hotkey_network_delegation_challenge (
 nonce bytea PRIMARY KEY CHECK (octet_length(nonce)=32),
 domain_hash bytea NOT NULL CHECK (octet_length(domain_hash)=32),
 network_id uuid NOT NULL,
 generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 4096),
 expires_at bigint NOT NULL,
 message text NOT NULL CHECK (octet_length(message) BETWEEN 1 AND 8192)
);
CREATE INDEX hotkey_network_delegation_challenge_owner ON hotkey_network_delegation_challenge(domain_hash,network_id,expires_at);
CREATE TABLE hotkey_network_delegation (
 domain_hash bytea NOT NULL CHECK (octet_length(domain_hash)=32),
 network_id uuid NOT NULL,
 generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 4096),
 original_hash bytea NOT NULL UNIQUE CHECK (octet_length(original_hash)=32),
 nonce bytea NOT NULL UNIQUE REFERENCES hotkey_network_delegation_challenge(nonce),
 original bytea NOT NULL CHECK (octet_length(original) BETWEEN 1 AND 12288),
 accepted_at timestamp NOT NULL,
 PRIMARY KEY(domain_hash,network_id,generation)
);
ALTER TABLE st_payout_wallet_resolution
 DROP CONSTRAINT st_payout_wallet_resolution_mode_check,
 ADD CONSTRAINT st_payout_wallet_resolution_mode_check CHECK (mode IN ('provider','network','hotkey')),
 ADD COLUMN hotkey bytea CHECK (octet_length(hotkey)=32),
 ADD COLUMN hotkey_consent_hash bytea CHECK (octet_length(hotkey_consent_hash)=32),
 ADD COLUMN hotkey_consent_generation bigint CHECK (hotkey_consent_generation BETWEEN 1 AND 4096),
 ADD COLUMN hotkey_consent_head_hash bytea CHECK (octet_length(hotkey_consent_head_hash)=32),
 ADD COLUMN hotkey_consent_head_generation bigint CHECK (hotkey_consent_head_generation BETWEEN 1 AND 4096),
 ADD CONSTRAINT st_payout_wallet_resolution_hotkey_columns_check CHECK (num_nonnulls(hotkey,hotkey_consent_hash,hotkey_consent_generation,hotkey_consent_head_hash,hotkey_consent_head_generation) IN (0,5)),
 ADD CONSTRAINT st_payout_wallet_resolution_hotkey_mode_check CHECK ((mode='hotkey')=(hotkey IS NOT NULL));
CREATE TRIGGER hotkey_wallet_mapping_consent_guard BEFORE UPDATE OR DELETE ON hotkey_wallet_mapping_consent
 FOR EACH ROW EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER hotkey_wallet_mapping_consent_truncate_guard BEFORE TRUNCATE ON hotkey_wallet_mapping_consent
 FOR EACH STATEMENT EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER hotkey_wallet_mapping_submitter_guard BEFORE UPDATE OR DELETE ON hotkey_wallet_mapping_submitter
 FOR EACH ROW EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER hotkey_wallet_mapping_submitter_truncate_guard BEFORE TRUNCATE ON hotkey_wallet_mapping_submitter
 FOR EACH STATEMENT EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER hotkey_network_delegation_challenge_guard BEFORE UPDATE OR DELETE ON hotkey_network_delegation_challenge
 FOR EACH ROW EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER hotkey_network_delegation_challenge_truncate_guard BEFORE TRUNCATE ON hotkey_network_delegation_challenge
 FOR EACH STATEMENT EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER hotkey_network_delegation_guard BEFORE UPDATE OR DELETE ON hotkey_network_delegation
 FOR EACH ROW EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER hotkey_network_delegation_truncate_guard BEFORE TRUNCATE ON hotkey_network_delegation
 FOR EACH STATEMENT EXECUTE FUNCTION wallet_mapping_original_guard();
`
