// Candidate custody is checked directly before registration, including actual
// PostgreSQL catalogs. The integration owns suffix registration and retirement.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Inherited custody predicates share catalog CTEs with the native emitter.
// Reuse that exact prefix; a naked SELECT is not the production query shape.
func competitionIntakeFirstCatalogTestQuery(t testing.TB) string {
	t.Helper()
	return snMainnetMigrationProbePrefix(t) + "SELECT " + competitionIntakeFirstArtifactQuery()
}

// Every replacement is a reviewed intake-lineage successor. Post-close
// recovery is a separate lineage which joins only in the final composite.
// Do not follow an arbitrary registered retirement or waive an old predicate.
func competitionIntakeFirstCatalogTestPredicateAt(t testing.TB, head int) string {
	t.Helper()
	query := competitionIntakeFirstArtifactQuery()
	previous := server.CompetitionIntakeFirstMigrationVersion()
	if previous == 0 {
		return query
	}
	for _, successor := range []snMainnetMigrationContract{
		competitionReviewHandoffMigrationContract,
		competitionOfficialReviewMigrationContract,
		competitionExecutorControlAmendmentMigrationContract,
		competitionExecutorQualificationRecoveryMigrationContract,
		competitionExecutorAdmissionExceptionMigrationContract,
		competitionExecutorAdmissionClaimMigrationContract,
		competitionDistributedReevaluationMigrationContract,
	} {
		version := successor.artifact.requiredVersion
		prior := migrationCurrentSuffixTestContract(t, previous)
		registered := migrationCurrentSuffixTestContract(t, version)
		if version <= previous || prior.artifact.removedVersion != version || registered.query != successor.query {
			t.Fatal("intake catalog has an unreviewed successor or changed registered predicate", previous, version)
		}
		if version <= head {
			query = successor.query
		}
		previous = version
	}
	if migrationCurrentSuffixTestContract(t, previous).artifact.removedVersion != 0 {
		t.Fatal("intake catalog final owner has an unreviewed successor")
	}
	return query
}

// The old fixture stopped at AdmissionException and therefore rejected a
// coherent Claim/current schema. Exact-byte boundary controls fail before a
// database opens if either known successor is accidentally omitted again.
func TestMigrationsCompetitionIntakeFirstCatalogSelectsExactPublishedOwner(t *testing.T) {
	owners := []snMainnetMigrationContract{
		migrationCurrentSuffixTestContract(t, server.CompetitionIntakeFirstMigrationVersion()),
		competitionReviewHandoffMigrationContract,
		competitionOfficialReviewMigrationContract,
		competitionExecutorControlAmendmentMigrationContract,
		competitionExecutorQualificationRecoveryMigrationContract,
		competitionExecutorAdmissionExceptionMigrationContract,
		competitionExecutorAdmissionClaimMigrationContract,
		competitionDistributedReevaluationMigrationContract,
	}
	for index, owner := range owners {
		heads := []int{owner.artifact.requiredVersion}
		if index+1 < len(owners) {
			heads = append(heads, owners[index+1].artifact.requiredVersion-1)
		} else {
			heads = append(heads, owner.artifact.requiredVersion+1, server.MigrationCount())
		}
		for _, head := range heads {
			if query := competitionIntakeFirstCatalogTestPredicateAt(t, head); query != owner.query {
				t.Fatal("intake catalog selected an obsolete/future body at the wrong boundary", head, owner.artifact.requiredVersion)
			}
		}
	}
}

// Removing the prefix deterministically loses index_artifact before a fault
// can be tested, even when the complete deployed schema is healthy.
func TestMigrationsCompetitionIntakeFirstCatalogQueryRetainsNativePrefix(t *testing.T) {
	query := competitionIntakeFirstCatalogTestQuery(t)
	if !strings.HasPrefix(strings.TrimSpace(query), "WITH ") || !strings.Contains(query, "index_artifact AS (") ||
		!strings.HasSuffix(query, "SELECT "+competitionIntakeFirstArtifactQuery()) {
		t.Fatal("intake catalog fixture detached its inherited native CTE authority")
	}
}

