// Stable close reports retain their authenticated owner and exact original content.
// They outlive billing cleanup; legacy empty-id reports keep their existing path.
package server

// No foreign key permits retention after the contract or client directory retires.
// The owner/report namespace prevents cross-client replay while allowing equal work.
const contractCloseReportEvidenceSchemaSql = `
CREATE TABLE contract_close_report_evidence (
 client_id uuid NOT NULL,
 report_id uuid NOT NULL,
 contract_id uuid NOT NULL,
 party varchar(16) NOT NULL CHECK (party IN ('source','destination')),
 acked_byte_count bigint NOT NULL CHECK (acked_byte_count >= 0),
 unacked_byte_count numeric(20,0) NOT NULL CHECK (unacked_byte_count >= 0 AND unacked_byte_count <= 18446744073709551615),
 checkpoint boolean NOT NULL,
 accepted_at timestamp NOT NULL,
 PRIMARY KEY (client_id, report_id)
);
CREATE INDEX contract_close_report_evidence_contract ON contract_close_report_evidence(contract_id,party,accepted_at,report_id);
CREATE FUNCTION contract_close_report_evidence_guard() RETURNS trigger LANGUAGE plpgsql AS $close_report_guard$
BEGIN RAISE EXCEPTION 'original contract close reports are immutable'; END $close_report_guard$;
CREATE TRIGGER contract_close_report_evidence_guard BEFORE UPDATE OR DELETE ON contract_close_report_evidence
 FOR EACH ROW EXECUTE FUNCTION contract_close_report_evidence_guard();
CREATE TRIGGER contract_close_report_evidence_truncate_guard BEFORE TRUNCATE ON contract_close_report_evidence
 FOR EACH STATEMENT EXECUTE FUNCTION contract_close_report_evidence_guard();
`

// The contract retention task (CONTRACT-PARTITION.md) deletes evidence older
// than the 13-week contract horizon, and only from a transaction that sets
// urnetwork.close_evidence_retention. 91 days is that horizon
// (ContractRetention); a newer row, an update and a truncate stay refused.
const contractCloseReportEvidenceRetentionGuardSql = `
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE FUNCTION contract_close_report_evidence_guard() RETURNS trigger LANGUAGE plpgsql AS $close_report_guard$
BEGIN
 IF TG_OP = 'DELETE' AND current_setting('urnetwork.close_evidence_retention', true) = 'on' THEN
  IF OLD.accepted_at < (now() AT TIME ZONE 'UTC') - interval '91 days' THEN
   RETURN OLD;
  END IF;
 END IF;
 RAISE EXCEPTION 'original contract close reports are immutable';
END $close_report_guard$;
`

// Weekly partitions of the evidence (PARTITION-MAINTENANCE-PLAN.md C5) move
// rows between partitions twice: the cutover moves rows minted past its week
// out of the plain table into the staged partitioned table, and creating a
// week moves that week's rows out of the DEFAULT partition into it. A move is
// not a retention delete, so it has its own value of the same opt-in: with
// urnetwork.close_evidence_retention = 'move' the guard admits deleting a row
// only once an identical copy is where the row moves to. From the plain table
// that is the staged parent of its cutover; from the DEFAULT partition, the
// weekly partition of its report id's week (report ids lead with their
// creation millisecond, and weekly partitions are named for their Monday).
// Retention deletes ('on') keep the 91-day rule; updates and truncates stay
// refused.
const contractCloseReportEvidenceMoveGuardSql = `
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE FUNCTION contract_close_report_evidence_guard() RETURNS trigger LANGUAGE plpgsql AS $close_report_guard$
DECLARE
 destination text;
 moved boolean := false;
BEGIN
 IF TG_OP = 'DELETE' AND current_setting('urnetwork.close_evidence_retention', true) = 'on' THEN
  IF OLD.accepted_at < (now() AT TIME ZONE 'UTC') - interval '91 days' THEN
   RETURN OLD;
  END IF;
 ELSIF TG_OP = 'DELETE' AND current_setting('urnetwork.close_evidence_retention', true) = 'move' AND TG_TABLE_SCHEMA = 'public' THEN
  IF TG_TABLE_NAME = 'contract_close_report_evidence' THEN
   destination := 'contract_close_report_evidence_p';
  ELSIF TG_TABLE_NAME = 'contract_close_report_evidence_default' THEN
   destination := 'contract_close_report_evidence_w' || to_char(date_trunc('week', timestamp '1970-01-01 00:00:00' + ('x' || substr(replace(OLD.report_id::text, '-', ''), 1, 12))::bit(48)::bigint * interval '1 millisecond'), 'YYYYMMDD');
  END IF;
  IF destination IS NOT NULL AND to_regclass('public.' || quote_ident(destination)) IS NOT NULL THEN
   EXECUTE format('SELECT EXISTS (SELECT 1 FROM public.%I moved_row WHERE moved_row.client_id = $1 AND moved_row.report_id = $2 AND moved_row = $3)', destination)
    INTO moved USING OLD.client_id, OLD.report_id, OLD;
   IF moved THEN
    RETURN OLD;
   END IF;
  END IF;
 END IF;
 RAISE EXCEPTION 'original contract close reports are immutable';
END $close_report_guard$;
`
