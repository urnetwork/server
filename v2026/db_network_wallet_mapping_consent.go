// Migration 786 retains coldkey-signed network wallet mappings, one chain per
// (domain, network) beside the unchanged per-provider chains of migration 772,
// and the earning wallet each published release epoch settled for every
// provider. All three tables are append-only.
package server

const networkWalletMappingConsentSchemaSql = `
CREATE TABLE network_wallet_mapping_challenge (
 nonce bytea PRIMARY KEY CHECK (octet_length(nonce)=32),
 domain_hash bytea NOT NULL CHECK (octet_length(domain_hash)=32),
 network_id uuid NOT NULL,
 generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 4096),
 expires_at bigint NOT NULL,
 message text NOT NULL CHECK (octet_length(message) BETWEEN 1 AND 8192)
);
CREATE INDEX network_wallet_mapping_challenge_owner ON network_wallet_mapping_challenge(domain_hash,network_id,expires_at);
CREATE TABLE network_wallet_mapping_consent (
 domain_hash bytea NOT NULL CHECK (octet_length(domain_hash)=32),
 network_id uuid NOT NULL,
 generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 4096),
 original_hash bytea NOT NULL UNIQUE CHECK (octet_length(original_hash)=32),
 nonce bytea NOT NULL UNIQUE REFERENCES network_wallet_mapping_challenge(nonce),
 original bytea NOT NULL CHECK (octet_length(original) BETWEEN 1 AND 12288),
 accepted_at timestamp NOT NULL,
 PRIMARY KEY(domain_hash,network_id,generation)
);
CREATE TABLE st_payout_wallet_resolution (
 deployment_key varchar(96) NOT NULL,
 epoch bigint NOT NULL,
 no_id bigint NOT NULL,
 client_id uuid NOT NULL,
 network_id uuid NOT NULL,
 mode varchar(16) NOT NULL CHECK (mode IN ('provider','network')),
 coldkey bytea NOT NULL CHECK (octet_length(coldkey)=32),
 consent_hash bytea NOT NULL CHECK (octet_length(consent_hash)=32),
 consent_generation bigint NOT NULL CHECK (consent_generation BETWEEN 1 AND 4096),
 head_hash bytea NOT NULL CHECK (octet_length(head_hash)=32),
 head_generation bigint NOT NULL CHECK (head_generation BETWEEN 1 AND 4096),
 PRIMARY KEY(deployment_key,epoch,no_id,client_id)
);
CREATE TRIGGER network_wallet_mapping_challenge_guard BEFORE UPDATE OR DELETE ON network_wallet_mapping_challenge
 FOR EACH ROW EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER network_wallet_mapping_challenge_truncate_guard BEFORE TRUNCATE ON network_wallet_mapping_challenge
 FOR EACH STATEMENT EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER network_wallet_mapping_consent_guard BEFORE UPDATE OR DELETE ON network_wallet_mapping_consent
 FOR EACH ROW EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER network_wallet_mapping_consent_truncate_guard BEFORE TRUNCATE ON network_wallet_mapping_consent
 FOR EACH STATEMENT EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER st_payout_wallet_resolution_guard BEFORE UPDATE OR DELETE ON st_payout_wallet_resolution
 FOR EACH ROW EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER st_payout_wallet_resolution_truncate_guard BEFORE TRUNCATE ON st_payout_wallet_resolution
 FOR EACH STATEMENT EXECUTE FUNCTION wallet_mapping_original_guard();
`
