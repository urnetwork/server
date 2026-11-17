// The transport capability upgrade preserves every historical round fence.
package server

import (
	"strings"
	"testing"
)

// Compare the whole guard, not a few keywords, to prohibit accidental changes
// to the seven-day production cadence, zero-fee staging or predecessor fences.
func TestCompetitionRuntimeBlobMigrationChangesOnlyCapability(t *testing.T) {
	start := "CREATE OR REPLACE FUNCTION competition_v2_round_insert_guard()"
	previous := competitionAutomationStagingMigration[strings.Index(competitionAutomationStagingMigration, start):]
	previous = previous[:strings.Index(previous, "END $guard$;")+len("END $guard$;")]
	updated := competitionAutomationRuntimeBlobMigration[strings.Index(competitionAutomationRuntimeBlobMigration, start):]
	updated = updated[:strings.Index(updated, "END $guard$;")+len("END $guard$;")]
	expected := strings.Replace(previous, "policy->>'required_api_capability' IS DISTINCT FROM 'competition.reward.v2'", "COALESCE(policy->>'required_api_capability','') NOT IN ('competition.reward.v2','competition.reward.v2.runtime-blob.v1')", 1)
	if previous == expected || updated != expected {
		t.Fatal("runtime blob migration changed a historical round fence")
	}
}
