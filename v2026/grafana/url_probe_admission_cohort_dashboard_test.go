package grafana

import (
	"slices"
	"strings"
	"testing"
)

func TestUrlProbeAdmissionDashboardPreservesQualifiedSnapshotAndUnknownAge(t *testing.T) {
	dashboard := readTestDashboard(t, "egress-probes.json")
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("URL cohort diagnostics must remain authenticated")
	}
	for _, id := range []int{74, 75, 76, 77, 78, 79} {
		panel := dashboardPanelById(dashboard, id)
		if panel == nil || len(panel.Targets) != 1 {
			t.Fatalf("missing URL cohort panel %d", id)
		}
		query := panel.Targets[0].Expr
		for _, guard := range []string{
			`env="$env",job="taskworker",instance!=""`,
			"urnetwork_url_probe_fleet_observed_timestamp_seconds",
			"urnetwork_url_probe_shard_observed_timestamp_seconds", `shard="0"`,
			"process_start_time_seconds", "count by (env,job,host,block)",
			"and on() (count(", "time() - 180", "time() + 30",
			"timestamp(", "group_left timestamp(",
		} {
			if !strings.Contains(query, guard) {
				t.Errorf("panel %d loses current atomic census guard %q", id, guard)
			}
		}
		for _, forbidden := range []string{"topk(", "vector(0)", "clamp_min(", "sum(urnetwork_url_probe"} {
			if strings.Contains(query, forbidden) {
				t.Errorf("panel %d can manufacture or arbitrarily select coverage: %s", id, forbidden)
			}
		}
		if !strings.Contains(panel.Description, "never summed") || !strings.Contains(panel.Description, "no data") {
			t.Errorf("panel %d omits publisher/freshness semantics", id)
		}
		if id == 77 || id == 79 {
			if strings.Contains(query, "urnetwork_url_probe_admission_cohort") {
				t.Errorf("legacy quota/security panel %d depends on new cohort rollout", id)
			}
			continue
		}
		for _, guard := range []string{"urnetwork_url_probe_admission_cohort_contract", "== 9", "== 3", "sum by (env,job,host,block,instance)", "floor("} {
			if !strings.Contains(query, guard) {
				t.Errorf("panel %d lost complete cohort partition guard %q", id, guard)
			}
		}
	}
	mature := dashboardPanelById(dashboard, 74)
	unknown := dashboardPanelById(dashboard, 76)
	if mature.FieldConfig.Defaults.Unit != "percentunit" || !strings.Contains(mature.Title, "Known mature") ||
		!strings.Contains(mature.Description, "N/A means zero mature denominator") ||
		!strings.Contains(mature.Description, "100% does not prove whole-fleet") ||
		!strings.Contains(mature.Targets[0].Expr, `cohort="mature"} > 0`) {
		t.Fatal("known mature quota lost its denominator or subset qualification")
	}
	if unknown.GridPos.Y != mature.GridPos.Y || !strings.Contains(unknown.Targets[0].Expr, `cohort="age_unknown"`) ||
		!strings.Contains(unknown.Description, "Any nonzero value withholds a whole-fleet") {
		t.Fatal("unknown first-admission age is not visible beside the known mature percentage")
	}
	counts := dashboardPanelById(dashboard, 78)
	if counts.Type != "timeseries" || counts.Targets[0].LegendFormat != "{{cohort}} {{state}}" ||
		counts.Targets[0].Instant || !strings.Contains(counts.Description, "Mature deficits stay visible") {
		t.Fatal("cohort timeline lost the mature, warming, and unknown deficits")
	}
}
