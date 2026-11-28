// Each correction changes functions in place. Their original bodies, their
// replacements and the independent journal checks have distinct lifetimes.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// A blanket retirement of either predecessor would lose its unchanged guards.
func TestMigrationsEpochThreeRuntimeCorrectionFunctionCustody(t *testing.T) {
	version := server.CompetitionEpochThreeRuntimeCorrectionMigrationVersion()
	correction := migrationCurrentSuffixTestContract(t, version)
	source := migrationCurrentSuffixTestContract(t, 844)
	intake := migrationCurrentSuffixTestContract(t, server.CompetitionEpochThreeSourceRecoveryMigrationVersion())
	if source.artifact.removedVersion != 0 || intake.artifact.removedVersion != 0 || version <= intake.artifact.requiredVersion {
		t.Fatal("runtime correction retired or preceded an independent catalog owner")
	}
	bodies := server.CompetitionEpochThreeRuntimeCorrectionBodiesSql()
	check := func(prior snMainnetMigrationContract, signature, result, oldBody, newBody string, arguments ...string) {
		t.Helper()
		oldPin := snMainnetMigrationFunction(signature, result, "plpgsql", "v", false, oldBody, arguments...)
		newPin := snMainnetMigrationFunction(signature, result, "plpgsql", "v", false, newBody, arguments...)
		if strings.Count(prior.query, oldPin) != 1 || !strings.Contains(prior.query, migrationCompetitionBefore(version, oldPin)) ||
			strings.Contains(correction.query, oldPin) || strings.Count(correction.query, newPin) != 1 {
			t.Fatalf("wrong historical or new executable custody for %s at v%d", signature, version)
		}
	}
	check(source, "competition_v2_source_readiness_guard()", "trigger", competitionMigration844CompetitionV2SourceReadinessGuardBody, bodies.SourceReadinessGuard)
	checked := 0
	for _, function := range server.CompetitionIntakeFirstFunctionsSql() {
		switch function.Signature {
		case competitionIntakeRuntimeReadySignature:
			check(intake, function.Signature, function.Result, function.Body, bodies.IntakeRuntimeReady, function.Arguments...)
			checked++
		case competitionIntakeBaselineValidSignature:
			check(intake, function.Signature, function.Result, function.Body, bodies.IntakeBaselineValid, function.Arguments...)
			checked++
		}
	}
	if checked != 2 {
		t.Fatal("missing published runtime or baseline predecessor")
	}
}

// A missing successor is actionable only at a source-known, applied head.
func TestMigrationsEpochThreeRuntimeCorrectionSignalBoundary(t *testing.T) {
	testEpochThreeCorrectionSignalBoundary(t, server.CompetitionEpochThreeRuntimeCorrectionMigrationVersion())
}

// The builder append keeps the amendment journal and its trigger at the prior
// owner, while the new function has its own exact published contract.
func TestMigrationsEpochThreeExecutorBuilderCorrectionCustody(t *testing.T) {
	version := server.CompetitionEpochThreeExecutorBuilderCorrectionMigrationVersion()
	runtimeVersion := server.CompetitionEpochThreeRuntimeCorrectionMigrationVersion()
	prior := migrationCurrentSuffixTestContract(t, server.CompetitionEpochThreeSourceRecoveryMigrationVersion())
	previous := snMainnetMigrationFunction(competitionExecutorAmendmentGuardSignature, "trigger", "plpgsql", "v", false, server.CompetitionExecutorControlAmendmentBodiesSql().AmendmentGuard)
	next := snMainnetMigrationFunction(competitionExecutorAmendmentGuardSignature, "trigger", "plpgsql", "v", false, server.CompetitionEpochThreeExecutorBuilderCorrectionBodySql())
	trigger := snMainnetMigrationTrigger("competition_executor_control_amendment", "competition_executor_control_amendment_identity", "competition_executor_control_amendment_guard", 7)
	if version <= runtimeVersion || prior.artifact.removedVersion != 0 || migrationCurrentSuffixTestContract(t, runtimeVersion).artifact.removedVersion != 0 ||
		strings.Count(prior.query, previous) != 1 || !strings.Contains(prior.query, migrationCompetitionBefore(version, previous)) ||
		!strings.Contains(prior.query, trigger) || next == previous || migrationCurrentSuffixTestContract(t, version).query != next {
		t.Fatal("builder correction lost its exact function handoff or independent custody")
	}
	testEpochThreeCorrectionSignalBoundary(t, version)
}

