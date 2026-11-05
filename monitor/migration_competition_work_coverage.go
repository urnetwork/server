// Coverage custody is wholly additive. No historical source, review or
// eligibility owner retires when the new instrument contract is registered.
package monitor

import (
	"strings"

	"github.com/urnetwork/server"
)

// The integration owner registers this contract only after appending the SQL
// migration. An absent registration cannot silently become a version-zero pin.
func competitionWorkCoverageMigrationContract() snMainnetMigrationContract {
	version := server.CompetitionWorkCoverageMigrationVersion()
	if version == 0 {
		panic("coverage custody requires its appended migration")
	}
	return migrationCurrentSuffix(version, "seeded crawl coverage policy and eligibility custody", competitionWorkCoverageArtifactQueries()...)
}

// Exact bodies, metadata and unrestricted enabled triggers jointly establish
// authority; name-only or INSERT-only replacements cannot satisfy this probe.
func competitionWorkCoverageArtifactQueries() []string {
	bodies := server.CompetitionWorkCoverageBodiesSql()
	production, staging := server.CompetitionWorkCoverageLegacyEligibilityBodiesSql()
	policyQuery := snMainnetMigrationFunction("competition_work_coverage_policy_valid(jsonb)", "boolean", "sql", "i", false, bodies["policy"], "policy")
	scoreQuery := snMainnetMigrationFunction("competition_work_coverage_score_valid(jsonb,jsonb)", "boolean", "plpgsql", "i", false, bodies["score"], "score", "evaluation_policy")
	queries := []string{
		strings.Replace(policyQuery, "p.proparallel='u'", "p.proparallel='s'", 1),
		strings.Replace(scoreQuery, "p.proparallel='u'", "p.proparallel='s'", 1),
		snMainnetMigrationFunction("competition_v2_score_eligible_policy(jsonb,double precision,double precision,boolean,jsonb)", "boolean", "plpgsql", "i", false, bodies["eligible"], "score", "baseline_raw", "margin", "staging_mode", "evaluation_policy"),
		snMainnetMigrationFunction("competition_v2_score_eligible(jsonb,double precision,double precision)", "boolean", "sql", "i", false, production, "score", "baseline_raw", "margin"),
		snMainnetMigrationFunction("competition_v2_score_eligible_mode(jsonb,double precision,double precision,boolean)", "boolean", "sql", "i", false, staging, "score", "baseline_raw", "margin", "staging_mode"),
	}
	for _, guard := range []struct {
		name, table string
		mask        int
	}{
		{name: "job", table: "competition_job", mask: 23},
		{name: "baseline", table: "competition_round_baseline", mask: 7},
		{name: "seal", table: "competition_evaluation_seal", mask: 7},
		{name: "source", table: "competition_source_activation", mask: 23},
		{name: "round", table: "competition_round", mask: 23},
		{name: "equivalence", table: "competition_executor_equivalence", mask: 7},
	} {
		function := "competition_work_coverage_" + guard.name + "_guard"
		queries = append(queries, snMainnetMigrationFunction(function+"()", "trigger", "plpgsql", "v", false, bodies[guard.name]),
			snMainnetMigrationTrigger(guard.table, "competition_work_coverage_"+guard.name, function, guard.mask))
	}
	return queries
}
