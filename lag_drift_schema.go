package server

// The lag.dev drift test's schema (lag.dev/DRIFTTEST.md;
// model/lag_drift_model.go, model/lag_drift_calibration_model.go), four
// migrations in db_migrations.go, each described there: the drift test's
// tables with lag_track's kind and drift columns, the drift model's
// calibration, the country of a drift test's client_ip, and a drift run's k
// and k_early. The texts are those migrations' exact DDL, named so the catalog
// monitor follows each to its version wherever merges place it.
const lagDriftSchemaSql = `
		SET LOCAL lock_timeout = '5s';
		ALTER TABLE lag_track
			ADD COLUMN IF NOT EXISTS kind varchar(8) NOT NULL DEFAULT 'lag',
			ADD COLUMN IF NOT EXISTS drift_test_id uuid NULL,
			ADD COLUMN IF NOT EXISTS drift_run smallint NULL;

		CREATE TABLE IF NOT EXISTS lag_drift_test (
			drift_test_id uuid NOT NULL PRIMARY KEY,
			network_id uuid NOT NULL,
			token_hash bytea NOT NULL,
			client_ip_hash bytea NULL,
			client_ip_family smallint NOT NULL,
			parent_origins varchar(256)[] NULL,
			confidence double precision NOT NULL,
			reference varchar(128) NULL,
			created_at timestamp NOT NULL,
			expires_at timestamp NOT NULL,
			first_started_at timestamp NULL,
			retest_until timestamp NULL,
			run_count smallint NOT NULL DEFAULT 0,
			updated_at timestamp NOT NULL,
			failed_at timestamp NULL,
			revoke_time timestamp NULL,
			result_json jsonb NULL,
			result_at timestamp NULL,
			CONSTRAINT lag_drift_test_token_hash UNIQUE (token_hash)
		);
		CREATE INDEX IF NOT EXISTS lag_drift_test_network_id_created_at
		ON lag_drift_test (network_id, created_at);
		CREATE INDEX IF NOT EXISTS lag_drift_test_created_at
		ON lag_drift_test (created_at, drift_test_id);
		CREATE INDEX IF NOT EXISTS lag_drift_test_client_ip_hash_created_at
		ON lag_drift_test (created_at)
		WHERE client_ip_hash IS NOT NULL;

		CREATE TABLE IF NOT EXISTS lag_drift_run (
			drift_test_id uuid NOT NULL,
			run smallint NOT NULL,
			track_id uuid NOT NULL,
			state varchar(16) NOT NULL,
			started_at timestamp NOT NULL,
			finished_at timestamp NULL,
			completed_at timestamp NULL,
			stop varchar(16) NULL,
			ip_family smallint NOT NULL,
			client_ip_matches bool NULL,
			ip_country_code varchar(2) NULL,
			ip_country_name varchar(256) NULL,
			extenders_json jsonb NOT NULL,
			outcome varchar(32) NULL,
			supported_stop varchar(16) NULL,
			answered smallint NULL,
			usable smallint NULL,
			measured_ms int NULL,
			PRIMARY KEY (drift_test_id, run),
			CONSTRAINT lag_drift_run_track_id UNIQUE (track_id)
		);
		CREATE INDEX IF NOT EXISTS lag_drift_run_open_started_at
		ON lag_drift_run (started_at)
		WHERE state <> 'complete';

		CREATE TABLE IF NOT EXISTS lag_drift_network (
			network_id uuid NOT NULL PRIMARY KEY,
			parent_origins varchar(256)[] NULL,
			disable_time timestamp NULL,
			update_time timestamp NOT NULL
		);
	`

const lagDriftCalibrationSchemaSql = `
		CREATE TABLE IF NOT EXISTS lag_drift_calibration (
			calibration_id uuid NOT NULL PRIMARY KEY,
			calibrated_at timestamp NOT NULL,
			window_start timestamp NOT NULL,
			window_end timestamp NOT NULL,
			fit_tracks int NOT NULL,
			fit_observations int NOT NULL,
			kappa double precision NOT NULL,
			sigma0_ms double precision NOT NULL,
			relay_threshold_ms double precision NOT NULL,
			summary_json jsonb NOT NULL,
			withdrawn_at timestamp NULL
		);
		CREATE INDEX IF NOT EXISTS lag_drift_calibration_calibrated_at
		ON lag_drift_calibration (calibrated_at, calibration_id)
		WHERE withdrawn_at IS NULL;
	`

const lagDriftClientIpCountrySchemaSql = `
		SET LOCAL lock_timeout = '5s';
		ALTER TABLE lag_drift_test
			ADD COLUMN IF NOT EXISTS client_ip_country_code varchar(2) NULL,
			ADD COLUMN IF NOT EXISTS client_ip_country_name varchar(256) NULL;
	`

const lagDriftRunKSchemaSql = `
		SET LOCAL lock_timeout = '5s';
		ALTER TABLE lag_drift_run
			ADD COLUMN IF NOT EXISTS k double precision NULL,
			ADD COLUMN IF NOT EXISTS k_early double precision NULL;
	`

// LagDriftSchemaMigrationVersion is the version that creates lag_drift_test,
// lag_drift_run and lag_drift_network and adds lag_track's kind and drift
// columns.
func LagDriftSchemaMigrationVersion() int {
	return sqlMigrationVersion(lagDriftSchemaSql)
}

// LagDriftCalibrationSchemaMigrationVersion is the version that creates
// lag_drift_calibration.
func LagDriftCalibrationSchemaMigrationVersion() int {
	return sqlMigrationVersion(lagDriftCalibrationSchemaSql)
}

// LagDriftClientIpCountrySchemaMigrationVersion is the version that adds the
// country of a drift test's client_ip to lag_drift_test.
func LagDriftClientIpCountrySchemaMigrationVersion() int {
	return sqlMigrationVersion(lagDriftClientIpCountrySchemaSql)
}

// LagDriftRunKSchemaMigrationVersion is the version that adds a drift run's k
// and k_early to lag_drift_run.
func LagDriftRunKSchemaMigrationVersion() int {
	return sqlMigrationVersion(lagDriftRunKSchemaSql)
}