// Exercise the public reducer below publication, at publication and above its
// source authority. A later known migration may also cause numeric lag.
func testEpochThreeCorrectionSignalBoundary(t *testing.T, version int) {
	t.Helper()
	artifact := migrationCurrentSuffixTestContract(t, version).artifact
	for _, test := range []struct {
		head  int
		class string
	}{
		{version - 1, "migration-behind"},
		{version, "migration-schema-drift"},
		{server.MigrationCount() + 1, "cannot-observe"},
	} {
		row := syntheticMigrationMissingArtifactRow(t, test.head, artifact.name)
		catalog := syntheticMigrationCatalogRows(min(test.head, server.MigrationCount()))
		if test.head > server.MigrationCount() {
			catalog = migrationFutureSchemaCatalogRows()
		}
		alerts := migrationFutureSchemaAlerts(t, NewMigrationsSignal(), artifact.name, row, catalog)
		alert := requireAlertClass(t, alerts, test.class)
		if test.class == "migration-schema-drift" {
			if alert.Severity != SeverityPage || requireAlertClassCount(alerts, "cannot-observe") != 0 ||
				!strings.Contains(alert.Observed, fmt.Sprintf("%s@v%d", artifact.name, version)) {
				t.Fatal("applied correction fault lost its immediate owning page", version)
			}
		} else if requireAlertClassCount(alerts, "migration-schema-drift") != 0 {
			t.Fatalf("v%d fault was incorrectly attributed at head %d", version, test.head)
		}
	}
}

