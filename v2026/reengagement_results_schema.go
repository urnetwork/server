package server

// The re-engagement results (mmm/reengagement/PLAN.md "MEASUREMENT",
// model/reengagement_results_model.go): one row per cohort day (the UTC day of
// an episode's first real or virtual send), experiment, variant, stream,
// platform and price tier, with the episode counts. The nightly
// ReengagementResultsRollup rewrites the last `reengagement.results.rollup_days`
// cohort days; GET /admin/reengagement/results reads it. A new table keyed by
// its primary key only, with constant defaults.
const reengagementResultsDailySchemaSql = `
		CREATE TABLE reengagement_results_daily (
			cohort_day timestamp NOT NULL,
			experiment varchar(64) NOT NULL,
			variant varchar(64) NOT NULL,
			stream varchar(32) NOT NULL,
			platform varchar(32) NOT NULL,
			tier varchar(32) NOT NULL,
			exposures int NOT NULL DEFAULT 0,
			virtual_exposures int NOT NULL DEFAULT 0,
			sent int NOT NULL DEFAULT 0,
			delivered int NOT NULL DEFAULT 0,
			landing_clicked int NOT NULL DEFAULT 0,
			reason_answers int NOT NULL DEFAULT 0,
			keeps int NOT NULL DEFAULT 0,
			reactivation_7d int NOT NULL DEFAULT 0,
			reactivation_30d int NOT NULL DEFAULT 0,
			retention_90d int NOT NULL DEFAULT 0,
			pro_restart_30d int NOT NULL DEFAULT 0,
			unsubscribe int NOT NULL DEFAULT 0,
			complaint int NOT NULL DEFAULT 0,
			hard_bounce int NOT NULL DEFAULT 0,
			matured_days int NOT NULL DEFAULT 0,
			computed_at timestamp NOT NULL,

			PRIMARY KEY (cohort_day, experiment, variant, stream, platform, tier)
		)
	`

// The catalog monitor follows this immutable DDL wherever merges place it.
func ReengagementResultsDailySchemaMigrationVersion() int {
	return sqlMigrationVersion(reengagementResultsDailySchemaSql)
}
