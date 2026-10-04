// Exact signed-request lookup survives a lost first reply without inventing a
// complete verifier population or rewriting historical original receipts.
package server

// This is appended after the full original-consent migration prefix. The
// bounded expression index also finds retained pre-index originals; duplicates
// remain a conflict instead of selecting an arbitrary historical winner.
const verifyOriginalRequestSchemaSql = `
CREATE TABLE verify_original_request (
 request_hash bytea PRIMARY KEY CHECK (octet_length(request_hash)=32),
 trail_id uuid NOT NULL,
 previous_depth int NOT NULL,
 FOREIGN KEY (trail_id,previous_depth) REFERENCES verify_original_transition(trail_id,previous_depth)
);
CREATE TRIGGER verify_original_request_append_only BEFORE UPDATE OR DELETE ON verify_original_request
 FOR EACH ROW EXECUTE FUNCTION verify_original_append_only_guard();
CREATE TRIGGER verify_original_request_no_truncate BEFORE TRUNCATE ON verify_original_request
 FOR EACH STATEMENT EXECUTE FUNCTION verify_original_append_only_guard();
CREATE TABLE verify_original_request_lookup (
 trail_id uuid NOT NULL,
 previous_depth int NOT NULL,
 client_id uuid NOT NULL,
 scope_json jsonb NOT NULL,
 request_message bytea NOT NULL CHECK (octet_length(request_message) BETWEEN 1 AND 2048),
 request_signature bytea NOT NULL CHECK (octet_length(request_signature)=64),
 PRIMARY KEY (trail_id,previous_depth),
 FOREIGN KEY (trail_id,previous_depth) REFERENCES verify_original_transition(trail_id,previous_depth)
);
CREATE INDEX verify_original_request_lookup_identity ON verify_original_request_lookup
 (client_id,sha256(request_message),sha256(request_signature));
INSERT INTO verify_original_request_lookup
 SELECT trail_id,previous_depth,((convert_from(original_body,'UTF8')::jsonb)->'trail'->>'client_id')::uuid,
 COALESCE((convert_from(original_body,'UTF8')::jsonb)->'scope','null'::jsonb),
 decode((convert_from(original_body,'UTF8')::jsonb)->>'request_message','base64'),
 decode((convert_from(original_body,'UTF8')::jsonb)->>'request_signature','base64')
 FROM verify_original_transition;
CREATE FUNCTION verify_original_request_capture() RETURNS trigger LANGUAGE plpgsql AS $request_capture$
DECLARE body jsonb;
BEGIN
 body := convert_from(NEW.original_body,'UTF8')::jsonb;
 INSERT INTO verify_original_request_lookup VALUES
 (NEW.trail_id,NEW.previous_depth,(body->'trail'->>'client_id')::uuid,
 COALESCE(body->'scope','null'::jsonb),decode(body->>'request_message','base64'),decode(body->>'request_signature','base64'));
 RETURN NEW;
END
$request_capture$;
CREATE TRIGGER verify_original_request_capture AFTER INSERT ON verify_original_transition
 FOR EACH ROW EXECUTE FUNCTION verify_original_request_capture();
CREATE TRIGGER verify_original_request_lookup_append_only BEFORE UPDATE OR DELETE ON verify_original_request_lookup
 FOR EACH ROW EXECUTE FUNCTION verify_original_append_only_guard();
CREATE TRIGGER verify_original_request_lookup_no_truncate BEFORE TRUNCATE ON verify_original_request_lookup
 FOR EACH STATEMENT EXECUTE FUNCTION verify_original_append_only_guard();
`
