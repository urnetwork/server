// Binds a preannounced admission time in the database independently of the CLI.
package server

import "strings"

// Reuses the latest complete round guard, preserving winner policy, runtime,
// source mode, full-window and readiness requirements for scheduled rounds.
func competitionScheduledOpeningMigration() string {
	latest := competitionWinnerReviewMigration()
	start := strings.Index(latest, "CREATE OR REPLACE FUNCTION competition_v2_round_insert_guard")
	if start < 0 {
		panic("latest round authorization migration is absent")
	}
	round := latest[start:]
	anchor := "  OR NEW.closes_at-NEW.opens_at<>make_interval(secs=>window_seconds)"
	if strings.Count(round, anchor) != 1 {
		panic("latest round schedule guard is ambiguous")
	}
	return "SET LOCAL lock_timeout = '5s';\n" + strings.Replace(round, anchor,
		"  OR ((intent->>'opens_at')::timestamp>'1970-01-01'::timestamp AND NEW.opens_at IS DISTINCT FROM (intent->>'opens_at')::timestamp)\n"+anchor, 1)
}
