// Production eligibility supersedes two exact Snow predicates, never the
// remaining trusted-instrument, source, provider or identity authority.
package monitor

import (
	"strings"

	"github.com/urnetwork/server/v2026"
)

// The predecessor retains its exact checks until this appended version. This
// successor owns all inherited custody plus its new helper and operative trigger.
var competitionProductionBenefitMigrationContract = func() snMainnetMigrationContract {
	lockDigest, productIntent, sourceAuthorized := server.CompetitionTrustedInstrumentBodiesSql()
	valid, guard, findingsV1, findingsV2 := server.CompetitionProductionBenefitBodiesSql()
	validQuery := snMainnetMigrationFunction("competition_v2_production_benefit_review_valid(jsonb)", "boolean", "plpgsql", "i", false, valid, "request")
	validQuery = strings.Replace(validQuery, "p.proparallel='u'", "p.proparallel='s'", 1)
	return migrationCurrentSuffix(server.CompetitionProductionBenefitMigrationVersion(), "production-benefit eligibility with retained review and instrument custody",
		snMainnetMigrationFunction("competition_v2_instrument_lock_digest(jsonb)", "text", "sql", "i", false, lockDigest, "commits"),
		snMainnetMigrationFunction("competition_v2_instrument_product_intent(jsonb,text,text,text)", "jsonb", "plpgsql", "v", false, productIntent, "intent", "env", "competition", "season"),
		competitionSnowTwoMemberCustodyWithFindingsArtifactQuery(sourceAuthorized, findingsV1, findingsV2), validQuery,
		snMainnetMigrationFunction("competition_v2_production_benefit_review_guard()", "trigger", "plpgsql", "v", false, guard),
		snMainnetMigrationTrigger("competition_integrity_review", "competition_v2_production_benefit_review", "competition_v2_production_benefit_review_guard", 7))
}()
