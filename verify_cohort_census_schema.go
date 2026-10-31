package server

// The verify cohort's durable census (model/verify_cohort_rotation.go): every
// provider a deployment ever admitted, keyed by deployment and client. The
// migration list's text, unchanged, so its published identity stays the same.
const verifyCohortCensusSchemaSql = `
		CREATE TABLE verify_cohort_census (
			deployment_key varchar(96) NOT NULL,
			client_id uuid NOT NULL,
			admitted_at timestamp NOT NULL,

			PRIMARY KEY (deployment_key, client_id)
		)
	`

// The catalog monitor follows this immutable DDL wherever merges place it.
func VerifyCohortCensusSchemaMigrationVersion() int {
	return sqlMigrationVersion(verifyCohortCensusSchemaSql)
}
