// The artifact contracts of the dual-database ARIN canary's two migrations,
// part of the single migrations signal (arindbctl/ARINDB.md, "Connection and
// rollup facts"): the rollup's slots, then the connection slots and their write
// guard.
package monitor

import "strings"

// These published versions keep their order when later migrations append.
// Neither a newer source head nor a longer artifact catalog renumbers them.
const (
	arindbCanaryRollupMigrationVersion   = 837
	arindbCanaryLocationMigrationVersion = arindbCanaryRollupMigrationVersion + 1
)

// This body intentionally pins the function the location migration publishes. It does not read
// connection rows or infer whether a paired release is deployed.
const arinOldQualityGuardFunctionBody = `
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
	`

// The old-slot guard must be the published body on every insert and update; a
// disabled, narrowed or replaced guard would let a writer that predates the
// old slot keep an earlier positive old fact.
var arinOldQualityWriteGuardArtifactQuery = `(
	EXISTS (SELECT 1 FROM pg_proc AS actual
		WHERE actual.oid=to_regprocedure('public.network_client_location_arin_old_quality_guard()')
			AND actual.prorettype='trigger'::regtype AND actual.prokind='f'
			AND NOT actual.prosecdef AND actual.proconfig IS NULL
			AND actual.prosrc='` + strings.ReplaceAll(arinOldQualityGuardFunctionBody, "'", "''") + `'
			AND actual.prolang=(SELECT oid FROM pg_language WHERE lanname='plpgsql'))
	AND EXISTS (SELECT 1 FROM pg_trigger AS actual
		WHERE actual.tgrelid=to_regclass('public.network_client_location')
			AND actual.tgname='network_client_location_arin_old_quality_guard'
			AND actual.tgfoid=to_regprocedure('public.network_client_location_arin_old_quality_guard()')
			AND actual.tgtype=23 AND actual.tgenabled='O'
			AND actual.tgnargs=0 AND actual.tgqual IS NULL AND actual.tgattr=''::int2vector
			AND actual.tgoldtable IS NULL AND actual.tgnewtable IS NULL AND NOT actual.tgisinternal)
)`

// The rollup migration: the rollup's old slot and both slots' epochs, with
// their exact types, nullability and defaults.
func migrationArindbCanaryRollupArtifacts() []string {
	return []string{
		snMainnetMigrationColumns("network_client_location_reliability",
			snMainnetMigrationColumn{name: "arin_old_risk", kind: "boolean", notNull: true, defaultExpression: "false"},
			snMainnetMigrationColumn{name: "arin_old_non_quality", kind: "boolean", notNull: true, defaultExpression: "false"},
			snMainnetMigrationColumn{name: "arin_epoch", kind: "bigint", notNull: true, defaultExpression: "0"},
			snMainnetMigrationColumn{name: "arin_old_epoch", kind: "bigint", notNull: true, defaultExpression: "0"}),
	}
}

// The location migration: each connection's old slot, with its exact types,
// nullability and defaults, and the old-slot guard.
func migrationArindbCanaryLocationArtifacts() []string {
	return []string{
		snMainnetMigrationColumns("network_client_location",
			snMainnetMigrationColumn{name: "arin_old_risk", kind: "boolean", notNull: true, defaultExpression: "false"},
			snMainnetMigrationColumn{name: "arin_old_non_quality", kind: "boolean", notNull: true, defaultExpression: "false"},
			snMainnetMigrationColumn{name: "arin_old_quality_verified", kind: "boolean", notNull: true, defaultExpression: "false"},
			snMainnetMigrationColumn{name: "arin_old_database_build_epoch", kind: "bigint", notNull: true, defaultExpression: "0"},
			snMainnetMigrationColumn{name: "arin_old_quality_write_token", kind: "uuid"}),
		arinOldQualityWriteGuardArtifactQuery,
	}
}
