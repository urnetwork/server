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
