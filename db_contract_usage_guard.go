// Settlement cannot depend on every running operator having the latest writer.
// The database refuses missing new proof and protects admitted usage attribution.
package server

// Shared with migration admission so an altered function or disabled trigger
// cannot hide behind an unchanged numeric migration head. The Go usage decoder
// still owns full snapshot validation; this boundary owns presence and custody.
const ContractUsageGuardFunctionBodySql = `
DECLARE
	legacy jsonb;
BEGIN
	IF TG_OP = 'UPDATE' THEN
		IF OLD.provider_usage IS NOT NULL AND (
			NEW.provider_usage IS DISTINCT FROM OLD.provider_usage OR
			NEW.contract_id IS DISTINCT FROM OLD.contract_id
		) THEN
			RAISE EXCEPTION 'contract usage snapshot is immutable';
		END IF;
		IF OLD.outcome IS NOT NULL THEN
			IF NEW.contract_id IS DISTINCT FROM OLD.contract_id OR
				NEW.outcome IS DISTINCT FROM OLD.outcome OR
				NEW.close_time IS DISTINCT FROM OLD.close_time THEN
				RAISE EXCEPTION 'contract terminal usage attribution is immutable';
			END IF;
			IF OLD.provider_usage IS NULL AND NEW.provider_usage IS NOT NULL THEN
				-- Historical missing proof can only become explicit uncredited debt.
				-- The separate reviewed repair owns the exact whole-row/report census.
				legacy := NEW.provider_usage->'legacy_exclusion';
				IF OLD.outcome <> 'settled' OR OLD.close_time IS NULL OR
					(NEW.provider_usage - 'legacy_exclusion') IS DISTINCT FROM
						'{"version":1,"byte_count":0,"providers":[],"excluded_reason":"legacy_usage_unavailable"}'::jsonb OR
					jsonb_typeof(legacy) IS DISTINCT FROM 'object' OR
					(legacy - ARRAY['repair_manifest_sha256','contract_id','epoch','closed_at','retained_report_minimum','final_acceptance']) IS DISTINCT FROM '{}'::jsonb OR
					legacy->>'contract_id' IS DISTINCT FROM OLD.contract_id::text OR
					(legacy->>'closed_at')::timestamptz AT TIME ZONE 'UTC' IS DISTINCT FROM OLD.close_time OR
					legacy->'final_acceptance' IS DISTINCT FROM 'false'::jsonb OR
					NOT COALESCE(legacy->>'repair_manifest_sha256' ~ '^sha256:[0-9a-f]{64}$', false) OR
					jsonb_typeof(legacy->'epoch') IS DISTINCT FROM 'number' OR
					NOT COALESCE((legacy->>'epoch')::bigint > 0, false) OR
					jsonb_typeof(legacy->'retained_report_minimum') IS DISTINCT FROM 'number' OR
					NOT COALESCE((legacy->>'retained_report_minimum')::bigint >= 0, false) THEN
					RAISE EXCEPTION 'historical contract usage requires exact zero-credit debt';
				END IF;
			END IF;
			RETURN NEW;
		END IF;
	END IF;
	IF NEW.outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination') THEN
		IF NEW.provider_usage IS NULL OR NEW.close_time IS NULL OR
			NEW.provider_usage ? 'legacy_exclusion' THEN
			RAISE EXCEPTION 'new contract settlement requires immutable provider usage';
		END IF;
	END IF;
	RETURN NEW;
END
`

// Appended prospectively without touching historical rows or signed artifacts.
// Old writers fail their transaction instead of publishing an incomplete epoch.
const contractUsageGuardSchemaSql = `
	CREATE FUNCTION transfer_contract_usage_guard()
	RETURNS trigger LANGUAGE plpgsql AS $transfer_contract_usage_guard$` + ContractUsageGuardFunctionBodySql + `$transfer_contract_usage_guard$;
	CREATE TRIGGER transfer_contract_usage_guard
	BEFORE INSERT OR UPDATE ON transfer_contract
	FOR EACH ROW EXECUTE FUNCTION transfer_contract_usage_guard();
`
