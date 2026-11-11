// Migration 949's catalog identity, for the monitor's schema contract
// (monitor/migration_competition_official_review.go). Reads the published SQL;
// changes nothing it publishes.
package server

import "strings"

// The position of the epoch-one official review migration, wherever merges
// place it; 0 when it is not registered.
func CompetitionOfficialReviewMigrationVersion() int {
	sql := competitionOfficialReviewMigration()
	version := 0
	for index, migration := range migrations {
		if value, ok := migration.(*SqlMigration); ok && value.sql == sql {
			if version != 0 {
				panic("official review migration is registered more than once")
			}
			version = index + 1
		}
	}
	return version
}

// The exact function bodies migration 949 publishes: its authorization's own
// functions, and the three review functions it re-creates with the official
// review exception (the winner source and winner review bodies of the review
// handoff, and the two-member review identity body).
type CompetitionOfficialReviewBodies struct {
	FindingsValid          string
	RequestAuthorized      string
	AuthorizationGuard     string
	WinnerSourceAuthorized string
	WinnerReviewGuard      string
	ReviewIdentityGuard    string
}

// Each body is the text between its dollar-quote tags, as the catalog stores it.
func CompetitionOfficialReviewBodiesSql() CompetitionOfficialReviewBodies {
	sql := competitionOfficialReviewMigration()
	body := func(tag string) string {
		parts := strings.Split(sql, "$"+tag+"$")
		if len(parts) != 3 {
			panic("official review body " + tag + " is absent or ambiguous")
		}
		return parts[1]
	}
	return CompetitionOfficialReviewBodies{
		FindingsValid:          body("official_findings"),
		RequestAuthorized:      body("official_authorized"),
		AuthorizationGuard:     body("official_grant"),
		WinnerSourceAuthorized: body("winner_source"),
		WinnerReviewGuard:      body("winner_review"),
		ReviewIdentityGuard:    body("guard"),
	}
}
