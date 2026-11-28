// The exact-job successor retains every historical recovery and admission
// predicate while owning its append-only audit and owner projection.
package monitor

import (
	"strings"

	"github.com/urnetwork/server/v2026"
)

// The current owner carries complete predecessor bodies; no retired predicate
// or generic successor walker can waive old or new catalog drift.
var competitionSecondRecoveryMigrationContract = func() snMainnetMigrationContract {
	old := server.CompetitionOperatorRequestedPatchRetryBodiesSql()
	bodies := server.CompetitionSecondRecoveryBodiesSql()
	query := competitionOperatorRequestedPatchRetryMigrationContract.query
	for _, replacement := range []struct{ previous, next string }{
		{previous: old.Immutable, next: bodies.Immutable},
		{previous: old.Executor, next: bodies.Executor},
		{previous: old.Intake, next: bodies.Intake},
		{previous: old.Insert, next: bodies.LegacyInsert},
		{previous: "CHECK ((evaluation_cycle = ANY (ARRAY[0, 1])))", next: "CHECK (((evaluation_cycle = ANY (ARRAY[0, 1])) OR ((evaluation_cycle = 2) AND competition_second_recovery_job_allowed(job_id))))"},
		{previous: "CHECK ((((evaluation_cycle = 0) AND (cycle_attempt_offset = 0) AND (cycle_started_at IS NULL)) OR ((evaluation_cycle = 1) AND (cycle_attempt_offset > 0) AND (cycle_attempt_offset <= attempt_count))))", next: "CHECK ((((evaluation_cycle = 0) AND (cycle_attempt_offset = 0) AND (cycle_started_at IS NULL) AND (second_recovery_receipt_json IS NULL)) OR ((evaluation_cycle = 1) AND (cycle_attempt_offset > 0) AND (cycle_attempt_offset <= attempt_count) AND (second_recovery_receipt_json IS NULL)) OR ((evaluation_cycle = 2) AND competition_second_recovery_job_allowed(job_id) AND (cycle_attempt_offset > 0) AND (cycle_attempt_offset <= attempt_count) AND (second_recovery_receipt_json IS NOT NULL) AND (jsonb_typeof(second_recovery_receipt_json) = 'object'::text) AND (NOT ((second_recovery_receipt_json ->> 'schema'::text) IS DISTINCT FROM '3'::text)) AND (NOT ((second_recovery_receipt_json ->> 'cycle'::text) IS DISTINCT FROM '2'::text)))))"},
	} {
		previous := snMainnetMigrationLiteral(replacement.previous)
		if strings.Count(query, previous) != 1 {
			panic("second recovery catalog predecessor changed")
		}
		query = strings.Replace(query, previous, snMainnetMigrationLiteral(replacement.next), 1)
	}
	return migrationCurrentSuffix(server.CompetitionSecondRecoveryMigrationVersion(), "exact-job second recovery with immutable prior receipt", query,
		snMainnetMigrationColumns("competition_job", snMainnetMigrationColumn{name: "second_recovery_receipt_json", kind: "jsonb"}),
		migrationFp2Table("competition_job_second_reevaluation"),
		snMainnetMigrationColumns("competition_job_second_reevaluation",
			snMainnetMigrationColumn{name: "job_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "round_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "operator_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "idempotency_key", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "request_sha256", kind: "character(64)", notNull: true},
			snMainnetMigrationColumn{name: "authorization_sha256", kind: "character(64)", notNull: true},
			snMainnetMigrationColumn{name: "previous_receipt_sha256", kind: "character(64)", notNull: true},
			snMainnetMigrationColumn{name: "authorized_at", kind: "timestamp without time zone", notNull: true},
			snMainnetMigrationColumn{name: "original_terminal_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "authorization_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "receipt_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "transaction_id", kind: "xid8", notNull: true, defaultExpression: "pg_current_xact_id()"}),
		snMainnetMigrationConstraints("competition_job_second_reevaluation", "PRIMARY KEY (job_id)",
			"UNIQUE (environment, competition_id, season_id, idempotency_key)",
			"CHECK (competition_second_recovery_job_allowed(job_id))",
			"CHECK ((request_sha256 ~ '^[0-9a-f]{64}$'::text))",
			"CHECK ((authorization_sha256 ~ '^[0-9a-f]{64}$'::text))",
			"CHECK ((previous_receipt_sha256 ~ '^[0-9a-f]{64}$'::text))"),
		snMainnetMigrationForeignKey("competition_job_second_reevaluation", "competition_job_reevaluation", "FOREIGN KEY (job_id) REFERENCES competition_job_reevaluation(job_id)"),
		snMainnetMigrationForeignKey("competition_job_second_reevaluation", "competition_round", "FOREIGN KEY (round_id) REFERENCES competition_round(round_id)"),
		snMainnetMigrationForeignKeyCensus("competition_job_second_reevaluation", "FOREIGN KEY (job_id) REFERENCES competition_job_reevaluation(job_id)", "FOREIGN KEY (round_id) REFERENCES competition_round(round_id)"),
		snMainnetMigrationFunction("competition_second_recovery_job_allowed(uuid)", "boolean", "sql", "i", false, server.CompetitionSecondRecoveryScopeBodySql(), "target"),
		snMainnetMigrationFunction("competition_second_recovery_transition_allows(competition_job,competition_job)", "boolean", "plpgsql", "v", false, bodies.Transition, "previous_job", "next_job"),
		snMainnetMigrationFunction("competition_second_recovery_insert_validate(competition_job_second_reevaluation)", "competition_job_second_reevaluation", "plpgsql", "v", false, bodies.Insert, "next_audit"),
		snMainnetMigrationFunction("competition_second_recovery_audit_validate(competition_job_second_reevaluation)", "competition_job_second_reevaluation", "plpgsql", "v", false, bodies.Audit, "next_audit"),
		snMainnetMigrationFunction("competition_second_recovery_insert_guard()", "trigger", "plpgsql", "v", false, bodies.InsertGuard),
		snMainnetMigrationFunction("competition_second_recovery_commit_guard()", "trigger", "plpgsql", "v", false, bodies.Commit),
		snMainnetMigrationTrigger("competition_job_second_reevaluation", "competition_job_second_reevaluation_append_only", "competition_append_only_guard", 27),
		snMainnetMigrationTrigger("competition_job_second_reevaluation", "competition_job_second_reevaluation_insert", "competition_second_recovery_insert_guard", 7),
		migrationCompetitionDeferredTrigger("competition_job_second_reevaluation", "competition_job_second_reevaluation_atomic", "competition_second_recovery_commit_guard", 5))
}()
