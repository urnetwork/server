package grafana

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The providers dashboard's egress row (connect/GEOMAP.md §10.3, §10.4): the
// bucket gauges, the exclusion reasons, and the backfill read beside them.

// the egress bucket gauges of connect/GEOMAP.md §10.4, replicated by every
// taskworker like the other stats gauges
const (
	providersEgressIndexMetric = "urnetwork_stats_provider_egress_index"
	providersExcludedMetric    = "urnetwork_stats_provider_excluded"
)

// the backfill histogram (GEOMAP §10.3 "Backfill"), the answered-providers
// counter it is read against, and the answer outcome counter, each observed
// by each api process for the answers it gives
const (
	providersBackfillMetric = "urnetwork_provider_backfill"
	providersAnsweredMetric = "urnetwork_provider_answered_total"
	providersOutcomesMetric = "urnetwork_findproviders2_outcomes_total"
)

// Whether a series is one of the per-process answer metrics, which are summed
// as rates rather than read with max.
func providersPerProcessMetric(metric string) bool {
	return strings.HasPrefix(metric, providersBackfillMetric+"_") ||
		metric == providersAnsweredMetric ||
		metric == providersOutcomesMetric
}

// The egress row of the providers dashboard: every bucket and every exclusion
// reason has its panel, the backfill sits beside the reasons so a mass probe
// failure reads as a wave of backfill, and every read aggregates across the
// fleet, is scoped to the env and is documented.
func TestProvidersDashboardShowsTheEgressBuckets(t *testing.T) {
	dashboard := readTestDashboard(t, "providers.json")
	expressions := dashboardExpressions(dashboard)
	joined := strings.Join(expressions, "\n")

	// each question the row answers, and the read that answers it
	for question, read := range map[string]string{
		"quality bucket":          `max by (index) (urnetwork_stats_provider_egress_index{env="$env",bucket="quality"})`,
		"speed bucket":            `max by (index) (urnetwork_stats_provider_egress_index{env="$env",bucket="speed"})`,
		"online bucket":           `max by (index) (urnetwork_stats_provider_egress_index{env="$env",bucket="online"})`,
		"buckets over time":       `max by (bucket, index) (urnetwork_stats_provider_egress_index{env="$env"})`,
		"clean quality supply":    `max(urnetwork_stats_provider_egress_index{env="$env",bucket="quality",index="0"})`,
		"left out, by reason":     `max by (reason) (urnetwork_stats_provider_excluded{env="$env"})`,
		"borrowed, by rank mode":  `sum by (rank_mode) (rate(urnetwork_provider_backfill_sum{env="$env"}[5m]))`,
		"borrowed per answer":     `sum by (rank_mode) (rate(urnetwork_provider_backfill_count{env="$env"}[5m]))`,
		"answers backfilled":      `sum by (rank_mode) (rate(urnetwork_provider_backfill_bucket{env="$env",le="0"}[5m]))`,
		"short and empty answers": `sum by (rank_mode, result_count) (rate(urnetwork_findproviders2_outcomes_total{env="$env",force_minimum="false"}[5m]))`,
		"borrowed share":          `sum by (rank_mode) (rate(urnetwork_provider_backfill_sum{env="$env"}[5m])) / sum by (rank_mode) (rate(urnetwork_provider_answered_total{env="$env"}[5m]))`,
	} {
		if !strings.Contains(joined, read) {
			t.Errorf("no panel answers %q (expected a read of %s)", question, read)
		}
	}

	// every reason the rules report has a panel of its own
	for _, reason := range []string{"blackhole", "tls", "country", "health", "unprobed"} {
		read := `max(urnetwork_stats_provider_excluded{env="$env",reason="` + reason + `"})`
		if !strings.Contains(joined, read) {
			t.Errorf("the %s exclusion has no panel of its own (expected %s)", reason, read)
		}
	}

	// the backfill sits in the reasons' row, beside them
	reasonsRow := -1
	backfillRow := -1
	for _, panel := range dashboard.Panels {
		for _, target := range panel.Targets {
			if strings.Contains(target.Expr, providersExcludedMetric) && panel.Type == "timeseries" {
				reasonsRow = panel.GridPos.Y
			}
			if strings.Contains(target.Expr, providersBackfillMetric+"_sum") && panel.Type == "timeseries" && backfillRow < 0 {
				backfillRow = panel.GridPos.Y
			}
		}
	}
	if reasonsRow < 0 || reasonsRow != backfillRow {
		t.Errorf("the backfill (row y=%d) is not beside the exclusion reasons (row y=%d)", backfillRow, reasonsRow)
	}

	// the backfill histogram is per process: every read of it is a rate
	// summed across the processes, over a window wider than the staggered
	// scrape interval, and scoped to the env
	backfillRead := regexp.MustCompile(`sum by \((rank_mode|rank_mode, result_count)\) \(rate\((urnetwork_provider_backfill_(sum|count|bucket)|urnetwork_provider_answered_total|urnetwork_findproviders2_outcomes_total)\{env="\$env"[^}]*\}\[5m\]\)\)`)
	for _, expression := range expressions {
		series := 0
		for _, metric := range metricNamePattern.FindAllString(expression, -1) {
			if providersPerProcessMetric(metric) {
				series += 1
			}
		}
		if reads := len(backfillRead.FindAllString(expression, -1)); reads != series {
			t.Errorf("a per-process answer read is not a summed rate scoped to the env: %s", expression)
		}
	}

	// the egress gauges are read replica-safely, like every stats gauge here
	for _, expression := range expressions {
		for _, metric := range []string{providersEgressIndexMetric, providersExcludedMetric} {
			assertReplicaSafeReads(t, "providers egress row", expression, metric, `{env="$env"`)
		}
	}

	// every panel is documented: the row is read during an incident by someone
	// who did not write it
	for _, panel := range dashboard.Panels {
		if panel.Type != "row" && panel.Description == "" {
			t.Errorf("panel %q has no description", panel.Title)
		}
	}

	// the egress metrics are internal: which providers the rules leave out,
	// and how often answers borrow, is operator business
	for _, metric := range []string{providersEgressIndexMetric, providersExcludedMetric, providersBackfillMetric, providersAnsweredMetric} {
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
