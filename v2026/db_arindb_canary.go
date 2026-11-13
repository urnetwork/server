// The dual-database canary's stored facts (arindbctl/ARINDB.md, "Connection and
// rollup facts"). Each connection carries the facts of the latest and the old
// ARIN database, and each provider rollup carries both slots with their epochs.
// The additions are invisible to old binaries, and their defaults of false and
// 0 never read as clean: a slot is used only when its epoch is the published
// serving epoch, and 0 never is.
package server

// Appended after schema 836, the FP2 modern provider evidence, as two
// migrations that each lock one table. One transaction over both tables held
// ACCESS EXCLUSIVE on network_client_location while it waited for the rollup's
// lock, so every location write and the FP2 Quality guard queued behind it,
// and update_reliabilities, which reads location rows before it writes the
// rollup, could deadlock with it.
// New columns are metadata-only additions with constant defaults; nothing is
// rewritten.

// The first: the rollup's two slots and their epochs.
const arinDatabaseCanaryRollupSchemaSql = `
	SET LOCAL lock_timeout = '5s';
	ALTER TABLE network_client_location_reliability
		ADD COLUMN arin_old_risk boolean NOT NULL DEFAULT false,
		ADD COLUMN arin_old_non_quality boolean NOT NULL DEFAULT false,
		ADD COLUMN arin_epoch bigint NOT NULL DEFAULT 0,
		ADD COLUMN arin_old_epoch bigint NOT NULL DEFAULT 0;
`

// The second: each connection's old slot and its write guard. The old slot has its
// own write token and guard. A writer that predates the old slot still
// supplies a fresh latest token, so the published v751 guard cannot tell it
// apart; this guard revokes the old positive and marks the old facts
// unavailable whenever a write does not supply a fresh old token. The v751
// guard is left byte for byte as published.
const arinDatabaseCanaryLocationSchemaSql = `
	SET LOCAL lock_timeout = '5s';
	ALTER TABLE network_client_location
		ADD COLUMN arin_old_risk boolean NOT NULL DEFAULT false,
		ADD COLUMN arin_old_non_quality boolean NOT NULL DEFAULT false,
		ADD COLUMN arin_old_quality_verified boolean NOT NULL DEFAULT false,
		ADD COLUMN arin_old_database_build_epoch bigint NOT NULL DEFAULT 0,
		ADD COLUMN arin_old_quality_write_token uuid;
	CREATE FUNCTION network_client_location_arin_old_quality_guard() RETURNS trigger LANGUAGE plpgsql AS $arin_old_quality$
	BEGIN
		IF TG_OP = 'INSERT' THEN
			IF NEW.arin_old_quality_write_token IS NULL THEN
				NEW.arin_old_quality_verified := false;
				NEW.arin_old_database_build_epoch := 0;
			END IF;
		ELSIF NEW.arin_old_quality_write_token IS NULL OR
			NEW.arin_old_quality_write_token IS NOT DISTINCT FROM OLD.arin_old_quality_write_token THEN
			NEW.arin_old_quality_verified := false;
			NEW.arin_old_database_build_epoch := 0;
			NEW.arin_old_quality_write_token := OLD.arin_old_quality_write_token;
		END IF;
		RETURN NEW;
	END
	$arin_old_quality$;
	CREATE TRIGGER network_client_location_arin_old_quality_guard
		BEFORE INSERT OR UPDATE ON network_client_location
		FOR EACH ROW EXECUTE FUNCTION network_client_location_arin_old_quality_guard();
`
