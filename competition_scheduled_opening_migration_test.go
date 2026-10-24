// The database must independently bind the source intent's announced schedule.
package server

import (
	"strings"
	"testing"
)

// Installing the schedule guard must not reinstall obsolete source-count guards.
func TestCompetitionScheduledOpeningMigrationPreservesRoundAuthority(t *testing.T) {
	migration := competitionScheduledOpeningMigration()
	for _, expected := range []string{
		"CREATE OR REPLACE FUNCTION competition_v2_round_insert_guard",
		"NEW.opens_at IS DISTINCT FROM (intent->>'opens_at')::timestamp",
		"round and activated source closing policy disagree",
		"competition_v2_require_source_lease", "PERFORM competition_v2_source_mode(intent)",
		"NEW.closes_at-NEW.opens_at<>make_interval(secs=>window_seconds)",
		"source_lock_sha256", "host_qualification_sha256", "NEW.opens_at<activated",
	} {
		if !strings.Contains(migration, expected) {
			t.Fatalf("schedule guard lost %q", expected)
		}
	}
	for _, forbidden := range []string{"CREATE OR REPLACE FUNCTION competition_v2_source_authorized", "CREATE OR REPLACE FUNCTION competition_v2_source_graph_valid", "UPDATE competition_"} {
		if strings.Contains(migration, forbidden) {
			t.Fatalf("schedule migration rewrites unrelated source authority or evidence: %q", forbidden)
		}
	}
}
