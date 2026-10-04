// Startup enrollment retains the SDK's original generation statement alongside
// the independently signed client-key registration used for first admission.
package server

// Migration 774 follows SDK request custody, wallet consent and request lookup.
const providerWorkOwnerSchemaSql = `
CREATE TABLE provider_work_owner (
 domain_hash bytea NOT NULL CHECK (octet_length(domain_hash)=32),
 client_id bytea NOT NULL CHECK (octet_length(client_id)=16),
 generation bytea NOT NULL CHECK (octet_length(generation)=16),
 public_key bytea NOT NULL CHECK (octet_length(public_key)=32),
 owner_hash bytea NOT NULL UNIQUE CHECK (octet_length(owner_hash)=32),
 original bytea NOT NULL CHECK (octet_length(original) BETWEEN 1 AND 4096),
 key_registration bytea NOT NULL CHECK (octet_length(key_registration) BETWEEN 1 AND 8192),
 retained_at timestamp NOT NULL DEFAULT timezone('utc',now()),
 PRIMARY KEY(domain_hash,client_id,generation)
);
CREATE INDEX provider_work_owner_client ON provider_work_owner(client_id);
CREATE INDEX provider_work_owner_identity ON provider_work_owner(domain_hash,client_id,public_key,generation);
CREATE TRIGGER provider_work_owner_guard BEFORE UPDATE OR DELETE ON provider_work_owner
 FOR EACH ROW EXECUTE FUNCTION provider_work_original_guard();
CREATE TRIGGER provider_work_owner_truncate_guard BEFORE TRUNCATE ON provider_work_owner
 FOR EACH STATEMENT EXECUTE FUNCTION provider_work_original_guard();
`
