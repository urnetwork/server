// The re-engagement results' migration (server reengagement_results_schema.go,
// mmm/reengagement/PLAN.md "MEASUREMENT"): the nightly per-cohort-day episode
// counts the experiment readout reads.
package monitor

import "github.com/urnetwork/server"

// A durable ordinary table keyed by its dimension tuple, with the counts as
// integers defaulting to 0. The migration publishes no check, and no index
// except the key's, which the constraint requires valid and ready. The default
// expressions are the catalog's own text on PostgreSQL 18.
var reengagementResultsMigrationContract = migrationCurrentSuffix(server.ReengagementResultsDailySchemaMigrationVersion(), "re-engagement results by cohort day",
	migrationFp2Table("reengagement_results_daily"),
	snMainnetMigrationColumns("reengagement_results_daily",
		snMainnetMigrationColumn{name: "cohort_day", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "experiment", kind: "character varying(64)", notNull: true},
		snMainnetMigrationColumn{name: "variant", kind: "character varying(64)", notNull: true},
		snMainnetMigrationColumn{name: "stream", kind: "character varying(32)", notNull: true},
		snMainnetMigrationColumn{name: "platform", kind: "character varying(32)", notNull: true},
		snMainnetMigrationColumn{name: "tier", kind: "character varying(32)", notNull: true},
		snMainnetMigrationColumn{name: "exposures", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "virtual_exposures", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "sent", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "delivered", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "landing_clicked", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "reason_answers", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "keeps", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "reactivation_7d", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "reactivation_30d", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "retention_90d", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "pro_restart_30d", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "unsubscribe", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "complaint", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "hard_bounce", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "matured_days", kind: "integer", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "computed_at", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("reengagement_results_daily", "PRIMARY KEY (cohort_day, experiment, variant, stream, platform, tier)"))
