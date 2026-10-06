// Optional chain evidence shares the existing immutable report transaction.
// Old routes omit it; no backfill or current-key lookup can manufacture history.
package server

const contractCloseInventorySchemaSql = `
ALTER TABLE contract_close_report_evidence
 ADD COLUMN original_inventory bytea NULL CHECK (original_inventory IS NULL OR octet_length(original_inventory) BETWEEN 1 AND 1024),
 ADD CONSTRAINT contract_close_inventory_requires_report CHECK (original_inventory IS NULL OR original_report IS NOT NULL);
`
