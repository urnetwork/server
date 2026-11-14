package server

// lag.dev's measurement api probes (lag.dev/PLAN.md A4;
// model/network_extender_measurement_probe_model.go): the latest probe of each
// published family of each extender the lag.dev sample serves, with the
// address it probed, its outcome, the latest success and the failures in a
// row. One row per extender and family, as network_extender_address keys its
// own. A new table: nothing is rewritten, and older binaries never read it.
// The migration list's text, unchanged, so its published identity stays the
// same.
const networkExtenderMeasurementProbeSchemaSql = `
		CREATE TABLE network_extender_measurement_probe (
			extender_id uuid NOT NULL,
			ip_version smallint NOT NULL,
			ip inet NOT NULL,
			probe_time timestamp NOT NULL,
			outcome varchar(16) NOT NULL,
			success_time timestamp NULL,
			consecutive_failures int NOT NULL DEFAULT 0,

			PRIMARY KEY (extender_id, ip_version)
		)
	`

// The catalog monitor follows this immutable DDL wherever merges place it.
func NetworkExtenderMeasurementProbeSchemaMigrationVersion() int {
	return sqlMigrationVersion(networkExtenderMeasurementProbeSchemaSql)
}
