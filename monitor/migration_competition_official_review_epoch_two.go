// The reviewer-only amendment carries the complete current recovery, source,
// calibration and native-handoff custody, replacing five exact function bodies.
package monitor

import (
	"strings"

	"github.com/urnetwork/server"
)

// No inherited predicate is retired without being owned by this successor.
var competitionOfficialReviewEpochTwoMigrationContract = func() snMainnetMigrationContract {
	previous := server.CompetitionOfficialReviewBodiesSql()
	bodies := server.CompetitionOfficialReviewEpochTwoBodiesSql()
	query := competitionSecondRecoveryMigrationContract.query
	for _, replacement := range []struct{ previous, next string }{
		{previous: previous.RequestAuthorized, next: bodies.RequestAuthorized},
		{previous: previous.AuthorizationGuard, next: bodies.AuthorizationGuard},
		{previous: previous.ReviewIdentityGuard, next: bodies.ReviewIdentityGuard},
		{previous: previous.WinnerSourceAuthorized, next: bodies.WinnerSourceAuthorized},
		{previous: previous.WinnerReviewGuard, next: bodies.WinnerReviewGuard},
	} {
		old := snMainnetMigrationLiteral(replacement.previous)
		if strings.Count(query, old) != 1 {
			panic("epoch-two official review predecessor custody absent or ambiguous")
		}
		query = strings.Replace(query, old, snMainnetMigrationLiteral(replacement.next), 1)
	}
	findings := snMainnetMigrationFunction("competition_official_review_epoch_two_findings_valid(jsonb)", "boolean", "plpgsql", "i", false, bodies.FindingsValid, "request")
	findings = strings.Replace(findings, "p.proparallel='u'", "p.proparallel='s'", 1)
	return migrationCurrentSuffix(server.CompetitionOfficialReviewEpochTwoMigrationVersion(), "epoch-two official reviewer substitution with retained complete custody", query,
		findings,
		snMainnetMigrationFunction("competition_official_review_epoch_two_binding(uuid,jsonb,timestamp without time zone)", "boolean", "sql", "s", false, bodies.PrereviewBinding,
			"round_uuid", "request", "imported_at"))
}()
