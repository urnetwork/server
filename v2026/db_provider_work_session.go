// Original session mutations share a durable endpoint fence with reservations.
package server

// Migration 776 preserves unsigned gaps from rolling writers. No mutable
// connection row, cleanup, or signing-key rotation can reset a journal birth.
const providerWorkSessionSchemaSql = `
CREATE TABLE provider_work_session_head (
 client_id uuid PRIMARY KEY,
 sequence bigint NOT NULL CHECK(sequence>0)
);
CREATE TABLE provider_work_session_event (
 client_id uuid NOT NULL,
 sequence bigint NOT NULL CHECK(sequence>0),
 network_id uuid,
 connection_id uuid,
 kind text NOT NULL CHECK(kind IN ('baseline','admit','retire')),
 observed_at timestamp NOT NULL,
 extender_id uuid,
 transaction_id bigint NOT NULL,
 PRIMARY KEY(client_id,sequence)
);
CREATE INDEX provider_work_session_event_transaction ON provider_work_session_event(transaction_id,client_id,sequence);
CREATE INDEX provider_work_session_event_connection ON provider_work_session_event(client_id,connection_id,sequence);
CREATE TABLE provider_work_session_receipt (
 client_id uuid NOT NULL,
 sequence bigint NOT NULL,
 receipt_hash bytea NOT NULL UNIQUE CHECK(octet_length(receipt_hash)=32),
 original bytea NOT NULL CHECK(octet_length(original) BETWEEN 1 AND 65536),
 PRIMARY KEY(client_id,sequence),
 FOREIGN KEY(client_id,sequence) REFERENCES provider_work_session_event(client_id,sequence)
);
CREATE TABLE provider_work_reservation_original (
 contract_id uuid PRIMARY KEY,
 source_id uuid NOT NULL,
 source_sequence bigint NOT NULL,
 destination_id uuid NOT NULL,
 destination_sequence bigint NOT NULL,
 receipt_hash bytea NOT NULL UNIQUE CHECK(octet_length(receipt_hash)=32),
 original bytea NOT NULL CHECK(octet_length(original) BETWEEN 1 AND 65536)
);
CREATE TABLE provider_work_stream_original (
 stream_id uuid PRIMARY KEY,
 origin_contract_id uuid NOT NULL,
 receipt_hash bytea NOT NULL UNIQUE CHECK(octet_length(receipt_hash)=32),
 original bytea NOT NULL CHECK(octet_length(original) BETWEEN 1 AND 65536)
);
CREATE TABLE provider_work_stream_contract (
 contract_id uuid PRIMARY KEY,
 stream_id uuid NOT NULL REFERENCES provider_work_stream_original(stream_id)
);
CREATE TABLE provider_work_outcome_original (
 contract_id uuid PRIMARY KEY,
 receipt_hash bytea NOT NULL UNIQUE CHECK(octet_length(receipt_hash)=32),
 original bytea NOT NULL CHECK(octet_length(original) BETWEEN 1 AND 65536)
);
` + providerWorkSessionEndpointLockSql + `
CREATE FUNCTION provider_work_session_append(client uuid,connection uuid,event_kind text,extender uuid)
 RETURNS bigint LANGUAGE plpgsql AS $session_append$
DECLARE next_sequence bigint; owner_network uuid;
BEGIN
 PERFORM provider_work_endpoint_lock(client);
 INSERT INTO provider_work_session_head(client_id,sequence) VALUES(client,1)
 ON CONFLICT(client_id) DO UPDATE SET sequence=provider_work_session_head.sequence+1
 RETURNING sequence INTO next_sequence;
 IF event_kind='retire' THEN
  SELECT network_id INTO owner_network FROM provider_work_session_event
   WHERE client_id=client AND connection_id=connection AND kind='admit' ORDER BY sequence DESC LIMIT 1;
 ELSE
  SELECT network_id INTO owner_network FROM network_client WHERE client_id=client;
 END IF;
 INSERT INTO provider_work_session_event(client_id,sequence,network_id,connection_id,kind,observed_at,extender_id,transaction_id)
 VALUES(client,next_sequence,owner_network,connection,event_kind,clock_timestamp() AT TIME ZONE 'UTC',extender,txid_current());
 RETURN next_sequence;
END;
$session_append$;
CREATE FUNCTION provider_work_session_mutation() RETURNS trigger LANGUAGE plpgsql AS $session_mutation$
BEGIN
 IF TG_OP='UPDATE' AND ROW(OLD.client_id,OLD.connection_id,OLD.connected,OLD.extender_id)
  IS NOT DISTINCT FROM ROW(NEW.client_id,NEW.connection_id,NEW.connected,NEW.extender_id) THEN RETURN NEW; END IF;
 IF TG_OP<>'INSERT' AND OLD.connected THEN
  PERFORM provider_work_session_append(OLD.client_id,OLD.connection_id,'retire',NULL);
 END IF;
 IF TG_OP<>'DELETE' AND NEW.connected THEN
  PERFORM provider_work_session_append(NEW.client_id,NEW.connection_id,'admit',NEW.extender_id);
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END;
$session_mutation$;
` + providerWorkSessionStatementFenceSql + `
CREATE TRIGGER provider_work_session_statement_fence BEFORE INSERT OR UPDATE OR DELETE ON network_client_connection
 FOR EACH STATEMENT EXECUTE FUNCTION provider_work_session_statement_fence();
CREATE TRIGGER provider_work_session_mutation AFTER INSERT OR UPDATE OR DELETE ON network_client_connection
 FOR EACH ROW EXECUTE FUNCTION provider_work_session_mutation();
CREATE FUNCTION provider_work_session_head_guard() RETURNS trigger LANGUAGE plpgsql AS $head_guard$
BEGIN
 IF TG_OP='UPDATE' AND NEW.client_id=OLD.client_id AND NEW.sequence=OLD.sequence+1 THEN RETURN NEW; END IF;
 RAISE EXCEPTION 'provider work journal birth cannot be reset';
END;
$head_guard$;
CREATE TRIGGER provider_work_session_head_guard BEFORE UPDATE OR DELETE ON provider_work_session_head
 FOR EACH ROW EXECUTE FUNCTION provider_work_session_head_guard();
CREATE TRIGGER provider_work_session_head_truncate_guard BEFORE TRUNCATE ON provider_work_session_head
 FOR EACH STATEMENT EXECUTE FUNCTION provider_work_original_guard();
DO $guards$ DECLARE table_name text; BEGIN
 FOREACH table_name IN ARRAY ARRAY['provider_work_session_event','provider_work_session_receipt',
 'provider_work_reservation_original','provider_work_stream_original','provider_work_stream_contract','provider_work_outcome_original'] LOOP
  EXECUTE format('CREATE TRIGGER original_guard BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION provider_work_original_guard()',table_name);
  EXECUTE format('CREATE TRIGGER original_truncate_guard BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION provider_work_original_guard()',table_name);
 END LOOP;
END; $guards$;
`

