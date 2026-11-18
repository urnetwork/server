// The lag.dev drift test's schema (server lag_drift_schema.go,
// lag.dev/DRIFTTEST.md; model/lag_drift_model.go,
// model/lag_drift_calibration_model.go): lag_track's kind and drift columns
// with the drift test's three tables, then the drift model's calibration, the
// country of a drift test's client_ip, and a drift run's k and k_early, four
// migrations with a contract each.
package monitor

import "github.com/urnetwork/server"

// lag_track's kind, required with the constant default lag, and the nullable
// drift test and run a drift track belongs to; then three durable ordinary
// tables with every column, key and index as the migration publishes them:
// the drift tests, keyed by the test with the token's hash unique, and indexed
// by network, by creation for retention, and by creation over the tests that
// still hold an address hash; their runs, keyed by test and run with each
// run's track unique, and indexed by start over the runs not complete; and
// each network's drift settings, keyed by the network. The migration creates
// each only when absent, so a look-alike of another shape would survive it:
// the shape itself is the artifact. Follow the migration's position after
// merges instead of pinning a number.
var lagDriftMigrationContract = migrationCurrentSuffix(
	server.LagDriftSchemaMigrationVersion(),
	"lag.dev drift tables and track kind",
	snMainnetMigrationColumns("lag_track",
		snMainnetMigrationColumn{name: "kind", kind: "character varying(8)", notNull: true, defaultExpression: "'lag'::character varying"},
		snMainnetMigrationColumn{name: "drift_test_id", kind: "uuid"},
		snMainnetMigrationColumn{name: "drift_run", kind: "smallint"}),
	migrationFp2Table("lag_drift_test"),
	snMainnetMigrationColumns("lag_drift_test",
		snMainnetMigrationColumn{name: "drift_test_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "token_hash", kind: "bytea", notNull: true},
		snMainnetMigrationColumn{name: "client_ip_hash", kind: "bytea"},
		snMainnetMigrationColumn{name: "client_ip_family", kind: "smallint", notNull: true},
		snMainnetMigrationColumn{name: "parent_origins", kind: "character varying(256)[]"},
		snMainnetMigrationColumn{name: "confidence", kind: "double precision", notNull: true},
		snMainnetMigrationColumn{name: "reference", kind: "character varying(128)"},
		snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "expires_at", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "first_started_at", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "retest_until", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "run_count", kind: "smallint", notNull: true, defaultExpression: "0"},
		snMainnetMigrationColumn{name: "updated_at", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "failed_at", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "revoke_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "result_json", kind: "jsonb"},
		snMainnetMigrationColumn{name: "result_at", kind: "timestamp without time zone"}),
	snMainnetMigrationConstraints("lag_drift_test", "PRIMARY KEY (drift_test_id)", "UNIQUE (token_hash)"),
	migrationFp2Index("lag_drift_test", "lag_drift_test_network_id_created_at", "network_id, created_at", "", false),
	migrationFp2Index("lag_drift_test", "lag_drift_test_created_at", "created_at, drift_test_id", "", false),
	migrationFp2Index("lag_drift_test", "lag_drift_test_client_ip_hash_created_at", "created_at", "(client_ip_hash IS NOT NULL)", false),
	migrationFp2Table("lag_drift_run"),
	snMainnetMigrationColumns("lag_drift_run",
		snMainnetMigrationColumn{name: "drift_test_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "run", kind: "smallint", notNull: true},
		snMainnetMigrationColumn{name: "track_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "state", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "started_at", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "finished_at", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "completed_at", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "stop", kind: "character varying(16)"},
		snMainnetMigrationColumn{name: "ip_family", kind: "smallint", notNull: true},
		snMainnetMigrationColumn{name: "client_ip_matches", kind: "boolean"},
		snMainnetMigrationColumn{name: "ip_country_code", kind: "character varying(2)"},
		snMainnetMigrationColumn{name: "ip_country_name", kind: "character varying(256)"},
		snMainnetMigrationColumn{name: "extenders_json", kind: "jsonb", notNull: true},
		snMainnetMigrationColumn{name: "outcome", kind: "character varying(32)"},
		snMainnetMigrationColumn{name: "supported_stop", kind: "character varying(16)"},
		snMainnetMigrationColumn{name: "answered", kind: "smallint"},
		snMainnetMigrationColumn{name: "usable", kind: "smallint"},
		snMainnetMigrationColumn{name: "measured_ms", kind: "integer"}),
	snMainnetMigrationConstraints("lag_drift_run", "PRIMARY KEY (drift_test_id, run)", "UNIQUE (track_id)"),
	migrationFp2Index("lag_drift_run", "lag_drift_run_open_started_at", "started_at", "((state)::text <> 'complete'::text)", false),
	migrationFp2Table("lag_drift_network"),
	snMainnetMigrationColumns("lag_drift_network",
		snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "parent_origins", kind: "character varying(256)[]"},
		snMainnetMigrationColumn{name: "disable_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("lag_drift_network", "PRIMARY KEY (network_id)"))

// One durable ordinary table keyed by the calibration, with ten NOT NULL
// columns and no defaults, the nullable withdrawal, and the index the drift
// test reads the latest calibration not withdrawn by.
var lagDriftCalibrationMigrationContract = migrationCurrentSuffix(
	server.LagDriftCalibrationSchemaMigrationVersion(),
	"lag.dev drift calibration",
	migrationFp2Table("lag_drift_calibration"),
	snMainnetMigrationColumns("lag_drift_calibration",
		snMainnetMigrationColumn{name: "calibration_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "calibrated_at", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "window_start", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "window_end", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "fit_tracks", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "fit_observations", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "kappa", kind: "double precision", notNull: true},
		snMainnetMigrationColumn{name: "sigma0_ms", kind: "double precision", notNull: true},
		snMainnetMigrationColumn{name: "relay_threshold_ms", kind: "double precision", notNull: true},
		snMainnetMigrationColumn{name: "summary_json", kind: "jsonb", notNull: true},
		snMainnetMigrationColumn{name: "withdrawn_at", kind: "timestamp without time zone"}),
	snMainnetMigrationConstraints("lag_drift_calibration", "PRIMARY KEY (calibration_id)"),
	migrationFp2Index("lag_drift_calibration", "lag_drift_calibration_calibrated_at", "calibrated_at, calibration_id", "(withdrawn_at IS NULL)", false))

// Two nullable columns on lag_drift_test with no default: a test allocated
// before them has none, which judges as an unknown country, never another.
var lagDriftClientIpCountryMigrationContract = migrationCurrentSuffix(
	server.LagDriftClientIpCountrySchemaMigrationVersion(),
	"lag.dev drift test client country",
	snMainnetMigrationColumns("lag_drift_test",
		snMainnetMigrationColumn{name: "client_ip_country_code", kind: "character varying(2)"},
		snMainnetMigrationColumn{name: "client_ip_country_name", kind: "character varying(256)"}))

// Two nullable double precision columns on lag_drift_run with no default: a
// run started before them has none and is judged at the textbook k.
var lagDriftRunKMigrationContract = migrationCurrentSuffix(
	server.LagDriftRunKSchemaMigrationVersion(),
	"lag.dev drift run k and k_early",
	snMainnetMigrationColumns("lag_drift_run",
		snMainnetMigrationColumn{name: "k", kind: "double precision"},
		snMainnetMigrationColumn{name: "k_early", kind: "double precision"}))
