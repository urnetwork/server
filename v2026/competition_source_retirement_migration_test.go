// Prospective retirement must retain all source authority beyond its key census.
package server

import (
	"strings"
	"testing"
)

// A deployment cannot accidentally restore the old source guard and drop the
// elected-winner, exact-source, lease, or readiness requirements.
func TestCompetitionSourceRetirementMigrationPreservesWinnerAuthority(t *testing.T) {
	migration := competitionSourceRetirementMigration()
	for _, expected := range []string{
		"CREATE OR REPLACE FUNCTION competition_v2_source_graph_valid",
		"jsonb_object_keys(commits))=10",
		"'server','connect','sdk','proxy','glog','goidenticons','userwireguard','sn','warp','gvisor'",
		"jsonb_object_keys(intent->'git_refs'))<>11",
		"PERFORM competition_v2_source_mode(intent)",
		"competition_v2_winner_source_authorized(intent,predecessor_id)",
		"source_lock_sha256", "policy_approval", "reviewed_source_promotion",
	} {
		if !strings.Contains(migration, expected) {
			t.Fatalf("retirement migration lost source authority %q", expected)
		}
	}
	for _, forbidden := range []string{"operator-proxy", "THEN 11 ELSE 12 END", "CREATE TRIGGER", "UPDATE competition_"} {
		if strings.Contains(migration, forbidden) {
			t.Fatalf("retirement rewrites evidence or retains old requirements: %q", forbidden)
		}
	}
}
