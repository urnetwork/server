// The operator's executor control amendment (server
// competition_executor_control_amendment_migration.go) belongs to the migration
// catalog. The migration re-creates the distributed executor's equivalence
// guard and the intake grading readiness, whose exact bodies the official
// review custody carries on, so it retires that custody at its own version and
// carries all of it on, with only those two bodies replaced by the ones it
// publishes.
package monitor

import (
	"strings"

	"github.com/urnetwork/server"
)

// Every inherited official review predicate survives; only the exact
// equivalence guard and intake grading readiness bodies move to the ones that
// read the amended host identity. The started-job index, the amendment table,
// its key, checks and round key, its five functions and its append-only,
// truncate and insert guards, and the receipt and qualification guards on the
// executor's tables are new.
var competitionExecutorControlAmendmentMigrationContract = func() snMainnetMigrationContract {
	bodies := server.CompetitionExecutorControlAmendmentBodiesSql()
	_, _, _, equivalenceGuard, _ := server.CompetitionDistributedEvaluationBodiesSql()
	gradingReady := ""
	for _, function := range server.CompetitionIntakeFirstFunctionsSql() {
		if function.Signature == "competition_v2_intake_grading_ready(uuid)" {
			gradingReady = function.Body
		}
	}
	if gradingReady == "" {
		panic("intake grading readiness body is absent")
	}
	replace := func(query, old, next string) string {
		old = snMainnetMigrationLiteral(old)
		if strings.Count(query, old) != 1 {
			panic("executor control amendment predecessor custody body is absent or ambiguous")
		}
		return strings.Replace(query, old, snMainnetMigrationLiteral(next), 1)
	}
	inherited := competitionOfficialReviewMigrationContract.query
	inherited = replace(inherited, equivalenceGuard, bodies.EquivalenceGuard)
	inherited = replace(inherited, gradingReady, bodies.IntakeGradingReady)
	const roundKey = "FOREIGN KEY (round_id) REFERENCES competition_round(round_id)"
	queries := []string{
		inherited,
		migrationFp2Index("competition_job", "competition_executor_control_started_job", "round_id",
			"((attempt_count <> 0) OR (started_at IS NOT NULL) OR (execution_deadline_at IS NOT NULL))", false),
		migrationFp2Table("competition_executor_control_amendment"),
		snMainnetMigrationColumns("competition_executor_control_amendment",
			snMainnetMigrationColumn{name: "round_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "amendment_sha256", kind: "character(64)", notNull: true},
			snMainnetMigrationColumn{name: "amendment_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "amendment_canonical", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("competition_executor_control_amendment", "PRIMARY KEY (round_id)",
			"CHECK ((amendment_sha256 ~ '^[0-9a-f]{64}$'::text))",
			"CHECK (((octet_length(amendment_canonical) >= 1) AND (octet_length(amendment_canonical) <= 1048576)))",
			"CHECK (((convert_from(amendment_canonical, 'UTF8'::name))::jsonb = amendment_json))"),
		snMainnetMigrationForeignKey("competition_executor_control_amendment", "competition_round", roundKey),
		snMainnetMigrationForeignKeyCensus("competition_executor_control_amendment", roundKey),
		snMainnetMigrationFunction("competition_executor_control_amendment_guard()", "trigger", "plpgsql", "v", false, bodies.AmendmentGuard),
		snMainnetMigrationFunction("competition_executor_control_host(uuid,text)", "jsonb", "sql", "s", false, bodies.Host, "target", "identity"),
		snMainnetMigrationFunction("competition_executor_control_receipt_guard()", "trigger", "plpgsql", "v", false, bodies.ReceiptGuard),
		snMainnetMigrationFunction("competition_executor_control_qualification_guard()", "trigger", "plpgsql", "v", false, bodies.QualificationGuard),
		snMainnetMigrationFunction("competition_executor_control_hosts_ready(uuid)", "boolean", "plpgsql", "v", false, bodies.HostsReady, "target"),
	}
	for _, trigger := range []struct {
		table, name, function string
		kind                  int
	}{
		{table: "competition_executor_control_amendment", name: "competition_executor_control_amendment_append_only", function: "competition_append_only_guard", kind: 27},
		{table: "competition_executor_control_amendment", name: "competition_executor_control_amendment_no_truncate", function: "competition_append_only_guard", kind: 34},
		{table: "competition_executor_control_amendment", name: "competition_executor_control_amendment_identity", function: "competition_executor_control_amendment_guard", kind: 7},
		{table: "competition_executor_equivalence", name: "competition_executor_control_receipt_identity", function: "competition_executor_control_receipt_guard", kind: 7},
		{table: "competition_executor_qualification_run", name: "competition_executor_control_qualification_identity", function: "competition_executor_control_qualification_guard", kind: 7},
	} {
		queries = append(queries, snMainnetMigrationTrigger(trigger.table, trigger.name, trigger.function, trigger.kind))
	}
	return migrationCurrentSuffix(server.CompetitionExecutorControlAmendmentMigrationVersion(), "operator executor control amendment with retained official review, handoff, intake and winner-review custody", queries...)
}()
