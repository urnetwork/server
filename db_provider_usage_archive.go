// Billing retention archives the existing subnet proof in the same transaction
// that deletes its contract. Already deleted history is never reconstructed.
package server

// The only accepted append is an exact copy of an existing terminal contract.
// Update, delete and truncate cannot turn retained proof into different credit.
// Migration admission shares the exact body to detect an altered guard.
const ProviderUsageArchiveGuardFunctionBodySql = `
BEGIN
	IF TG_OP <> 'INSERT' THEN
		RAISE EXCEPTION 'archived provider usage is append-only';
	END IF;
	IF NOT EXISTS (
		SELECT 1 FROM transfer_contract AS source
		WHERE source.contract_id = NEW.contract_id
			AND source.outcome = NEW.outcome
			AND source.close_time IS NOT DISTINCT FROM NEW.close_time
			AND source.provider_usage IS NOT DISTINCT FROM NEW.provider_usage
	) THEN
		RAISE EXCEPTION 'provider usage archive differs from its terminal source';
	END IF;
	RETURN NEW;
END
`

// Copy the exact current reader surface, including historical missing proof.
// Capture failure aborts deletion; rollback removes the copy with the delete.
// A repeated delete sees no source row and cannot duplicate its accounting.
const ProviderUsageArchiveCaptureFunctionBodySql = `
BEGIN
	IF OLD.outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination') THEN
		INSERT INTO st_provider_usage_archive (contract_id,outcome,close_time,provider_usage)
		VALUES (OLD.contract_id,OLD.outcome,OLD.close_time,OLD.provider_usage);
	END IF;
	RETURN OLD;
END
`

// No backfill runs during this migration. Retained contracts remain the live
// source until deletion, and earlier missing/deleted history gains no credit.
const providerUsageArchiveSchemaSql = `
	CREATE TABLE st_provider_usage_archive (
		contract_id uuid PRIMARY KEY,
		outcome varchar NOT NULL CHECK (outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')),
		close_time timestamp NULL,
		provider_usage jsonb NULL
	);
	CREATE INDEX st_provider_usage_archive_close_time ON st_provider_usage_archive (close_time,contract_id);
	CREATE FUNCTION st_provider_usage_archive_guard()
	RETURNS trigger LANGUAGE plpgsql AS $archive_guard$` + ProviderUsageArchiveGuardFunctionBodySql + `$archive_guard$;
	CREATE TRIGGER st_provider_usage_archive_guard
	BEFORE INSERT OR UPDATE OR DELETE ON st_provider_usage_archive
	FOR EACH ROW EXECUTE FUNCTION st_provider_usage_archive_guard();
	CREATE TRIGGER st_provider_usage_archive_truncate_guard
	BEFORE TRUNCATE ON st_provider_usage_archive
	FOR EACH STATEMENT EXECUTE FUNCTION st_provider_usage_archive_guard();
	CREATE FUNCTION transfer_contract_usage_archive_capture()
	RETURNS trigger LANGUAGE plpgsql AS $archive_capture$` + ProviderUsageArchiveCaptureFunctionBodySql + `$archive_capture$;
	CREATE TRIGGER transfer_contract_usage_archive_capture
	BEFORE DELETE ON transfer_contract
	FOR EACH ROW EXECUTE FUNCTION transfer_contract_usage_archive_capture();
	CREATE TRIGGER transfer_contract_usage_archive_truncate_guard
	BEFORE TRUNCATE ON transfer_contract
	FOR EACH STATEMENT EXECUTE FUNCTION st_provider_usage_archive_guard();
`
