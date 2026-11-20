package server

// The verify cohort v2's rolling census (sn VERIFY-COHORT-V2.md §2.4-2.6;
// model/verify_cohort_rolling.go), in one additive migration:
//   - sn_head_summary keeps what a validator reports with each summary: its
//     census (census_providers), the recipient cut's eligible bindings
//     (bound_clients) and how many of them served a verified hop that tempo
//     (bound_clients_observed). A summary from a validator that predates them,
//     and every row stored before this migration, reads null.
//   - verify_cohort_census keeps each member's last settlement epoch with an
//     assignment (last_epoch). A legacy row, written before the rolling census
//     started, reads null.
//   - verify_cohort_state records, once per deployment, the validator epoch the
//     rolling census started from. Head summaries are kept only 7 days, so the
//     switch can't be read back from them later.
//
// Nullable columns with no default and a new table: nothing existing is
// rewritten, and the running release, which names the columns it writes, writes
// on. The short lock wait keeps the alters from queuing behind the summary
// ingest or the cohort's write-through; one that can't take its table within it
// fails and is run again.
const verifyCohortRollingSchemaSql = `
		SET LOCAL lock_timeout = '2s';
		ALTER TABLE sn_head_summary
			ADD COLUMN census_providers bigint NULL,
			ADD COLUMN bound_clients bigint NULL,
			ADD COLUMN bound_clients_observed bigint NULL;
		ALTER TABLE verify_cohort_census
			ADD COLUMN last_epoch bigint NULL;
		CREATE TABLE verify_cohort_state (
			deployment_key varchar(96) NOT NULL,
			rolling_from_epoch bigint NOT NULL,
			rolling_since timestamp NOT NULL,

			PRIMARY KEY (deployment_key)
		);
	`

// The catalog monitor follows this immutable DDL wherever merges place it.
func VerifyCohortRollingSchemaMigrationVersion() int {
	return sqlMigrationVersion(verifyCohortRollingSchemaSql)
}
