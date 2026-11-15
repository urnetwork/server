// Recovery metadata shares the immutable original's transaction and row.
package server

// The public signed receipt grammar stays unchanged. Null means a historical
// seed retained no window; no epoch is inferred or backfilled for old rows.
// The existing append-only trigger also protects this added column.
const verifyOriginalWindowSchemaSql = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE verify_original_transition
	ADD COLUMN head_window varchar(32) NULL;
`