// No predecessor-wide waiver is manufactured. Its full query, changed only at
// the exact replaced body, remains part of the new successor artifact.
func TestMigrationsCompetitionIntakeFirstCarriesExactPredecessorCustody(t *testing.T) {
	query := competitionIntakeFirstArtifactQuery()
	bodies := map[string]string{}
	for _, function := range server.CompetitionIntakeFirstFunctionsSql() {
		bodies[function.Signature] = function.Body
		if !strings.Contains(query, "p.prosrc="+snMainnetMigrationLiteral(function.Body)) {
			t.Fatalf("intake custody omitted exact executable body %s", function.Signature)
		}
	}
	round, reserved, baseline := server.CompetitionIntakeFirstLegacyBodiesSql()
	for _, predecessor := range []struct{ query, old, next string }{
		{query: competitionProductionBenefitMigrationContract.query, old: round, next: bodies["competition_v2_round_insert_guard()"]},
		{query: competitionMigration846ArtifactQuery, old: reserved, next: bodies["competition_v2_round_reserved_identity_guard()"]},
		{query: competitionDistributedEvaluationMigrationContract.query, old: baseline, next: bodies["competition_round_baseline_insert_guard()"]},
		{query: "(" + strings.Join(competitionWorkCoverageArtifactQueries(), " AND ") + ")", old: server.CompetitionIntakeFirstLegacyCoverageRoundBodySql(), next: bodies["competition_work_coverage_round_guard()"]},
	} {
		want := strings.Replace(predecessor.query, snMainnetMigrationLiteral(predecessor.old), snMainnetMigrationLiteral(predecessor.next), 1)
		if !strings.Contains(query, want) || strings.Contains(query, "p.prosrc="+snMainnetMigrationLiteral(predecessor.old)) {
			t.Fatal("intake successor lost inherited custody or retained an obsolete executable body")
		}
	}
	for _, required := range []string{"competition_source_operator_handoff", "request_canonical", "grant_canonical", "round_policy_canonical", "api_readiness_canonical", "competition_source_intake_activation_no_truncate", "competition_source_intake_pointer_identity", "competition_01_intake_execution", "competition_01_intake_review_fence", "competition_01_intake_decision_fence", "NOT p.prosecdef", "p.provolatile='v'", "RI_FKey_check_ins"} {
		if !strings.Contains(query, required) {
			t.Fatalf("intake custody omitted schema/authority %s", required)
		}
	}
}

// A healthy complete new schema is the positive control. Each individual fault
// is rolled back; an unregistered migration is installed only in TestEnv's DB.
func TestMigrationsCompetitionIntakeFirstActualCatalogFaults(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		var actualHead int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(max(end_version_number),0) FROM migration_audit WHERE status='success'`).Scan(&actualHead))
		})
		if actualHead != server.MigrationCount() {
			tb.Fatalf("intake catalog fixture schema=%d want=%d", actualHead, server.MigrationCount())
		}
		tb.Logf("intake_catalog_fixture_schema_head=%d", actualHead)
		if server.CompetitionIntakeFirstMigrationVersion() == 0 {
			server.Tx(ctx, func(tx server.PgTx) { server.RaisePgResult(tx.Exec(ctx, server.CompetitionIntakeFirstMigrationSql())) }, server.OptReadWrite())
		}
		query := snMainnetMigrationProbePrefix(tb) + "SELECT " + competitionIntakeFirstCatalogTestPredicateAt(tb, actualHead)
		// This is one complete source-owned predicate, not the whole census.
		// Refuse an oversized successor instead of recreating unbounded planning.
		if len(query) > DefaultMigrationCensusSettings().MaximumStatementBytes {
			tb.Fatal("direct intake catalog predicate exceeds bounded statement admission")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var healthy bool
			if err := conn.QueryRow(ctx, query).Scan(&healthy); err != nil || !healthy {
				tb.Fatalf("complete intake candidate catalog is not coherent: %v", err)
			}
		})
		for _, fault := range []struct{ name, sql string }{
			{name: "claim clock barrier disabled", sql: `ALTER TABLE competition_job DISABLE TRIGGER competition_01_intake_execution`},
			{name: "human history mutable", sql: `ALTER TABLE competition_source_operator_handoff DISABLE TRIGGER competition_source_operator_handoff_append_only`},
			{name: "grant truncate bypass", sql: `ALTER TABLE competition_source_intake_activation DISABLE TRIGGER competition_source_intake_activation_no_truncate`},
			{name: "readiness helper weakened", sql: `CREATE OR REPLACE FUNCTION competition_v2_intake_grading_ready(target uuid) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN RETURN true; END $$`},
			{name: "handshake privilege changed", sql: `ALTER FUNCTION competition_v2_intake_handoff_guard() SECURITY DEFINER`},
			{name: "source snapshot fence omitted", sql: `ALTER TABLE competition_reward_decision DISABLE TRIGGER competition_01_intake_decision_fence`},
			{name: "old round body restored", sql: `CREATE OR REPLACE FUNCTION competition_v2_round_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $old$` + func() string { value, _, _ := server.CompetitionIntakeFirstLegacyBodiesSql(); return value }() + `$old$`},
			{name: "new raw bytes dropped", sql: `ALTER TABLE competition_source_intake_activation ALTER COLUMN grant_canonical DROP NOT NULL`},
			{name: "retained source authority weakened", sql: `CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb,env text,competition text,season text) RETURNS void LANGUAGE plpgsql AS $$ BEGIN RETURN; END $$`},
			{name: "retained executor clock weakened", sql: `CREATE OR REPLACE FUNCTION competition_executor_job_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`},
			{name: "old coverage round body restored", sql: `CREATE OR REPLACE FUNCTION competition_work_coverage_round_guard() RETURNS trigger LANGUAGE plpgsql AS $old$` + server.CompetitionIntakeFirstLegacyCoverageRoundBodySql() + `$old$`},
			{name: "retained coverage source guard disabled", sql: `ALTER TABLE competition_source_activation DISABLE TRIGGER competition_work_coverage_source`},
		} {
			snMainnetMigrationFaultTx(tb, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					tb.Fatalf("inject %s: %v", fault.name, err)
				}
				var healthy bool
				if err := tx.QueryRow(ctx, query).Scan(&healthy); err != nil || healthy {
					tb.Fatalf("intake custody failed to detect %s: %v", fault.name, err)
				}
			})
		}
	})
}
