// Original verification transitions and wallet proofs survive hot-state expiry.
package server

// Append after the published migration prefix. Missing historical bytes remain
// null; none of these tables assert completeness of a verifier population.
const verifyOriginalSchemaSql = `
CREATE TABLE verify_original_transition (
 trail_id uuid NOT NULL,
 previous_depth int NOT NULL CHECK (previous_depth BETWEEN 0 AND 16),
 observed_time timestamp NOT NULL,
 original_body bytea NOT NULL CHECK (octet_length(original_body) BETWEEN 1 AND 65536),
 original_signature bytea NOT NULL CHECK (octet_length(original_signature) = 64),
 PRIMARY KEY (trail_id, previous_depth)
);
CREATE INDEX verify_original_transition_window ON verify_original_transition(observed_time, trail_id, previous_depth);
CREATE TABLE verify_original_pending (
 trail_id uuid PRIMARY KEY,
 previous_depth int NOT NULL,
 recovery_time timestamp NOT NULL
);
CREATE INDEX verify_original_pending_due ON verify_original_pending(recovery_time,trail_id);
CREATE FUNCTION verify_original_append_only_guard() RETURNS trigger LANGUAGE plpgsql AS $verify_original_guard$
BEGIN RAISE EXCEPTION 'verification originals are append-only'; END
$verify_original_guard$;
CREATE TRIGGER verify_original_append_only BEFORE UPDATE OR DELETE ON verify_original_transition
 FOR EACH ROW EXECUTE FUNCTION verify_original_append_only_guard();
ALTER TABLE verify_trail ADD COLUMN original_state bytea NULL;
CREATE TRIGGER verify_trail_original_append_only BEFORE UPDATE OR DELETE ON verify_trail
 FOR EACH ROW EXECUTE FUNCTION verify_original_append_only_guard();
ALTER TABLE st_provider_wallet_history
 ADD COLUMN original_message text NULL,
 ADD COLUMN original_signature text NULL,
 ADD CONSTRAINT st_provider_wallet_original_pair CHECK ((original_message IS NULL) = (original_signature IS NULL));
ALTER TABLE st_event ADD COLUMN original_log bytea NULL CHECK (original_log IS NULL OR octet_length(original_log) BETWEEN 1 AND 65536);
CREATE TRIGGER st_event_original_append_only BEFORE UPDATE OR DELETE ON st_event
 FOR EACH ROW EXECUTE FUNCTION verify_original_append_only_guard();
CREATE TABLE st_fleet_binding_original (
 deployment_key varchar(96) NOT NULL,
 client_id uuid NOT NULL,
 generation bigint NOT NULL,
 receipt_hash bytea NOT NULL CHECK (octet_length(receipt_hash)=32),
 original_body bytea NOT NULL CHECK (octet_length(original_body) BETWEEN 1 AND 16384),
 observed_time timestamp NOT NULL,
 PRIMARY KEY (deployment_key,client_id,generation,receipt_hash)
);
CREATE TRIGGER st_fleet_binding_original_append_only BEFORE UPDATE OR DELETE ON st_fleet_binding_original
 FOR EACH ROW EXECUTE FUNCTION verify_original_append_only_guard();
`
