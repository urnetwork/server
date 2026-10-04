// Original signatures are immutable companions of the same accepted increment.
// Nullable fields preserve historical unsigned reports without inventing evidence.
package server

// Extend rather than rewrite the report migration; old trigger custody applies
// to these columns too. No foreign key to mutable key or client heads is needed.
const contractCloseOriginalSchemaSql = `
ALTER TABLE contract_close_report
 ADD COLUMN original_report bytea NULL CHECK (original_report IS NULL OR octet_length(original_report) BETWEEN 1 AND 1024),
 ADD COLUMN original_key_registration bytea NULL CHECK (original_key_registration IS NULL OR octet_length(original_key_registration) BETWEEN 1 AND 8192),
 ADD COLUMN original_key_issue varchar(64) NOT NULL DEFAULT '' CHECK (original_key_issue IN ('','history_not_found','history_capacity','history_read_unavailable')),
 ADD CONSTRAINT contract_close_original_issue_requires_report CHECK (original_key_issue = '' OR (original_report IS NOT NULL AND original_key_registration IS NULL)),
 ADD CONSTRAINT contract_close_original_key_requires_report CHECK (original_key_registration IS NULL OR original_report IS NOT NULL);
CREATE INDEX contract_close_original_census ON contract_close_report(contract_id,client_id,report_id);
`
