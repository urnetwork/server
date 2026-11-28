package server

// Constant defaults preserve old rows without a backfill. The generation
// handshake requires both scheduling and finishing workers to use the new code.
const taskRunOnceGenerationSchemaSql = `
	SET LOCAL lock_timeout = '5s';
	ALTER TABLE pending_task
		ADD COLUMN run_once_generation bigint NOT NULL DEFAULT 0,
		ADD COLUMN run_once_wake_at timestamp NULL,
		ADD COLUMN claim_generation bigint NOT NULL DEFAULT 0;
`
