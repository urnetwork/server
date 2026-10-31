// The verify cohort census's migration (server verify_cohort_census_schema.go):
// the durable census of every provider a deployment ever admitted, read when
// stale members rotate out of the lifetime cohort.
package monitor

import "github.com/urnetwork/server"

// A durable ordinary table keyed by deployment and client, with three NOT NULL
// columns and no defaults. The migration publishes no check, and no index
// except the key's, which the constraint requires valid and ready.
var verifyCohortCensusMigrationContract = migrationCurrentSuffix(server.VerifyCohortCensusSchemaMigrationVersion(), "verify cohort census of admitted providers",
	migrationFp2Table("verify_cohort_census"),
	snMainnetMigrationColumns("verify_cohort_census",
		snMainnetMigrationColumn{name: "deployment_key", kind: "character varying(96)", notNull: true},
		snMainnetMigrationColumn{name: "client_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "admitted_at", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("verify_cohort_census", "PRIMARY KEY (deployment_key, client_id)"))
