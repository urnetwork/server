// Keeps automation diagnostics internal and preserves the distinction between
// reward delivery, code approval, and actual settlement.
package grafana

import (
	"encoding/json"
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
		"automation_progression_blocked",
		"executor_slot_busy", "executor_noop_qualified", "executor_deadline_remaining_seconds",
		"executor_heartbeat_age_seconds", "executor_transport_failures_total", "executor_remote_state",
		"executor_reconciliation_required",
		"executor_physical_available", "executor_physical_observed_at_seconds",
		"receive_cleanup_failures_total",
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

// Receive housekeeping failures are counted by their finite phase only, beside
// the physical admission observation, in the first-count form a counter needs
// when its series starts at its first failure (counter_first_count_dashboard_test.go).
func TestCompetitionReceiveCleanupFailuresByPhase(t *testing.T) {
	dashboard := readTestDashboard(t, "competition-automation.json")
	panel := dashboardPanelById(dashboard, competitionReceiveCleanupPanel)
	if panel == nil || panel.Type != "timeseries" || panel.Title != "executor receive custody and cleanup failures" || panel.FieldConfig.Defaults.Unit != "short" || len(panel.Targets) != 1 {
		t.Fatalf("receive cleanup failures panel is missing or not a one-query short time series: %+v", panel)
	}
	const counter = `urnetwork_competition_receive_cleanup_failures_total{env="$env"}`
	want := "sum by (phase) ((increase(" + counter + "[1h]) and " + counter + " offset 1h) or " + counter + ")"
	if target := panel.Targets[0]; target.Expr != want || target.LegendFormat != "{{phase}}" {
		t.Errorf("receive cleanup failures panel reads %s as %q, want %s as {{phase}}", target.Expr, target.LegendFormat, want)
	}
	for _, explained := range []string{"capture", "retention", "cleanup", "close", "stays on disk", "Committed outcomes and scores are unchanged", "first failure"} {
		if !strings.Contains(panel.Description, explained) {
			t.Errorf("receive cleanup failures panel does not explain %q", explained)
		}
	}
	admission := dashboardPanelById(dashboard, 15)
	if admission == nil || panel.GridPos.Y != admission.GridPos.Y || panel.GridPos.X != admission.GridPos.X+admission.GridPos.W {
		t.Error("receive cleanup failures panel does not sit beside the physical admission observation")
	}
}

// The exported minus-one sentinel is not healthy zero. Threshold colors and
// the exact value mapping must both distinguish missing heartbeat telemetry.
func TestCompetitionExecutorHeartbeatDashboardMarksMissingObservationUnhealthy(t *testing.T) {
	dashboard := readTestDashboard(t, "competition-automation.json")
	panel := dashboardPanelById(dashboard, 12)
	if panel == nil || len(panel.Targets) != 1 || !strings.Contains(panel.Targets[0].Expr, "executor_heartbeat_age_seconds") {
		t.Fatal("executor heartbeat panel is missing")
	}
	steps := panel.FieldConfig.Defaults.Thresholds.Steps
	if len(steps) < 2 || steps[0].Value != nil || steps[0].Color != "red" || steps[1].Value == nil || *steps[1].Value != 0 || steps[1].Color != "green" {
		t.Fatal("missing heartbeat falls into the healthy threshold color")
	}
	content, err := dashboardsFs.ReadFile("dashboards/competition-automation.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Panels []struct {
			Id          int `json:"id"`
			FieldConfig struct {
				Defaults struct {
					Color struct {
						Mode string `json:"mode"`
					} `json:"color"`
					Mappings []struct {
						Type    string `json:"type"`
						Options map[string]struct {
							Color string `json:"color"`
							Text  string `json:"text"`
						} `json:"options"`
					} `json:"mappings"`
				} `json:"defaults"`
			} `json:"fieldConfig"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(content, &raw); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range raw.Panels {
		if candidate.Id != 12 {
			continue
		}
		if candidate.FieldConfig.Defaults.Color.Mode != "thresholds" {
			t.Fatal("executor heartbeat series do not use their health threshold colors")
		}
		for _, mapping := range candidate.FieldConfig.Defaults.Mappings {
			if value, present := mapping.Options["-1"]; present && mapping.Type == "value" && value.Color == "red" && value.Text == "missing heartbeat" {
				return
			}
		}
		t.Fatal("executor heartbeat has no explicit unhealthy missing-observation mapping")
	}
	t.Fatal("executor heartbeat panel could not be inspected")
}
