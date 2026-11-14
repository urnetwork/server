// The additive source-graph migration preserves historical definitions and changes only prospective mode authority.
package server

import (
	"strings"
	"testing"
)

// The independent pointer/round mode guard and the full source authorization must share the same exact graph.
func TestCompetitionStagingSourceGraphMigrationBindsAllSourceBoundaries(t *testing.T) {
	for _, required := range []string{
		"competition_v2_source_graph_valid", "THEN 10 ELSE 11 END", "THEN 11 ELSE 12 END",
		"(commits ? 'operator-proxy')=(NOT staging_mode)", "'warp','gvisor'",
		"CREATE OR REPLACE FUNCTION competition_v2_source_mode", "CREATE OR REPLACE FUNCTION competition_v2_source_authorized",
		"PERFORM competition_v2_source_mode(intent)", "source_lock_sha256", "reviewed_source_promotion",
	} {
		if !strings.Contains(competitionAutomationStagingSourceGraphMigration, required) {
			t.Fatalf("new source graph guard lacks %q", required)
		}
	}
	if !strings.Contains(competitionAutomationStagingMigration, "jsonb_object_keys(intent->'commits'))<>11") || !strings.Contains(competitionAutomationStagingMigration, "jsonb_object_keys(intent->'git_refs'))<>12") {
		t.Fatal("already-applied staging migration was edited")
	}
}
