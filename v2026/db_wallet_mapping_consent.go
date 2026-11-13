// Migration 772 retains exact coldkey-signed provider mappings after the full
// SDK work custody prefix. No historical login statement is promoted into it.
package server

const walletMappingConsentSchemaSql = `
CREATE TABLE wallet_mapping_challenge (
 nonce bytea PRIMARY KEY CHECK (octet_length(nonce)=32),
 domain_hash bytea NOT NULL CHECK (octet_length(domain_hash)=32),
 client_id uuid NOT NULL,
 generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 4096),
 expires_at bigint NOT NULL,
 message text NOT NULL CHECK (octet_length(message) BETWEEN 1 AND 8192)
);
CREATE INDEX wallet_mapping_challenge_owner ON wallet_mapping_challenge(domain_hash,client_id,expires_at);
CREATE TABLE wallet_mapping_consent (
 domain_hash bytea NOT NULL CHECK (octet_length(domain_hash)=32),
 client_id uuid NOT NULL,
 generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 4096),
 original_hash bytea NOT NULL UNIQUE CHECK (octet_length(original_hash)=32),
 nonce bytea NOT NULL UNIQUE REFERENCES wallet_mapping_challenge(nonce),
 original bytea NOT NULL CHECK (octet_length(original) BETWEEN 1 AND 12288),
 accepted_at timestamp NOT NULL,
 PRIMARY KEY(domain_hash,client_id,generation)
);
CREATE FUNCTION wallet_mapping_original_guard() RETURNS trigger LANGUAGE plpgsql AS $wallet_mapping_guard$
BEGIN RAISE EXCEPTION 'wallet mapping originals are append-only'; END
$wallet_mapping_guard$;
CREATE TRIGGER wallet_mapping_challenge_guard BEFORE UPDATE OR DELETE ON wallet_mapping_challenge
 FOR EACH ROW EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER wallet_mapping_challenge_truncate_guard BEFORE TRUNCATE ON wallet_mapping_challenge
 FOR EACH STATEMENT EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER wallet_mapping_consent_guard BEFORE UPDATE OR DELETE ON wallet_mapping_consent
 FOR EACH ROW EXECUTE FUNCTION wallet_mapping_original_guard();
CREATE TRIGGER wallet_mapping_consent_truncate_guard BEFORE TRUNCATE ON wallet_mapping_consent
 FOR EACH STATEMENT EXECUTE FUNCTION wallet_mapping_original_guard();
`
