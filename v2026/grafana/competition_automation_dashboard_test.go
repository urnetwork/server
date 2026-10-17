// Keeps automation diagnostics internal and preserves the distinction between
// reward delivery, code approval, and actual settlement.
package grafana

import (
	"slices"
	"strings"
	"testing"
)

// Aggregate control-plane telemetry must not turn into an early leaderboard.
func TestCompetitionAutomationDashboardHasNoCandidateDisclosure(t *testing.T) {
	dashboard := readTestDashboard(t, "competition-automation.json")
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("automation diagnostics must remain authenticated")
	}
	queries := strings.Join(dashboardExpressions(dashboard), "\n")
	for _, metric := range []string{
		"automation_unpublished_seals", "automation_pending_reward_events",
		"automation_oldest_reward_event_age_seconds", "automation_pending_source_activations",
		"automation_supervisor_heartbeat_age_seconds",
		"automation_supervisor_expected",
		"automation_hold_active", "automation_integrity_incidents",
	} {
		if !strings.Contains(queries, "urnetwork_competition_"+metric) {
			t.Errorf("missing automation metric %s", metric)
		}
	}
	for _, forbidden := range []string{"job_id", "patch_sha256", "raw_score", "normalized_score", "quantile"} {
		if strings.Contains(queries, forbidden) {
			t.Errorf("automation queries expose candidate field %s", forbidden)
		}
	}
	if !strings.Contains(dashboard.Description, "not a chain settlement") {
		t.Fatal("delivery receipt must not imply payment finality")
	}
}
