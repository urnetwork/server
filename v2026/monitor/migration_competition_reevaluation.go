// The audited single-use recovery of a failed competition job (server
// db_competition_reevaluation.go).
package monitor

import "github.com/urnetwork/server/v2026"

// The execution cycle columns on competition_job with their checks, the durable
// audit with every column, key and check and its two parents, and the exact guards
// and triggers that admit one recovery and fence stale writers. The migration
// re-creates competition_job_immutable_guard, which the v589 artifact pins only by
// name, so this contract owns its body. The checks and the defaults are the
// catalog's own text on PostgreSQL 18. The version is the migration's position in
// the server's list.
var competitionJobReevaluationMigrationContract = func() snMainnetMigrationContract {
	insertGuard, commitGuard, jobImmutableGuard, slotGuard := server.CompetitionJobReevaluationBodiesSql()
	contract := migrationCurrentSuffix(server.CompetitionJobReevaluationMigrationVersion(), "audited single-use competition job recovery",
		snMainnetMigrationColumns("competition_job",
			snMainnetMigrationColumn{name: "evaluation_cycle", kind: "integer", notNull: true, defaultExpression: "0"},
			snMainnetMigrationColumn{name: "cycle_attempt_offset", kind: "integer", notNull: true, defaultExpression: "0"},
			snMainnetMigrationColumn{name: "cycle_started_at", kind: "timestamp without time zone"}),
		snMainnetMigrationConstraints("competition_job",
			"CHECK ((evaluation_cycle = ANY (ARRAY[0, 1])))",
			"CHECK ((cycle_attempt_offset >= 0))",
			"CHECK ((((evaluation_cycle = 0) AND (cycle_attempt_offset = 0) AND (cycle_started_at IS NULL)) OR ((evaluation_cycle = 1) AND (cycle_attempt_offset > 0) AND (cycle_attempt_offset <= attempt_count))))"),
		migrationFp2Table("competition_job_reevaluation"),
		snMainnetMigrationColumns("competition_job_reevaluation",
			snMainnetMigrationColumn{name: "job_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "round_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "operator_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "idempotency_key", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "request_sha256", kind: "character(64)", notNull: true},
			snMainnetMigrationColumn{name: "authorization_sha256", kind: "character(64)", notNull: true},
			snMainnetMigrationColumn{name: "authorized_at", kind: "timestamp without time zone", notNull: true},
			snMainnetMigrationColumn{name: "original_terminal_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "authorization_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "receipt_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "transaction_id", kind: "xid8", notNull: true, defaultExpression: "pg_current_xact_id()"}),
		snMainnetMigrationConstraints("competition_job_reevaluation",
			"PRIMARY KEY (job_id)",
			"UNIQUE (environment, competition_id, season_id, idempotency_key)",
			"CHECK ((request_sha256 ~ '^[0-9a-f]{64}$'::text))",
			"CHECK ((authorization_sha256 ~ '^[0-9a-f]{64}$'::text))"),
		snMainnetMigrationForeignKey("competition_job_reevaluation", "competition_job", "FOREIGN KEY (job_id) REFERENCES competition_job(job_id)"),
		snMainnetMigrationForeignKey("competition_job_reevaluation", "competition_round", "FOREIGN KEY (round_id) REFERENCES competition_round(round_id)"),
		snMainnetMigrationForeignKeyCensus("competition_job_reevaluation",
			"FOREIGN KEY (job_id) REFERENCES competition_job(job_id)",
			"FOREIGN KEY (round_id) REFERENCES competition_round(round_id)"),
		snMainnetMigrationFunction("competition_job_reevaluation_insert_guard()", "trigger", "plpgsql", "v", false, insertGuard),
		snMainnetMigrationFunction("competition_job_reevaluation_commit_guard()", "trigger", "plpgsql", "v", false, commitGuard),
		snMainnetMigrationFunction("competition_job_immutable_guard()", "trigger", "plpgsql", "v", false, jobImmutableGuard),
		snMainnetMigrationFunction("competition_recovery_slot_guard()", "trigger", "plpgsql", "v", false, slotGuard),
		snMainnetMigrationTrigger("competition_job_reevaluation", "competition_job_reevaluation_append_only", "competition_append_only_guard", 27),
		snMainnetMigrationTrigger("competition_job_reevaluation", "competition_job_reevaluation_insert", "competition_job_reevaluation_insert_guard", 7),
		migrationCompetitionDeferredTrigger("competition_job_reevaluation", "competition_job_reevaluation_atomic", "competition_job_reevaluation_commit_guard", 5),
		snMainnetMigrationTrigger("competition_worker_slot", "competition_recovery_slot_authority", "competition_recovery_slot_guard", 19))
	contract.artifact.removedVersion = server.CompetitionReevaluationPostCloseMigrationVersion()
	return contract
}()
