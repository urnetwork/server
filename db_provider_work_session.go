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
CREATE FUNCTION provider_work_endpoint_lock(client uuid) RETURNS void LANGUAGE sql AS $endpoint_lock$
 SELECT pg_advisory_xact_lock(776,('x'||substr(md5(client::text),1,8))::bit(32)::int);
$endpoint_lock$;
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
CREATE FUNCTION provider_work_session_statement_fence() RETURNS trigger LANGUAGE plpgsql AS $statement_fence$
BEGIN
 IF current_setting('urnetwork.provider_work_cooperating',true)='1' THEN
  PERFORM pg_advisory_xact_lock_shared(-776::bigint);
 ELSE
  IF EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid() AND granted
   AND classid=4294967295::oid AND objid=4294966520::oid AND objsubid=1 AND mode='ShareLock')
   AND NOT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid() AND granted
    AND classid=4294967295::oid AND objid=4294966520::oid AND objsubid=1 AND mode='ExclusiveLock') THEN
   RAISE EXCEPTION 'provider work mutation lacks ordered endpoint fences' USING ERRCODE='40001';
  END IF;
  PERFORM pg_advisory_xact_lock(-776::bigint);
 END IF;
 RETURN NULL;
END;
$statement_fence$;
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
