package grafana

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The providers dashboard's capability-aware selection row (FP2-PROTOCOL.md
// §8.1): the classified decision's supply by capability label and by
// post-quantum signal, and the selector-mode and strict post-quantum request
// counters.

// the classified decision's gauges, replicated by every taskworker like the
// other stats gauges
const (
	providersEgressCapabilityMetric = "urnetwork_stats_provider_egress_capability"
	providersPostQuantumMetric      = "urnetwork_stats_provider_post_quantum"
)

// the per-process FindProviders2 counters of selector mode and strict
// post-quantum requests, each observed by each api, alt and proxy process
var providersCapabilityRequestMetrics = []string{
	"urnetwork_findproviders2_select_requests_total",
	"urnetwork_findproviders2_select_answered_total",
	"urnetwork_findproviders2_select_overflow_total",
	"urnetwork_findproviders2_select_ipv6_need_total",
	"urnetwork_findproviders2_post_quantum_requests_total",
}

// Every question the row answers has its read: each bucket's supply by
// label, the post-quantum supply, and every selector-mode and post-quantum
// counter by its labels. The gauges are read replica-safely with max, the
// counters as summed rates over a window wider than the staggered scrape
// interval, every read scoped to the env, every panel documented and in the
// row, and nothing of it is public.
func TestProvidersDashboardShowsTheCapabilityAwareSelection(t *testing.T) {
	dashboard := readTestDashboard(t, "providers.json")
	expressions := dashboardExpressions(dashboard)
	joined := strings.Join(expressions, "\n")

	for question, read := range map[string]string{
		"quality supply by capability":     `max by (ip, protocol) (urnetwork_stats_provider_egress_capability{env="$env",bucket="quality"})`,
		"speed supply by capability":       `max by (ip, protocol) (urnetwork_stats_provider_egress_capability{env="$env",bucket="speed"})`,
		"online supply by capability":      `max by (ip, protocol) (urnetwork_stats_provider_egress_capability{env="$env",bucket="online"})`,
		"post-quantum supply":              `max by (bucket, signal) (urnetwork_stats_provider_post_quantum{env="$env"})`,
		"strict post-quantum requests":     `sum by (field) (rate(urnetwork_findproviders2_post_quantum_requests_total{env="$env"}[5m]))`,
		"selector-mode requests":           `sum by (rank_mode, ip, protocol) (rate(urnetwork_findproviders2_select_requests_total{env="$env"}[5m]))`,
		"selector-mode providers answered": `sum by (rank_mode, overflow_step, ip, protocol) (rate(urnetwork_findproviders2_select_answered_total{env="$env"}[5m]))`,
		"requests given overflow":          `sum by (rank_mode) (rate(urnetwork_findproviders2_select_overflow_total{env="$env"}[5m]))`,
		"ipv6 need met or short":           `sum by (rank_mode, outcome) (rate(urnetwork_findproviders2_select_ipv6_need_total{env="$env"}[5m]))`,
	} {
		if !strings.Contains(joined, read) {
			t.Errorf("no panel answers %q (expected a read of %s)", question, read)
		}
	}

	for _, expression := range expressions {
		for _, metric := range []string{providersEgressCapabilityMetric, providersPostQuantumMetric} {
			assertReplicaSafeReads(t, "providers capability row", expression, metric, `{env="$env"`)
		}
	}
	counterRead := regexp.MustCompile(`sum by \([a-z_, ]+\) \(rate\((urnetwork_findproviders2_(select_requests|select_answered|select_overflow|select_ipv6_need|post_quantum_requests)_total)\{env="\$env"[^}]*\}\[5m\]\)\)`)
	for _, expression := range expressions {
		series := 0
		for _, metric := range metricNamePattern.FindAllString(expression, -1) {
			if slices.Contains(providersCapabilityRequestMetrics, metric) {
				series += 1
			}
		}
		if reads := len(counterRead.FindAllString(expression, -1)); reads != series {
			t.Errorf("a per-process selection counter is not a summed rate scoped to the env: %s", expression)
		}
	}

	// every panel reading the row's metrics is documented, cites the design
	// and sits below the row header
	rowY := -1
	for _, panel := range dashboard.Panels {
		if panel.Type == "row" && strings.Contains(panel.Title, "FP2-PROTOCOL.md") {
			rowY = panel.GridPos.Y
		}
	}
	if rowY < 0 {
		t.Fatal("no capability-aware selection row")
	}
	rowMetrics := append([]string{providersEgressCapabilityMetric, providersPostQuantumMetric}, providersCapabilityRequestMetrics...)
	panels := 0
	for _, panel := range dashboard.Panels {
		reads := false
		for _, target := range panel.Targets {
			for _, metric := range rowMetrics {
				if strings.Contains(target.Expr, metric) {
					reads = true
				}
			}
		}
		if !reads {
			continue
		}
		panels += 1
		if panel.Description == "" || !strings.Contains(panel.Description, "FP2-PROTOCOL.md") {
			t.Errorf("panel %q does not explain itself with the design", panel.Title)
		}
		if panel.GridPos.Y <= rowY {
			t.Errorf("panel %q sits above the capability-aware selection row", panel.Title)
		}
	}
	if panels != 9 {
		t.Errorf("%d panels read the row's metrics, want 9", panels)
	}

	// internal: which providers lost a capability, and how answers overflow,
	// is operator business
	for _, metric := range rowMetrics {
		if slices.Contains(publicSafeMetrics, metric) {
			t.Errorf("%s is internal and must not be public-safe", metric)
		}
		for _, entry := range mustReadDashboardDir(t) {
			other := readTestDashboard(t, entry)
			if !slices.Contains(other.Tags, PublicTag) {
				continue
			}
			if strings.Contains(strings.Join(dashboardExpressions(other), "\n"), metric) {
				t.Errorf("public dashboard %s reads the internal %s", entry, metric)
			}
		}
	}
}
