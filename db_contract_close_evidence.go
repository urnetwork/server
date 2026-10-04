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
