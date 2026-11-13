// Migration 946's catalog identity, for the monitor's schema contract
// (monitor/migration_competition_review_handoff.go). Reads the published SQL;
// changes nothing it publishes.
package server

import "strings"

// The position of the pending review handoff migration, wherever merges place
// it; 0 when it is not registered.
func CompetitionReviewHandoffMigrationVersion() int {
	sql := competitionReviewHandoffMigration()
	version := 0
	for index, migration := range migrations {
		if value, ok := migration.(*SqlMigration); ok && value.sql == sql {
			if version != 0 {
				panic("review handoff migration is registered more than once")
			}
			version = index + 1
		}
	}
	return version
}

// The exact function bodies migration 946 publishes: its ledger's own
// functions, and the three winner-review functions it re-creates with the
// handoff branches.
type CompetitionReviewHandoffBodies struct {
	Clock                  string
	OwnerGuard             string
	CallGuard              string
	CompletionGuard        string
	Observed               string
	ImportGuard            string
	WinnerSourceAuthorized string
	WinnerReviewGuard      string
	WinnerCheckpointGuard  string
}

// Each body is the text between its dollar-quote tags, as the catalog stores it.
func CompetitionReviewHandoffBodiesSql() CompetitionReviewHandoffBodies {
	sql := competitionReviewHandoffMigration()
	body := func(tag string) string {
		parts := strings.Split(sql, "$"+tag+"$")
		if len(parts) != 3 {
			panic("review handoff body " + tag + " is absent or ambiguous")
		}
		return parts[1]
	}
	return CompetitionReviewHandoffBodies{
		Clock:                  body("handoff_clock"),
		OwnerGuard:             body("handoff_owner"),
		CallGuard:              body("handoff_call"),
		CompletionGuard:        body("handoff_complete"),
		Observed:               body("handoff_observed"),
		ImportGuard:            body("handoff_import"),
		WinnerSourceAuthorized: body("winner_source"),
		WinnerReviewGuard:      body("winner_review"),
		WinnerCheckpointGuard:  body("winner_checkpoint"),
	}
}
