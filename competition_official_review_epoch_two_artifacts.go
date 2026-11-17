// Catalog custody follows the appended epoch-two definitions without changing
// the frozen epoch-one migration or any retained authorization bytes.
package server

import "strings"

// The registered position follows all published migrations, including merges.
func CompetitionOfficialReviewEpochTwoMigrationVersion() int {
	sql := competitionOfficialReviewEpochTwoMigration()
	version := 0
	for index, migration := range migrations {
		if value, ok := migration.(*SqlMigration); ok && value.sql == sql {
			if version != 0 {
				panic("epoch-two official review migration registered twice")
			}
			version = index + 1
		}
	}
	return version
}

// Exact new and replaced bodies allow the monitor to retain every inherited
// predicate while pinning only this amendment's catalog changes.
type CompetitionOfficialReviewEpochTwoBodies struct {
	FindingsValid          string
	PrereviewBinding       string
	RequestAuthorized      string
	AuthorizationGuard     string
	ReviewIdentityGuard    string
	WinnerSourceAuthorized string
	WinnerReviewGuard      string
}

// The catalog stores precisely the contents between these unique quote tags.
func CompetitionOfficialReviewEpochTwoBodiesSql() CompetitionOfficialReviewEpochTwoBodies {
	sql := competitionOfficialReviewEpochTwoMigration()
	body := func(tag string) string {
		parts := strings.Split(sql, "$"+tag+"$")
		if len(parts) != 3 {
			panic("epoch-two official review body absent or ambiguous: " + tag)
		}
		return parts[1]
	}
	return CompetitionOfficialReviewEpochTwoBodies{
		FindingsValid: body("official_two_findings"), PrereviewBinding: body("official_two_binding"),
		RequestAuthorized: body("official_authorized"), AuthorizationGuard: body("official_grant"),
		ReviewIdentityGuard: body("guard"), WinnerSourceAuthorized: body("winner_source"), WinnerReviewGuard: body("winner_review"),
	}
}
