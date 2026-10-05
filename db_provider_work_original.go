// SDK boundary custody retains exact signed requests and cuts without replacing
// a previously approved boundary or interpreting database rows as a full roster.
package server

// Migration 771 follows the published prefix and verification originals at 770.
const providerWorkOriginalSchemaSql = `
CREATE TABLE provider_work_request (
 request_hash bytea PRIMARY KEY CHECK (octet_length(request_hash)=32),
 request_id bytea NOT NULL UNIQUE CHECK (octet_length(request_id)=16),
 domain_hash bytea NOT NULL CHECK (octet_length(domain_hash)=32),
 client_id bytea NOT NULL CHECK (octet_length(client_id)=16),
 generation bytea NOT NULL CHECK (octet_length(generation)=16),
 public_key bytea NOT NULL CHECK (octet_length(public_key)=32),
 epoch numeric(20,0) NOT NULL CHECK (epoch BETWEEN 0 AND 18446744073709551615),
 kind varchar(5) NOT NULL CHECK (kind IN ('start','end')),
 issued_at bigint NOT NULL,
 expires_at bigint NOT NULL CHECK (expires_at>issued_at AND expires_at-issued_at<=3600),
 original bytea NOT NULL CHECK (octet_length(original) BETWEEN 1 AND 8192),
 retained_at timestamp NOT NULL DEFAULT timezone('utc',now()),
 UNIQUE(domain_hash,client_id,generation,epoch,kind)
);
CREATE INDEX provider_work_request_owner ON provider_work_request(domain_hash,client_id,generation,public_key,expires_at);
CREATE TABLE provider_work_cut (
 request_hash bytea PRIMARY KEY REFERENCES provider_work_request(request_hash),
 cut_hash bytea NOT NULL CHECK (octet_length(cut_hash)=32),
 original bytea NOT NULL CHECK (octet_length(original) BETWEEN 1 AND 8388608),
 retained_at timestamp NOT NULL DEFAULT timezone('utc',now())
);
CREATE INDEX provider_work_cut_hash ON provider_work_cut(cut_hash);
CREATE FUNCTION provider_work_original_guard() RETURNS trigger LANGUAGE plpgsql AS $provider_work_guard$
BEGIN RAISE EXCEPTION 'provider work originals are append-only'; END
$provider_work_guard$;
CREATE TRIGGER provider_work_request_guard BEFORE UPDATE OR DELETE ON provider_work_request
 FOR EACH ROW EXECUTE FUNCTION provider_work_original_guard();
CREATE TRIGGER provider_work_request_truncate_guard BEFORE TRUNCATE ON provider_work_request
 FOR EACH STATEMENT EXECUTE FUNCTION provider_work_original_guard();
CREATE TRIGGER provider_work_cut_guard BEFORE UPDATE OR DELETE ON provider_work_cut
 FOR EACH ROW EXECUTE FUNCTION provider_work_original_guard();
CREATE TRIGGER provider_work_cut_truncate_guard BEFORE TRUNCATE ON provider_work_cut
 FOR EACH STATEMENT EXECUTE FUNCTION provider_work_original_guard();
CREATE TABLE provider_work_authority (
 domain_hash bytea NOT NULL CHECK (octet_length(domain_hash)=32),
 epoch numeric(20,0) NOT NULL CHECK (epoch BETWEEN 0 AND 18446744073709551615),
 authority_hash bytea NOT NULL UNIQUE CHECK (octet_length(authority_hash)=32),
 original bytea NOT NULL CHECK (octet_length(original) BETWEEN 1 AND 2097152),
 retained_at timestamp NOT NULL DEFAULT timezone('utc',now()),
 PRIMARY KEY(domain_hash,epoch)
);
CREATE TRIGGER provider_work_authority_guard BEFORE UPDATE OR DELETE ON provider_work_authority
 FOR EACH ROW EXECUTE FUNCTION provider_work_original_guard();
CREATE TRIGGER provider_work_authority_truncate_guard BEFORE TRUNCATE ON provider_work_authority
 FOR EACH STATEMENT EXECUTE FUNCTION provider_work_original_guard();
CREATE TABLE provider_work_window (
 artifact_hash bytea NOT NULL CHECK (octet_length(artifact_hash)=32),
 authority_hash bytea NOT NULL REFERENCES provider_work_authority(authority_hash),
 original bytea NOT NULL CHECK (octet_length(original) BETWEEN 1 AND 8388608),
 retained_at timestamp NOT NULL DEFAULT timezone('utc',now()),
 PRIMARY KEY(artifact_hash,authority_hash)
);
CREATE TRIGGER provider_work_window_guard BEFORE UPDATE OR DELETE ON provider_work_window
 FOR EACH ROW EXECUTE FUNCTION provider_work_original_guard();
CREATE TRIGGER provider_work_window_truncate_guard BEFORE TRUNCATE ON provider_work_window
 FOR EACH STATEMENT EXECUTE FUNCTION provider_work_original_guard();
`
