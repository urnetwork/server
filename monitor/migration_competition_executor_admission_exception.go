// The retained host admission exception (server
// competition_executor_admission_exception_migration.go) belongs to the
// migration catalog. The migration re-creates the whole-host readiness whose
// exact body the qualification recovery custody carries from the executor
// control amendment, so it retires that custody at its own version and
// carries all of it on, with only that body replaced by the one it publishes.
package monitor

import (
	"strings"

	"github.com/urnetwork/server"
)

// The admitted evidence view: every no-op receipt and every retained receipt
// an operator exception admits, the rows the whole-host readiness reads.
const competitionExecutorAdmittedEvidenceViewDefinition = ` SELECT competition_executor_equivalence.round_id,
    competition_executor_equivalence.host_id,
    competition_executor_equivalence.receipt_json,
    competition_executor_equivalence.receipt_sha256,
    competition_executor_equivalence.baseline_sha256,
    competition_executor_equivalence.created_at
   FROM competition_executor_equivalence
UNION ALL
 SELECT competition_executor_admission_exception.round_id,
    competition_executor_admission_exception.host_id,
    competition_executor_admission_exception.receipt_json,
    competition_executor_admission_exception.receipt_sha256,
    competition_executor_admission_exception.baseline_sha256,
    competition_executor_admission_exception.created_at
   FROM competition_executor_admission_exception;`

// Every inherited qualification recovery predicate survives; only the exact
// whole-host readiness body moves to the one that reads the admitted evidence
// through the selected qualification clock. The exception table, its key,
// checks and round key, its insert guard, its append-only, truncate and
// insert triggers, and the admitted evidence view are new.
var competitionExecutorAdmissionExceptionMigrationContract = func() snMainnetMigrationContract {
	bodies := server.CompetitionExecutorAdmissionExceptionBodiesSql()
	control := server.CompetitionExecutorControlAmendmentBodiesSql()
	old := snMainnetMigrationLiteral(control.HostsReady)
	inherited := competitionExecutorQualificationRecoveryMigrationContract.query
	if strings.Count(inherited, old) != 1 {
		panic("admission exception predecessor custody body is absent or ambiguous")
	}
	inherited = strings.Replace(inherited, old, snMainnetMigrationLiteral(bodies.HostsReady), 1)
	const roundKey = "FOREIGN KEY (round_id) REFERENCES competition_round(round_id)"
	queries := []string{
		inherited,
		migrationFp2Table("competition_executor_admission_exception"),
		snMainnetMigrationColumns("competition_executor_admission_exception",
			snMainnetMigrationColumn{name: "round_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "host_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "approval_sha256", kind: "character(64)", notNull: true},
			snMainnetMigrationColumn{name: "approval_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "approval_canonical", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "receipt_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "receipt_canonical", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "receipt_sha256", kind: "character(64)", notNull: true},
			snMainnetMigrationColumn{name: "baseline_sha256", kind: "character(64)", notNull: true},
			snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true},
			snMainnetMigrationColumn{name: "approved_at", kind: "timestamp without time zone", notNull: true,
				defaultExpression: "(statement_timestamp() AT TIME ZONE 'UTC'::text)"}),
		snMainnetMigrationConstraints("competition_executor_admission_exception", "PRIMARY KEY (round_id, host_id)",
			"CHECK ((approval_sha256 ~ '^[0-9a-f]{64}$'::text))",
			"CHECK (((octet_length(approval_canonical) >= 1) AND (octet_length(approval_canonical) <= 32768)))",
			"CHECK (((octet_length(receipt_canonical) >= 1) AND (octet_length(receipt_canonical) <= 1048576)))",
			"CHECK ((receipt_sha256 ~ '^[0-9a-f]{64}$'::text))",
			"CHECK (((convert_from(approval_canonical, 'UTF8'::name))::jsonb = approval_json))",
			"CHECK ((encode(sha256(approval_canonical), 'hex'::text) = (approval_sha256)::text))",
			"CHECK (((convert_from(receipt_canonical, 'UTF8'::name))::jsonb = receipt_json))",
			"CHECK ((encode(sha256(receipt_canonical), 'hex'::text) = (receipt_sha256)::text))"),
		snMainnetMigrationForeignKey("competition_executor_admission_exception", "competition_round", roundKey),
		snMainnetMigrationForeignKeyCensus("competition_executor_admission_exception", roundKey),
		snMainnetMigrationFunction("competition_executor_admission_exception_guard()", "trigger", "plpgsql", "v", false, bodies.ExceptionGuard),
		// a plain view (never materialized) with its exact columns and query
		`EXISTS(SELECT 1 FROM pg_class c WHERE c.oid=to_regclass('public.competition_executor_admitted_evidence')
 AND c.relkind='v' AND c.relpersistence='p'
 AND pg_get_viewdef(c.oid)=` + snMainnetMigrationLiteral(competitionExecutorAdmittedEvidenceViewDefinition) + `)`,
		snMainnetMigrationColumns("competition_executor_admitted_evidence",
			snMainnetMigrationColumn{name: "round_id", kind: "uuid"},
			snMainnetMigrationColumn{name: "host_id", kind: "character varying(128)"},
			snMainnetMigrationColumn{name: "receipt_json", kind: "jsonb"},
			snMainnetMigrationColumn{name: "receipt_sha256", kind: "character(64)"},
			snMainnetMigrationColumn{name: "baseline_sha256", kind: "character(64)"},
			snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone"}),
	}
	for _, trigger := range []struct {
		table, name, function string
		kind                  int
	}{
		{table: "competition_executor_admission_exception", name: "competition_executor_admission_exception_append_only", function: "competition_append_only_guard", kind: 27},
		{table: "competition_executor_admission_exception", name: "competition_executor_admission_exception_no_truncate", function: "competition_append_only_guard", kind: 34},
		{table: "competition_executor_admission_exception", name: "competition_executor_admission_exception_identity", function: "competition_executor_admission_exception_guard", kind: 7},
	} {
		queries = append(queries, snMainnetMigrationTrigger(trigger.table, trigger.name, trigger.function, trigger.kind))
	}
	return migrationCurrentSuffix(server.CompetitionExecutorAdmissionExceptionMigrationVersion(), "retained host admission exception with retained qualification recovery custody", queries...)
}()
