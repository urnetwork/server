// The epoch-one official review authorization (server
// competition_official_review_migration.go) belongs to the migration catalog.
// The migration re-creates the winner source and winner review functions the
// review handoff custody pins, and the review identity function that custody
// carries on, so it retires that custody at its own version and carries all of
// it on, with only those three bodies replaced by the ones it publishes.
package monitor

import (
	"strings"

	"github.com/urnetwork/server"
)

// Every inherited handoff predicate survives; only the exact winner source,
// winner review and review identity bodies move to the ones with the official
// review exception. The authorization table, its key, checks and round key, its
// three functions and its append-only and insert guards are new.
var competitionOfficialReviewMigrationContract = func() snMainnetMigrationContract {
	bodies := server.CompetitionOfficialReviewBodiesSql()
	handoff := server.CompetitionReviewHandoffBodiesSql()
	_, _, _, _, identityGuard, _, _, _ := server.CompetitionSnowTwoMemberWinnerReviewBodiesSql()
	replace := func(query, old, next string) string {
		old = snMainnetMigrationLiteral(old)
		if strings.Count(query, old) != 1 {
			panic("official review predecessor custody body is absent or ambiguous")
		}
		return strings.Replace(query, old, snMainnetMigrationLiteral(next), 1)
	}
	inherited := competitionReviewHandoffMigrationContract.query
	inherited = replace(inherited, handoff.WinnerSourceAuthorized, bodies.WinnerSourceAuthorized)
	inherited = replace(inherited, handoff.WinnerReviewGuard, bodies.WinnerReviewGuard)
	inherited = replace(inherited, identityGuard, bodies.ReviewIdentityGuard)
	const roundKey = "FOREIGN KEY (round_id) REFERENCES competition_round(round_id)"
	findingsValid := snMainnetMigrationFunction("competition_official_review_findings_valid(jsonb)", "boolean", "sql", "i", false, bodies.FindingsValid, "request")
	findingsValid = strings.Replace(findingsValid, "p.proparallel='u'", "p.proparallel='s'", 1)
	queries := []string{
		inherited,
		migrationFp2Table("competition_official_review_authorization"),
		snMainnetMigrationColumns("competition_official_review_authorization",
			snMainnetMigrationColumn{name: "authorization_sha256", kind: "text", notNull: true},
			snMainnetMigrationColumn{name: "round_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "actor_id", kind: "text", notNull: true},
			snMainnetMigrationColumn{name: "import_sha256", kind: "text", notNull: true},
			snMainnetMigrationColumn{name: "import_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("competition_official_review_authorization", "PRIMARY KEY (authorization_sha256)",
			"CHECK ((authorization_sha256 ~ '^[0-9a-f]{64}$'::text))",
			"CHECK ((import_sha256 ~ '^[0-9a-f]{64}$'::text))"),
		snMainnetMigrationForeignKey("competition_official_review_authorization", "competition_round", roundKey),
		snMainnetMigrationForeignKeyCensus("competition_official_review_authorization", roundKey),
		findingsValid,
		snMainnetMigrationFunction("competition_official_review_request_authorized(uuid,jsonb)", "boolean", "sql", "s", false, bodies.RequestAuthorized,
			"round_uuid", "request"),
		snMainnetMigrationFunction("competition_official_review_authorization_guard()", "trigger", "plpgsql", "v", false, bodies.AuthorizationGuard),
		snMainnetMigrationTrigger("competition_official_review_authorization", "competition_official_review_immutable", "competition_append_only_guard", 27),
		snMainnetMigrationTrigger("competition_official_review_authorization", "competition_official_review_authorization_before_insert", "competition_official_review_authorization_guard", 7),
	}
	return migrationCurrentSuffix(server.CompetitionOfficialReviewMigrationVersion(), "epoch-one official review authorization with retained review handoff custody", queries...)
}()
