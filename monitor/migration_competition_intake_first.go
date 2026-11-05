// Unregistered intake-first custody is composed on demand; integration owns the
// actual append version and exact predecessor retirement boundaries.
package monitor

import (
	"strings"

	"github.com/urnetwork/server"
)

// Every inherited predicate survives. Only the three exact legacy-body pins
// are replaced by successors that embed those complete bodies byte-for-byte.
func competitionIntakeFirstArtifactQuery() string {
	functions := server.CompetitionIntakeFirstFunctionsSql()
	bodies := map[string]string{}
	queries := []string{}
	for _, function := range functions {
		bodies[function.Signature] = function.Body
		queries = append(queries, snMainnetMigrationFunction(function.Signature, function.Result, "plpgsql", "v", false, function.Body, function.Arguments...))
	}
	round, reserved, baseline := server.CompetitionIntakeFirstLegacyBodiesSql()
	replace := func(query, old, next string) string {
		old = snMainnetMigrationLiteral(old)
		if strings.Count(query, old) != 1 {
			panic("intake-first predecessor custody body is absent or ambiguous")
		}
		return strings.Replace(query, old, snMainnetMigrationLiteral(next), 1)
	}
	queries = append(queries,
		replace(competitionProductionBenefitMigrationContract.query, round, bodies["competition_v2_round_insert_guard()"]),
		replace(competitionMigration846ArtifactQuery, reserved, bodies["competition_v2_round_reserved_identity_guard()"]),
		replace(competitionDistributedEvaluationMigrationContract.query, baseline, bodies["competition_round_baseline_insert_guard()"]),
		migrationFp2Table("competition_source_operator_handoff"),
		snMainnetMigrationColumns("competition_source_operator_handoff",
			snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "target_epoch", kind: "integer", notNull: true},
			snMainnetMigrationColumn{name: "revision", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "idempotency_key", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "handoff_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "request_canonical", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("competition_source_operator_handoff", "PRIMARY KEY (environment, competition_id, season_id, target_epoch, revision)", "UNIQUE (environment, competition_id, season_id, idempotency_key)",
			"CHECK (((target_epoch >= 2) AND (target_epoch <= 6)))", "CHECK ((revision > 0))",
			"CHECK (((octet_length(request_canonical) >= 1) AND (octet_length(request_canonical) <= 1048576)))"),
		snMainnetMigrationForeignKeyCensus("competition_source_operator_handoff"),
		migrationFp2Table("competition_source_intake_activation"),
		snMainnetMigrationColumns("competition_source_intake_activation",
			snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true},
			snMainnetMigrationColumn{name: "target_round_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "target_epoch", kind: "integer", notNull: true},
			snMainnetMigrationColumn{name: "grant_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "grant_sha256", kind: "character(64)", notNull: true},
			snMainnetMigrationColumn{name: "round_policy_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "grant_canonical", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "round_policy_canonical", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "api_readiness_canonical", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("competition_source_intake_activation", "PRIMARY KEY (environment, competition_id, season_id, generation)", "UNIQUE (target_round_id)",
			"CHECK (((target_epoch >= 2) AND (target_epoch <= 6)))", "CHECK ((grant_sha256 ~ '^[0-9a-f]{64}$'::text))",
			"CHECK (((octet_length(grant_canonical) >= 1) AND (octet_length(grant_canonical) <= 1048576)))",
			"CHECK (((octet_length(round_policy_canonical) >= 1) AND (octet_length(round_policy_canonical) <= 1048576)))",
			"CHECK (((octet_length(api_readiness_canonical) >= 1) AND (octet_length(api_readiness_canonical) <= 1048576)))"),
		snMainnetMigrationForeignKey("competition_source_intake_activation", "competition_source_activation", "FOREIGN KEY (environment, competition_id, season_id, generation) REFERENCES competition_source_activation(environment, competition_id, season_id, generation)"),
		snMainnetMigrationForeignKeyCensus("competition_source_intake_activation", "FOREIGN KEY (environment, competition_id, season_id, generation) REFERENCES competition_source_activation(environment, competition_id, season_id, generation)"),
		migrationFp2Table("competition_source_intake_pointer"),
		snMainnetMigrationColumns("competition_source_intake_pointer",
			snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true}),
		snMainnetMigrationConstraints("competition_source_intake_pointer", "PRIMARY KEY (environment, competition_id, season_id)"),
		snMainnetMigrationForeignKey("competition_source_intake_pointer", "competition_source_intake_activation", "FOREIGN KEY (environment, competition_id, season_id, generation) REFERENCES competition_source_intake_activation(environment, competition_id, season_id, generation)"),
		snMainnetMigrationForeignKeyCensus("competition_source_intake_pointer", "FOREIGN KEY (environment, competition_id, season_id, generation) REFERENCES competition_source_intake_activation(environment, competition_id, season_id, generation)"))
	for _, trigger := range []struct {
		table, name, function string
		kind                  int
	}{
		{table: "competition_source_operator_handoff", name: "competition_source_operator_handoff_identity", function: "competition_v2_intake_handoff_guard", kind: 7},
		{table: "competition_source_operator_handoff", name: "competition_source_operator_handoff_append_only", function: "competition_append_only_guard", kind: 27},
		{table: "competition_source_operator_handoff", name: "competition_source_operator_handoff_no_truncate", function: "competition_append_only_guard", kind: 34},
		{table: "competition_source_intake_activation", name: "competition_source_intake_activation_identity", function: "competition_v2_intake_grant_guard", kind: 7},
		{table: "competition_source_intake_activation", name: "competition_source_intake_activation_append_only", function: "competition_append_only_guard", kind: 27},
		{table: "competition_source_intake_activation", name: "competition_source_intake_activation_no_truncate", function: "competition_append_only_guard", kind: 34},
		{table: "competition_source_intake_pointer", name: "competition_source_intake_pointer_identity", function: "competition_v2_intake_pointer_guard", kind: 31},
		{table: "competition_source_intake_pointer", name: "competition_source_intake_pointer_no_truncate", function: "competition_append_only_guard", kind: 34},
		{table: "competition_job", name: "competition_01_intake_execution", function: "competition_v2_intake_execution_guard", kind: 23},
		{table: "competition_integrity_review", name: "competition_01_intake_review_fence", function: "competition_v2_intake_ledger_fence_guard", kind: 7},
		{table: "competition_reward_decision", name: "competition_01_intake_decision_fence", function: "competition_v2_intake_ledger_fence_guard", kind: 7},
	} {
		queries = append(queries, snMainnetMigrationTrigger(trigger.table, trigger.name, trigger.function, trigger.kind))
	}
	return "(" + strings.Join(queries, " AND ") + ")"
}

// No package initializer registers an unpublished zero-version contract. The
// integration appends this candidate after assigning its actual migration row.
func competitionIntakeFirstMigrationContract() snMainnetMigrationContract {
	version := server.CompetitionIntakeFirstMigrationVersion()
	if version < 1 {
		panic("intake-first migration is not registered")
	}
	return migrationCurrentSuffix(version, "purpose-separated competition intake and manual source handoff custody", competitionIntakeFirstArtifactQuery())
}