// Read the old definitions from a genuinely migrated prefix, then restore
// them individually on the successor. Every DDL fault rolls back.
func TestMigrationsEpochThreeRuntimeCorrectionPublishedSchemaAndFaults(t *testing.T) {
	version := server.CompetitionEpochThreeRuntimeCorrectionMigrationVersion()
	source := migrationCurrentSuffixTestContract(t, 844)
	intake := migrationCurrentSuffixTestContract(t, server.CompetitionEpochThreeSourceRecoveryMigrationVersion())
	correction := migrationCurrentSuffixTestContract(t, version)
	builder := migrationCurrentSuffixTestContract(t, server.CompetitionEpochThreeExecutorBuilderCorrectionMigrationVersion())
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		prefix := snMainnetMigrationProbePrefix(t)
		checkFault := func(name, sql string, owner snMainnetMigrationContract) {
			t.Helper()
			statement := prefix + "SELECT " + owner.query
			if len(statement) > DefaultMigrationCensusSettings().MaximumStatementBytes {
				t.Fatal("runtime correction fixture exceeded bounded statement admission", name)
			}
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, sql); err != nil {
					t.Fatalf("inject %s: %v", name, err)
				}
				var healthy bool
				if err := tx.QueryRow(ctx, statement).Scan(&healthy); err != nil || healthy {
					t.Fatalf("%s: v%d healthy=%t error=%v", name, owner.artifact.requiredVersion, healthy, err)
				}
			})
		}
		checkPrefix := func(head int, wantRuntime, wantBuilder string) {
			t.Helper()
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			database := &migrationPingDatabaseSource{}
			alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(database))
			wantLag := 0
			if head < server.MigrationCount() {
				wantLag = 1
			}
			if err != nil || len(alerts) != wantLag || requireAlertClassCount(alerts, "migration-behind") != wantLag {
				t.Fatalf("published prefix v%d: alerts=%d lag=%d error=%v", head, len(alerts), requireAlertClassCount(alerts, "migration-behind"), err)
			}
			row := database.artifactRow
			if len(row) <= builder.artifact.rowColumn || row[0] != fmt.Sprint(head) || row[source.artifact.rowColumn] != "t" ||
				row[intake.artifact.rowColumn] != "t" || row[correction.artifact.rowColumn] != wantRuntime || row[builder.artifact.rowColumn] != wantBuilder {
				t.Fatalf("incoherent catalog row for correction prefix v%d", head)
			}
		}
		// Preserve PostgreSQL's complete original definition, then prove that
		// changing only its executable bytes already faults the historical owner.
		priorDefinition := func(signature string, owner snMainnetMigrationContract) string {
			t.Helper()
			var definition, body string
			server.Db(t.Context(), func(conn server.PgConn) {
				server.Raise(conn.QueryRow(t.Context(), "SELECT pg_get_functiondef(oid), prosrc FROM pg_proc WHERE oid=to_regprocedure($1)", "public."+signature).Scan(&definition, &body))
			})
			if strings.Count(definition, body) != 1 {
				t.Fatal("cannot isolate prior body", signature)
			}
			checkFault("changed historical "+signature, strings.Replace(definition, body, body+"\n-- changed body\n", 1), owner)
			return definition
		}
		checkPrefix(version-1, "f", "f")
		functions := []struct {
			signature, definition string
			owner                 snMainnetMigrationContract
		}{
			{signature: "competition_v2_source_readiness_guard()", owner: source},
			{signature: competitionIntakeRuntimeReadySignature, owner: intake},
			{signature: competitionIntakeBaselineValidSignature, owner: intake},
		}
		for i := range functions {
			function := &functions[i]
			function.definition = priorDefinition(function.signature, function.owner)
		}
		checkPrefix(version, "t", "f")
		for _, function := range functions {
			checkFault("restored historical "+function.signature, function.definition, correction)
		}
		if prior := builder.artifact.requiredVersion - 1; prior != version {
			checkPrefix(prior, "t", "f")
		}
		amendment := priorDefinition(competitionExecutorAmendmentGuardSignature, intake)
		checkPrefix(builder.artifact.requiredVersion, "t", "t")
		checkFault("restored prior builder guard", amendment, builder)
		checkFault("amendment insert trigger disabled", "ALTER TABLE competition_executor_control_amendment DISABLE TRIGGER competition_executor_control_amendment_identity", intake)
		for _, fault := range []struct{ name, sql string }{
			{"insert guard privilege", "ALTER FUNCTION competition_epoch_three_runtime_correction_guard() SECURITY DEFINER"},
			{"receipt validator volatility", "ALTER FUNCTION competition_epoch_three_runtime_receipt_valid(jsonb,text,jsonb) VOLATILE"},
			{"nullable journal JSON", "ALTER TABLE competition_epoch_three_runtime_correction ALTER COLUMN correction_json DROP NOT NULL"},
			{"insert trigger disabled", "ALTER TABLE competition_epoch_three_runtime_correction DISABLE TRIGGER competition_epoch_three_runtime_correction_identity"},
			{"append-only trigger disabled", "ALTER TABLE competition_epoch_three_runtime_correction DISABLE TRIGGER competition_epoch_three_runtime_correction_append_only"},
			{"truncate trigger disabled", "ALTER TABLE competition_epoch_three_runtime_correction DISABLE TRIGGER competition_epoch_three_runtime_correction_no_truncate"},
			{"canonical digest check removed", snMainnetMigrationDropConstraint("competition_epoch_three_runtime_correction", "c", "CHECK ((encode(sha256(correction_canonical), 'hex'::text) = (correction_sha256)::text))")},
			{"round foreign key removed", snMainnetMigrationDropConstraint("competition_epoch_three_runtime_correction", "f", "FOREIGN KEY (round_id) REFERENCES competition_round(round_id)")},
		} {
			checkFault(fault.name, fault.sql, correction)
		}
		checkFault("readiness trigger disabled", "ALTER TABLE competition_source_readiness DISABLE TRIGGER competition_v2_readiness_identity", source)
		checkFault("grading guard privilege", "ALTER FUNCTION competition_v2_intake_grading_ready(uuid) SECURITY DEFINER", intake)
	})
}
