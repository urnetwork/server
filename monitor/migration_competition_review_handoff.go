// The pending review handoff ledger (server competition_review_handoff_migration.go)
// belongs to the migration catalog. The migration re-creates the three
// winner-review functions the intake-first custody pins, so it retires that
// custody at its own version and carries all of it on, with only those three
// bodies replaced by the ones it publishes.
package monitor

import (
	"strings"

	"github.com/urnetwork/server"
)

// Every inherited intake-first predicate survives; only the exact winner
// source, review and checkpoint bodies move to the handoff-aware ones. The
// ledger's tables, keys, checks, sequence, functions and triggers are new.
var competitionReviewHandoffMigrationContract = func() snMainnetMigrationContract {
	bodies := server.CompetitionReviewHandoffBodiesSql()
	_, _, winnerSourceAuthorized, winnerReviewGuard, _, _, _, _ := server.CompetitionSnowTwoMemberWinnerReviewBodiesSql()
	_, _, _, _, winnerCheckpointGuard := server.CompetitionWinnerReviewBodiesSql()
	replace := func(query, old, next string) string {
		old = snMainnetMigrationLiteral(old)
		if !strings.Contains(query, old) {
			panic("review handoff predecessor custody body is absent")
		}
		return strings.ReplaceAll(query, old, snMainnetMigrationLiteral(next))
	}
	inherited := competitionIntakeFirstArtifactQuery()
	inherited = replace(inherited, winnerSourceAuthorized, bodies.WinnerSourceAuthorized)
	inherited = replace(inherited, winnerReviewGuard, bodies.WinnerReviewGuard)
	inherited = replace(inherited, winnerCheckpointGuard, bodies.WinnerCheckpointGuard)
	const handoffKey = "FOREIGN KEY (handoff_id) REFERENCES competition_review_handoff(handoff_id)"
	const roundKey = "FOREIGN KEY (round_id) REFERENCES competition_round(round_id)"
	queries := []string{
		inherited,
		migrationFp2Table("competition_review_handoff"),
		snMainnetMigrationColumns("competition_review_handoff",
			snMainnetMigrationColumn{name: "handoff_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "round_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "job_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "decision_sha256", kind: "text", notNull: true},
			snMainnetMigrationColumn{name: "owner_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "started_at", kind: "timestamp without time zone", notNull: true},
			snMainnetMigrationColumn{name: "deadline", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("competition_review_handoff", "PRIMARY KEY (handoff_id)", "UNIQUE (round_id, decision_sha256)",
			"CHECK ((decision_sha256 ~ '^[0-9a-f]{64}$'::text))",
			"CHECK ((octet_length((owner_json)::text) <= 1048576))",
			"CHECK (((deadline > started_at) AND (deadline <= (started_at + '24:00:00'::interval))))"),
		snMainnetMigrationForeignKey("competition_review_handoff", "competition_round", roundKey),
		snMainnetMigrationForeignKeyCensus("competition_review_handoff", roundKey),
		migrationFp2Table("competition_review_handoff_call"),
		snMainnetMigrationColumns("competition_review_handoff_call",
			snMainnetMigrationColumn{name: "event_cursor", kind: "bigint", notNull: true, defaultExpression: "nextval('competition_review_handoff_call_event_cursor_seq'::regclass)"},
			snMainnetMigrationColumn{name: "handoff_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "call_id", kind: "text", notNull: true},
			snMainnetMigrationColumn{name: "kind", kind: "text", notNull: true},
			snMainnetMigrationColumn{name: "call_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "recorded_at", kind: "timestamp without time zone", notNull: true}),
		migrationCompetitionSequence("competition_review_handoff_call", "event_cursor"),
		snMainnetMigrationConstraints("competition_review_handoff_call", "PRIMARY KEY (event_cursor)", "UNIQUE (handoff_id, call_id, kind)",
			"CHECK ((call_id ~ '^[a-z0-9][a-z0-9._-]{0,127}$'::text))",
			"CHECK ((kind = ANY (ARRAY['claim'::text, 'receipt'::text])))",
			"CHECK ((octet_length((call_json)::text) <= 8192))"),
		snMainnetMigrationForeignKey("competition_review_handoff_call", "competition_review_handoff", handoffKey),
		snMainnetMigrationForeignKeyCensus("competition_review_handoff_call", handoffKey),
		migrationFp2Table("competition_review_handoff_completion"),
		snMainnetMigrationColumns("competition_review_handoff_completion",
			snMainnetMigrationColumn{name: "handoff_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "completion_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "completed_at", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("competition_review_handoff_completion", "PRIMARY KEY (handoff_id)",
			"CHECK ((octet_length((completion_json)::text) <= 131072))"),
		snMainnetMigrationForeignKey("competition_review_handoff_completion", "competition_review_handoff", handoffKey),
		snMainnetMigrationForeignKeyCensus("competition_review_handoff_completion", handoffKey),
		snMainnetMigrationFunction("competition_review_handoff_clock()", "timestamp without time zone", "sql", "v", false, bodies.Clock),
		snMainnetMigrationFunction("competition_review_handoff_owner_guard()", "trigger", "plpgsql", "v", false, bodies.OwnerGuard),
		snMainnetMigrationFunction("competition_review_handoff_call_guard()", "trigger", "plpgsql", "v", false, bodies.CallGuard),
		snMainnetMigrationFunction("competition_review_handoff_completion_guard()", "trigger", "plpgsql", "v", false, bodies.CompletionGuard),
		snMainnetMigrationFunction("competition_review_handoff_observed(jsonb,timestamp without time zone)", "timestamp without time zone", "plpgsql", "s", false, bodies.Observed,
			"review_json", "legacy_created_at"),
		snMainnetMigrationFunction("competition_review_handoff_import_guard()", "trigger", "plpgsql", "v", false, bodies.ImportGuard),
	}
	for _, trigger := range []struct {
		table, name, function string
		kind                  int
	}{
		{table: "competition_review_handoff", name: "competition_review_handoff_immutable", function: "competition_append_only_guard", kind: 27},
		{table: "competition_review_handoff_call", name: "competition_review_handoff_call_immutable", function: "competition_append_only_guard", kind: 27},
		{table: "competition_review_handoff_completion", name: "competition_review_handoff_completion_immutable", function: "competition_append_only_guard", kind: 27},
		{table: "competition_review_handoff", name: "competition_review_handoff_before_insert", function: "competition_review_handoff_owner_guard", kind: 7},
		{table: "competition_review_handoff_call", name: "competition_review_handoff_call_before_insert", function: "competition_review_handoff_call_guard", kind: 7},
		{table: "competition_review_handoff_completion", name: "competition_review_handoff_completion_before_insert", function: "competition_review_handoff_completion_guard", kind: 7},
		{table: "competition_integrity_review", name: "competition_review_handoff_import_before_insert", function: "competition_review_handoff_import_guard", kind: 7},
	} {
		queries = append(queries, snMainnetMigrationTrigger(trigger.table, trigger.name, trigger.function, trigger.kind))
	}
	return migrationCurrentSuffix(server.CompetitionReviewHandoffMigrationVersion(), "pending review handoff ledger with retained intake and winner-review custody", queries...)
}()
