// The new migration extends already-applied automation guards without changing their historical bytes.
package server

import (
	"strings"
	"testing"
)

// Explicit mode checks must cover every independently writable routing boundary.
func TestCompetitionAutomationStagingMigrationGuardsEveryModeBoundary(t *testing.T) {
	for _, required := range []string{
		"competition_round_v2_staging_epoch", "competition_round_one_active_staging",
		"CASE WHEN COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false)",
		"competition_v2_source_mode", "competition_v2_score_eligible_mode",
		"competition_v2_readiness_mode_guard", "competition_v2_pointer_mode_guard", "competition_v2_decision_mode_guard",
		"sim-latency-staging", "submission_fee_usd", "'shadow'", "NEW.epoch_number>6",
		"NEW.status NOT IN ('received','rejected')", "receipt->>'applied_at' IS NOT NULL",
	} {
		if !strings.Contains(competitionAutomationStagingMigration, required) {
			t.Fatalf("staging migration lacks %q", required)
		}
	}
	if !strings.Contains(competitionAutomationMigration, "epoch_round.staging OR") || !strings.Contains(competitionAutomationGuardMigration, "interval '16 hours'") || !strings.Contains(competitionAutomationGuardMigration, "interval '7 days'") {
		t.Fatal("historical production migration definitions were changed")
	}
}
