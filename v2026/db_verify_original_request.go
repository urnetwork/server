// Exact signed-request lookup survives a lost first reply without inventing a
// complete verifier population or rewriting historical original receipts.
package server

// This is appended after the full original-consent migration prefix. The
// bounded expression index also finds retained pre-index originals; duplicates
// remain a conflict instead of selecting an arbitrary historical winner.
const verifyOriginalRequestSchemaSql = verifyOriginalRequestIndexBodySql + `
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
 SELECT trail_id,previous_depth,(body->'trail'->>'ClientId')::uuid,
 COALESCE(body->'scope','null'::jsonb),
 decode(body->>'request_message','base64'),decode(body->>'request_signature','base64')
 FROM verify_original_transition
 CROSS JOIN LATERAL (SELECT verify_original_request_index_body(original_body) AS body) AS original_index;
` + verifyOriginalRequestCaptureSql + `
CREATE TRIGGER verify_original_request_capture AFTER INSERT ON verify_original_transition
 FOR EACH ROW EXECUTE FUNCTION verify_original_request_capture();
CREATE TRIGGER verify_original_request_lookup_append_only BEFORE UPDATE OR DELETE ON verify_original_request_lookup
 FOR EACH ROW EXECUTE FUNCTION verify_original_append_only_guard();
CREATE TRIGGER verify_original_request_lookup_no_truncate BEFORE TRUNCATE ON verify_original_request_lookup
 FOR EACH STATEMENT EXECUTE FUNCTION verify_original_append_only_guard();
`

// The repaired reader is shared by initial installation and the append-only upgrade.
const verifyOriginalRequestCaptureSql = `
CREATE OR REPLACE FUNCTION verify_original_request_capture() RETURNS trigger LANGUAGE plpgsql AS $request_capture$
DECLARE body jsonb;
BEGIN
 body := verify_original_request_index_body(NEW.original_body);
 INSERT INTO verify_original_request_lookup VALUES
 (NEW.trail_id,NEW.previous_depth,(body->'trail'->>'ClientId')::uuid,
 COALESCE(body->'scope','null'::jsonb),decode(body->>'request_message','base64'),decode(body->>'request_signature','base64'));
 RETURN NEW;
END
$request_capture$;
`
