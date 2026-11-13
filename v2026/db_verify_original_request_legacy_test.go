// Frozen pre-repair SQL makes already-applied migration upgrade fixtures exact.
package server

// Exact historical SQL, including its durable migration identity.
const testLegacyVerifyOriginalRequestSql = `
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
 SELECT trail_id,previous_depth,((convert_from(original_body,'UTF8')::jsonb)->'trail'->>'ClientId')::uuid,
 COALESCE((convert_from(original_body,'UTF8')::jsonb)->'scope','null'::jsonb),
 decode((convert_from(original_body,'UTF8')::jsonb)->>'request_message','base64'),
 decode((convert_from(original_body,'UTF8')::jsonb)->>'request_signature','base64')
 FROM verify_original_transition;
CREATE FUNCTION verify_original_request_capture() RETURNS trigger LANGUAGE plpgsql AS $request_capture$
DECLARE body jsonb;
BEGIN
 body := convert_from(NEW.original_body,'UTF8')::jsonb;
 INSERT INTO verify_original_request_lookup VALUES
 (NEW.trail_id,NEW.previous_depth,(body->'trail'->>'ClientId')::uuid,
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

// Exact historical SQL, including its durable migration identity.
const testLegacyVerifyRequestClosureSql = `
CREATE TABLE verify_original_request_closed (
 request_hash bytea PRIMARY KEY CHECK (octet_length(request_hash)=32),
 client_id uuid NOT NULL,
 scope_json jsonb NOT NULL,
 request_message bytea NOT NULL CHECK (octet_length(request_message) BETWEEN 1 AND 2048),
 request_signature bytea NOT NULL CHECK (octet_length(request_signature)=64),
 closure_original bytea NOT NULL CHECK (octet_length(closure_original) BETWEEN 1 AND 8192),
 receipt_body bytea NOT NULL CHECK (octet_length(receipt_body) BETWEEN 1 AND 4096),
 receipt_signature bytea NOT NULL CHECK (octet_length(receipt_signature)=64)
);
CREATE INDEX verify_original_request_closed_identity ON verify_original_request_closed
 (client_id,sha256(request_message),sha256(request_signature));
CREATE FUNCTION verify_original_request_wire_lock(message bytea, signature bytea) RETURNS void
 LANGUAGE sql AS $wire_lock$
 SELECT pg_advisory_xact_lock(775,('x'||substr(encode(sha256(message||signature),'hex'),1,8))::bit(32)::int);
$wire_lock$;
CREATE FUNCTION verify_original_request_closed_fence() RETURNS trigger LANGUAGE plpgsql AS $closed_fence$
DECLARE body jsonb; message bytea; signature bytea;
BEGIN
 body := convert_from(NEW.original_body,'UTF8')::jsonb;
 message := decode(body->>'request_message','base64');
 signature := decode(body->>'request_signature','base64');
 PERFORM verify_original_request_wire_lock(message,signature);
 IF EXISTS (SELECT 1 FROM verify_original_request_closed c
  WHERE c.client_id=(body->'trail'->>'ClientId')::uuid
  AND c.scope_json=COALESCE(body->'scope','null'::jsonb)
  AND c.request_message=message AND c.request_signature=signature) THEN
  RAISE EXCEPTION 'verification request permanently closed' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END
$closed_fence$;
CREATE TRIGGER verify_original_request_closed_fence BEFORE INSERT ON verify_original_transition
 FOR EACH ROW EXECUTE FUNCTION verify_original_request_closed_fence();
CREATE FUNCTION verify_original_request_no_received_tombstone() RETURNS trigger LANGUAGE plpgsql AS $no_received$
BEGIN
 PERFORM verify_original_request_wire_lock(NEW.request_message,NEW.request_signature);
 IF EXISTS (SELECT 1 FROM verify_original_request_lookup r
  WHERE r.client_id=NEW.client_id AND r.scope_json=NEW.scope_json
  AND r.request_message=NEW.request_message AND r.request_signature=NEW.request_signature) THEN
  RAISE EXCEPTION 'verification request already received' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END
$no_received$;
CREATE TRIGGER verify_original_request_no_received_tombstone BEFORE INSERT ON verify_original_request_closed
 FOR EACH ROW EXECUTE FUNCTION verify_original_request_no_received_tombstone();
CREATE TRIGGER verify_original_request_closed_append_only BEFORE UPDATE OR DELETE ON verify_original_request_closed
 FOR EACH ROW EXECUTE FUNCTION verify_original_append_only_guard();
CREATE TRIGGER verify_original_request_closed_no_truncate BEFORE TRUNCATE ON verify_original_request_closed
 FOR EACH STATEMENT EXECUTE FUNCTION verify_original_append_only_guard();
`
