package grafana

import (
	"regexp"
	"strings"
	"testing"
)

// The lag.dev dashboard (lag.dev/PLAN.md X9) reads two kinds of series, and
// each has its own way to go wrong without any test noticing:
//
//   - the lag stats gauges are database counts that every taskworker
//     publishes, so a sum across hosts multiplies them by the taskworker
//     count; they must be read with max;
//   - the lag counters are per process, so a raw value is a process-lifetime
//     total that a restart resets; they must be read through increase() and
//     summed across processes.
//
// And lag.dev is open to anyone, so no query may group by a label that names
// a visitor or an extender.
func TestLagDevDashboardAnswersEachQuestion(t *testing.T) {
	dashboard := readTestDashboard(t, "lag-dev.json")

	if dashboard.Uid != "urnetwork-lag-dev" {
		t.Errorf("uid = %q", dashboard.Uid)
	}
	if len(dashboard.Templating.List) == 0 {
		t.Error("the dashboard has no env template variable")
	}
	expressions := dashboardExpressions(dashboard)
	if len(expressions) == 0 {
		t.Fatal("the dashboard has no queries")
	}

	// each question X9 asks, and the metric that answers it
	for question, metric := range map[string]string{
		"measurements per hour":   "urnetwork_stats_lag_results_1h",
		"tracks per hour":         "urnetwork_stats_lag_tracks_1h",
		"guest sessions per hour": "urnetwork_stats_lag_guests_1h",
		"session outcomes":        "urnetwork_lag_sessions_total",
		"track outcomes":          "urnetwork_lag_tracks_total",
		"report outcomes":         "urnetwork_lag_reports_total",
		"report post outcomes":    "urnetwork_lag_report_posts_total",
		"idle latency":            "urnetwork_stats_lag_idle_latency_p50_ms",
		"download speed":          "urnetwork_stats_lag_download_p50_mbps",
		"upload speed":            "urnetwork_stats_lag_upload_p50_mbps",
		"extenders offered":       "urnetwork_stats_lag_extenders_offered",
		"certificate requests":    "urnetwork_stats_lag_measurement_requests",
		"opted-out extenders":     "urnetwork_stats_lag_extenders_opted_out",
		"visitors by country":     "urnetwork_stats_lag_visitors_24h_by_country",
		"rows the sweep removed":  "urnetwork_lag_sweep_deleted_total",
		// amendment 12: what every other panel leaves out, and which client
		// ran what counts
		"automated measurements": "urnetwork_stats_lag_automated_results_1h",
		"measurements by source": "urnetwork_stats_lag_results_1h_by_source",
	} {
		found := false
		for _, expression := range expressions {
			if len(metricOccurrences(expression, metric)) != 0 {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no panel answers %q (expected %s)", question, metric)
		}
	}

	// every query is scoped to the env variable, or it mixes environments
	for _, expression := range expressions {
		if !strings.Contains(expression, `env="$env"`) {
			t.Errorf("query is not scoped to $env: %q", expression)
		}
	}
}

// Every lag stats gauge is read with max across hosts, and every lag counter
// through increase() inside a sum.
func TestLagDevDashboardReadsAreInstanceSafe(t *testing.T) {
	dashboard := readTestDashboard(t, "lag-dev.json")

	gauge := regexp.MustCompile(`urnetwork_stats_lag_[a-z0-9_]+`)
	counter := regexp.MustCompile(`urnetwork_lag_[a-z0-9_]+_total`)
	for _, panel := range dashboard.Panels {
		for _, target := range panel.Targets {
			expression := target.Expr
			for _, metric := range gauge.FindAllString(expression, -1) {
				assertReplicaSafeReads(t, panel.Title, expression, metric, `{env="$env"`)
			}
			for _, index := range counter.FindAllStringIndex(expression, -1) {
				metric := expression[index[0]:index[1]]
				if !strings.HasSuffix(expression[:index[0]], "increase(") {
					t.Errorf("panel %q reads counter %s without increase(): %s", panel.Title, metric, expression)
				}
				if !strings.HasPrefix(expression, "sum(") && !strings.HasPrefix(expression, "sum by (") {
					t.Errorf("panel %q does not sum counter %s across processes: %s", panel.Title, metric, expression)
				}
			}
		}
	}
}

// No query groups by a label outside the bounded enums the lag metrics carry:
// end reason, family, outcome, state, a country among the top 10 or other,
// the sweep's kind, why a track is automated, and which client ran it.
// Nothing names a visitor, an address, an extender or an agent (amendments 9
// and 12).
func TestLagDevDashboardGroupsOnlyByBoundedLabels(t *testing.T) {
	dashboard := readTestDashboard(t, "lag-dev.json")

	bounded := map[string]bool{
		"end_reason":   true,
		"ip_family":    true,
		"outcome":      true,
		"state":        true,
		"country_code": true,
		"kind":         true,
		"reason":       true,
		"source":       true,
	}
	byClause := regexp.MustCompile(`by \(([^)]*)\)`)
	for _, expression := range dashboardExpressions(dashboard) {
		for _, match := range byClause.FindAllStringSubmatch(expression, -1) {
			for _, label := range strings.Split(match[1], ",") {
				label = strings.TrimSpace(label)
				if !bounded[label] {
					t.Errorf("query groups by %q, which is not a bounded lag label: %s", label, expression)
				}
			}
		}
		for _, unbounded := range []string{"extender_id", "client_id", "network_id", "track_id", "signature_agent=", "instance)"} {
			if strings.Contains(expression, unbounded) {
				t.Errorf("query names %q: %s", unbounded, expression)
			}
		}
	}
}

// The headline is a time series of measurements per hour, stacked by how
// they ended, and every panel says what it reads and what normal looks like.
func TestLagDevDashboardHeadlineAndDescriptions(t *testing.T) {
	dashboard := readTestDashboard(t, "lag-dev.json")

	headline := (*testPanel)(nil)
	for panelIndex := range dashboard.Panels {
		if dashboard.Panels[panelIndex].Title == "measurements per hour" {
			headline = &dashboard.Panels[panelIndex]
		}
	}
	if headline == nil {
		t.Fatal("there is no measurements per hour panel")
	}
	if headline.Type != "timeseries" {
		t.Errorf("measurements per hour is a %s, want a timeseries", headline.Type)
	}
	if len(headline.Targets) != 1 || !strings.HasPrefix(headline.Targets[0].Expr, "sum by (end_reason) (") {
		t.Errorf("measurements per hour is not stacked by end reason: %+v", headline.Targets)
	}
	if headline.GridPos.Y != 1 {
		t.Errorf("measurements per hour is not the first panel under the first row: %+v", headline.GridPos)
	}

	for _, panel := range dashboard.Panels {
		if panel.Type == "row" {
			continue
		}
		if strings.TrimSpace(panel.Description) == "" {
			t.Errorf("panel %q has no description", panel.Title)
		}
	}
}
