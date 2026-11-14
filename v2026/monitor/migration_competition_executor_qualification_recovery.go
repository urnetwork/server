// The single-use executor qualification recovery (server
// competition_executor_qualification_recovery_migration.go) belongs to the
// migration catalog. The migration re-creates the distributed executor's
// equivalence guard, whose exact body the executor control amendment custody
// pins, so it retires that custody at its own version and carries all of it
// on, with only that body replaced by the one it publishes.
package monitor

import (
	"strings"

	"github.com/urnetwork/server/v2026"
)

// Every inherited executor control amendment predicate survives; only the
// exact equivalence guard body moves to the one that reads the recovered
// qualification clock and binds a receipt to its single replacement. The
// recovery table, its keys, checks and qualification-run key, its three
// functions, its append-only, truncate and insert guards, and the guard that
// keeps a replacement job out of public submissions are new.
var competitionExecutorQualificationRecoveryMigrationContract = func() snMainnetMigrationContract {
	bodies := server.CompetitionExecutorQualificationRecoveryBodiesSql()
	control := server.CompetitionExecutorControlAmendmentBodiesSql()
	old := snMainnetMigrationLiteral(control.EquivalenceGuard)
	inherited := competitionExecutorControlAmendmentMigrationContract.query
	if strings.Count(inherited, old) != 1 {
		panic("executor qualification recovery predecessor custody body is absent or ambiguous")
	}
	inherited = strings.Replace(inherited, old, snMainnetMigrationLiteral(bodies.EquivalenceGuard), 1)
	const runKey = "FOREIGN KEY (round_id, host_id) REFERENCES competition_executor_qualification_run(round_id, host_id)"
	// The clock is a set-returning SQL function: a record result, its two
	// input arguments and five table columns, each with its exact type.
	clock := snMainnetMigrationFunction("competition_executor_qualification_clock(uuid,text)", "record", "sql", "s", false, bodies.Clock,
		"target", "identity", "job_id", "started_at", "deadline_at", "api_image_digest", "worker_image_digest")
	for _, replacement := range []struct{ before, after string }{
		{before: "AND NOT p.proretset", after: "AND p.proretset"},
		{before: "AND p.proargmodes IS NULL", after: "AND p.proargmodes::text='{i,i,t,t,t,t,t}'" +
			" AND p.proallargtypes::regtype[]::text[]=ARRAY['uuid','text','uuid','timestamp without time zone','timestamp without time zone','character varying','character varying']"},
	} {
		if strings.Count(clock, replacement.before) != 1 {
			panic("set-returning function predicate shape changed")
		}
		clock = strings.Replace(clock, replacement.before, replacement.after, 1)
	}
	queries := []string{
		inherited,
		migrationFp2Table("competition_executor_qualification_recovery"),
		snMainnetMigrationColumns("competition_executor_qualification_recovery",
			snMainnetMigrationColumn{name: "round_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "host_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "authorization_sha256", kind: "character(64)", notNull: true},
			snMainnetMigrationColumn{name: "authorization_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "authorization_canonical", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "job_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "started_at", kind: "timestamp without time zone", notNull: true,
				defaultExpression: "(statement_timestamp() AT TIME ZONE 'UTC'::text)"},
			snMainnetMigrationColumn{name: "deadline_at", kind: "timestamp without time zone", notNull: true,
				defaultExpression: "((statement_timestamp() AT TIME ZONE 'UTC'::text) + '03:00:00'::interval)"},
			snMainnetMigrationColumn{name: "api_image_digest", kind: "character varying(71)", notNull: true},
			snMainnetMigrationColumn{name: "worker_image_digest", kind: "character varying(71)", notNull: true}),
		snMainnetMigrationConstraints("competition_executor_qualification_recovery", "PRIMARY KEY (round_id, host_id)", "UNIQUE (job_id)",
			"CHECK ((authorization_sha256 ~ '^[0-9a-f]{64}$'::text))",
			"CHECK (((octet_length(authorization_canonical) >= 1) AND (octet_length(authorization_canonical) <= 32768)))",
			"CHECK ((job_id <> '00000000-0000-0000-0000-000000000000'::uuid))",
			"CHECK (((api_image_digest)::text ~ '^sha256:[0-9a-f]{64}$'::text))",
			"CHECK (((worker_image_digest)::text ~ '^sha256:[0-9a-f]{64}$'::text))",
			"CHECK (((convert_from(authorization_canonical, 'UTF8'::name))::jsonb = authorization_json))",
			"CHECK ((encode(sha256(authorization_canonical), 'hex'::text) = (authorization_sha256)::text))",
			"CHECK ((deadline_at = (started_at + '03:00:00'::interval)))"),
		snMainnetMigrationForeignKey("competition_executor_qualification_recovery", "competition_executor_qualification_run", runKey),
		snMainnetMigrationForeignKeyCensus("competition_executor_qualification_recovery", runKey),
		snMainnetMigrationFunction("competition_executor_qualification_recovery_guard()", "trigger", "plpgsql", "v", false, bodies.RecoveryGuard),
		snMainnetMigrationFunction("competition_executor_recovery_job_guard()", "trigger", "plpgsql", "v", false, bodies.RecoveryJobGuard),
		clock,
	}
	for _, trigger := range []struct {
		table, name, function string
		kind                  int
	}{
		{table: "competition_executor_qualification_recovery", name: "competition_executor_qualification_recovery_append_only", function: "competition_append_only_guard", kind: 27},
		{table: "competition_executor_qualification_recovery", name: "competition_executor_qualification_recovery_no_truncate", function: "competition_append_only_guard", kind: 34},
		{table: "competition_executor_qualification_recovery", name: "competition_executor_qualification_recovery_identity", function: "competition_executor_qualification_recovery_guard", kind: 7},
		{table: "competition_job", name: "zz_competition_executor_recovery_job_identity", function: "competition_executor_recovery_job_guard", kind: 7},
	} {
		queries = append(queries, snMainnetMigrationTrigger(trigger.table, trigger.name, trigger.function, trigger.kind))
	}
	return migrationCurrentSuffix(server.CompetitionExecutorQualificationRecoveryMigrationVersion(), "single-use executor qualification recovery with retained executor control amendment custody", queries...)
}()
