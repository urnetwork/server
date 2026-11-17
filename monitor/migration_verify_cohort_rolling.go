// The verify cohort v2's rolling census migration (server
// verify_cohort_rolling_schema.go, sn VERIFY-COHORT-V2.md): the reported census
// columns of the head summaries, each cohort member's last epoch, and the table
// that records when the rolling census started.
package monitor

import "github.com/urnetwork/server"

// Nullable bigint columns with no defaults, and one durable ordinary table
// keyed by deployment with two NOT NULL columns and no defaults. Follow the
// migration's position after merges instead of pinning a number.
var verifyCohortRollingMigrationContract = migrationCurrentSuffix(
	server.VerifyCohortRollingSchemaMigrationVersion(),
	"verify cohort rolling census",
	snMainnetMigrationColumns("sn_head_summary",
		snMainnetMigrationColumn{name: "census_providers", kind: "bigint"},
		snMainnetMigrationColumn{name: "bound_clients", kind: "bigint"},
		snMainnetMigrationColumn{name: "bound_clients_observed", kind: "bigint"}),
	snMainnetMigrationColumns("verify_cohort_census",
		snMainnetMigrationColumn{name: "last_epoch", kind: "bigint"}),
	migrationFp2Table("verify_cohort_state"),
	snMainnetMigrationColumns("verify_cohort_state",
		snMainnetMigrationColumn{name: "deployment_key", kind: "character varying(96)", notNull: true},
		snMainnetMigrationColumn{name: "rolling_from_epoch", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "rolling_since", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("verify_cohort_state", "PRIMARY KEY (deployment_key)"))