// A rolling writer can already hold a connection row when its row trigger
// reaches the endpoint fence. Refuse that same-client conflict without waiting:
// the whole transaction rolls back and the existing bounded 40001 retry starts
// with no row locks. No mutation or journal event is omitted. Cleanup may finish
// on a later retry; no global exclusive fence serializes unrelated clients.
const providerWorkSessionEndpointLockSql = `
CREATE OR REPLACE FUNCTION provider_work_endpoint_lock(client uuid) RETURNS void LANGUAGE plpgsql AS $endpoint_lock$
BEGIN
 IF NOT pg_try_advisory_xact_lock(776,('x'||substr(md5(client::text),1,8))::bit(32)::int) THEN
  RAISE EXCEPTION 'provider work endpoint is busy' USING ERRCODE='40001';
 END IF;
END;
$endpoint_lock$;
`

// Retain the historical trigger binding while removing its global bridge.
// Existing binaries may still take the shared bridge, which has no conflicting
// holder after this repair. No-op connection updates need no endpoint fence.
const providerWorkSessionStatementFenceSql = `
CREATE OR REPLACE FUNCTION provider_work_session_statement_fence() RETURNS trigger LANGUAGE plpgsql AS $statement_fence$
BEGIN
 RETURN NULL;
END;
$statement_fence$;
`

// Repair already-installed v776 atomically without rewriting retained evidence
// or acquiring a table-wide connection lock. Fresh v776 uses the same functions
// so an upgrade cannot introduce the global mutex before reaching this repair.
// Transactions already executing the old function retain their locks until end.
const providerWorkSessionContentionRepairSql = `
SET LOCAL lock_timeout = '5s';
` + providerWorkSessionEndpointLockSql + providerWorkSessionStatementFenceSql
