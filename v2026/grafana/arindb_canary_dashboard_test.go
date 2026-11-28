package grafana

import (
	"slices"
	"strings"
	"testing"
)

// The ARIN database canary row of the providers dashboard (arindbctl/ARINDB.md,
// "Signals, alerts and dashboards"): the state and fallback, bucket supply
// under each database, losses, coverage, emptied countries, the indexer's
// pair and each process's slots, hashes and ages.
var arindbCanaryStatsMetrics = []string{
	"urnetwork_stats_arindb_canary_state",
	"urnetwork_stats_arindb_fallback_active",
	"urnetwork_stats_arindb_providers",
	"urnetwork_stats_arindb_providers_by_country",
	"urnetwork_stats_arindb_loss_ratio",
	"urnetwork_stats_arindb_emptied_countries",
	"urnetwork_stats_arindb_emptied_country",
	"urnetwork_stats_arindb_emptied_cities",
	"urnetwork_stats_arindb_coverage_ratio",
	"urnetwork_stats_arindb_database_build_epoch_seconds",
	"urnetwork_stats_arindb_database_info",
	"urnetwork_stats_arindb_serving_epoch_seconds",
	"urnetwork_stats_arindb_decision_seconds",
	"urnetwork_stats_arindb_decision_flags",
	"urnetwork_stats_arindb_gain_ratio",
	"urnetwork_stats_arindb_churn_ratio",
	"urnetwork_stats_arindb_cohort_providers",
	"urnetwork_stats_arindb_cohort_loss_ratio",
	"urnetwork_stats_arindb_serving_changes",
	"urnetwork_stats_arindb_latch_passing_runs",
	"urnetwork_stats_arindb_pair_since_seconds",
}

// each process's own ARIN slots, pushed with its own labels
var arindbCanaryProcessMetrics = []string{
	"urnetwork_arin_database_build_epoch_seconds",
	"urnetwork_arin_old_database_build_epoch_seconds",
	"urnetwork_arin_database_info",
}

func TestProvidersDashboardShowsTheArindbCanaryRow(t *testing.T) {
	dashboard := readTestDashboard(t, "providers.json")
	rowY := -1
	for _, panel := range dashboard.Panels {
		if panel.Type == "row" && panel.Title == "ARIN database canary" {
			rowY = panel.GridPos.Y
		}
	}
	if rowY < 0 {
		t.Fatal("the providers dashboard has no ARIN database canary row")
	}
	var inRow []string
	for _, panel := range dashboard.Panels {
		if panel.Type == "row" || panel.GridPos.Y <= rowY {
			continue
		}
		for _, target := range panel.Targets {
			inRow = append(inRow, target.Expr)
		}
	}
	joined := strings.Join(inRow, "\n")
	for question, read := range map[string]string{
		"which state":                   `max by (state) (urnetwork_stats_arindb_canary_state{env="$env"}) == 1`,
		"is the fallback serving":       `max(urnetwork_stats_arindb_fallback_active{env="$env"})`,
		"which database is served":      `max(urnetwork_stats_arindb_serving_epoch_seconds{env="$env"})`,
		"how old is the decision":       `time() - max(urnetwork_stats_arindb_decision_seconds{env="$env"})`,
		"supply under each database":    `max by (bucket, database) (urnetwork_stats_arindb_providers{env="$env"})`,
		"net loss":                      `max by (bucket) (urnetwork_stats_arindb_loss_ratio{env="$env"})`,
		"coverage":                      `max by (database) (urnetwork_stats_arindb_coverage_ratio{env="$env"})`,
		"how many countries emptied":    `max by (bucket) (urnetwork_stats_arindb_emptied_countries{env="$env"})`,
		"which countries emptied":       `max by (bucket, country_code) (urnetwork_stats_arindb_emptied_country{env="$env"})`,
		"how many cities emptied":       `max by (bucket) (urnetwork_stats_arindb_emptied_cities{env="$env"})`,
		"supply per country":            `max by (bucket, database, country_code) (urnetwork_stats_arindb_providers_by_country{env="$env"})`,
		"the indexer's epochs":          `max by (database) (urnetwork_stats_arindb_database_build_epoch_seconds{env="$env"})`,
		"the indexer's hashes":          `max(urnetwork_stats_arindb_database_info{env="$env"}) by (database, state, sha256)`,
		"each process's slots":          `max(urnetwork_arin_database_info{env="$env"}) by (service, host, block, instance, slot, state, sha256)`,
		"each process's latest age":     `time() - max by (service, host, block, instance) (urnetwork_arin_database_build_epoch_seconds{env="$env"} > 0)`,
		"each process's old age":        `time() - max by (service, host, block, instance) (urnetwork_arin_old_database_build_epoch_seconds{env="$env"} > 0)`,
		"the loss threshold is visible": `urnetwork_stats_arindb_loss_ratio`,
		"which conditions hold":         `max by (flag) (urnetwork_stats_arindb_decision_flags{env="$env"})`,
		"net gain":                      `max by (bucket) (urnetwork_stats_arindb_gain_ratio{env="$env"})`,
		"gross churn":                   `max by (bucket) (urnetwork_stats_arindb_churn_ratio{env="$env"})`,
		"the cohort and its reference":  `max by (reference) (urnetwork_stats_arindb_cohort_providers{env="$env"})`,
		"the cohort's loss":             `max(urnetwork_stats_arindb_cohort_loss_ratio{env="$env"})`,
		"how often the fleet switched":  `max(urnetwork_stats_arindb_serving_changes{env="$env"})`,
		"how near a latch's release":    `max(urnetwork_stats_arindb_latch_passing_runs{env="$env"})`,
		"how old the compared pair is":  `time() - max(urnetwork_stats_arindb_pair_since_seconds{env="$env"})`,
		"how often readers miss it":     `sum(rate(urnetwork_arindb_decision_reads_total{env="$env",outcome!="ok"}[$__rate_interval])) / sum(rate(urnetwork_arindb_decision_reads_total{env="$env"}[$__rate_interval]))`,
	} {
		if !strings.Contains(joined, read) {
			t.Errorf("the canary row does not answer %q (expected a read of %s)", question, read)
		}
	}
	for _, metric := range append(slices.Clone(arindbCanaryStatsMetrics), arindbCanaryProcessMetrics...) {
		if len(metricOccurrences(joined, metric)) == 0 {
			t.Errorf("the canary row does not chart %s", metric)
		}
	}

	// the breaker's default thresholds are drawn where they apply
	for title, value := range map[string]float64{
		"net loss of latest against old": 0.10, "coverage of the public cohort": 0.98,
		"net gain of latest against old": 0.10, "gross churn of latest against old": 0.20,
		"cohort loss against its recent size": 0.10, "decision reads that were not ok": 0.05,
	} {
		found := false
		for _, panel := range dashboard.Panels {
			if panel.Title != title {
				continue
			}
			found = true
			steps := panel.FieldConfig.Defaults.Thresholds.Steps
			if panel.FieldConfig.Defaults.Custom.ThresholdsStyle.Mode != "line" || len(steps) != 2 || steps[1].Value == nil || *steps[1].Value != value {
				t.Errorf("%s does not draw its %v threshold", title, value)
			}
		}
		if !found {
			t.Errorf("the canary row has no %q panel", title)
		}
	}

	// the canary is internal: which databases classify providers, and what a
	// release would remove, is operator business
	for _, metric := range append(slices.Clone(arindbCanaryStatsMetrics), arindbCanaryProcessMetrics...) {
		if slices.Contains(publicSafeMetrics, metric) {
			t.Errorf("%s is internal and must not be public-safe", metric)
		}
		for _, entry := range mustReadDashboardDir(t) {
			other := readTestDashboard(t, entry)
			if slices.Contains(other.Tags, PublicTag) && len(metricOccurrences(strings.Join(dashboardExpressions(other), "\n"), metric)) != 0 {
				t.Errorf("public dashboard %s reads the internal %s", entry, metric)
			}
		}
	}
}

// Each process's decision reads and lookup errors are counters, read as rates
// summed across the processes of a service, beside each process's database
// ages.
func TestServicesOverviewShowsTheArindbDecisionReadsAndOldAges(t *testing.T) {
	expressions := dashboardExpressions(readTestDashboard(t, "services-overview.json"))
	joined := strings.Join(expressions, "\n")
	for _, read := range []string{
		`sum by (service, outcome) (rate(urnetwork_arindb_decision_reads_total{env="$env",service=~"$service",block=~"$block",host=~"$host"}[$__rate_interval]))`,
		`sum by (service, slot) (rate(urnetwork_arin_lookup_errors_total{env="$env",service=~"$service",block=~"$block",host=~"$host"}[$__rate_interval]))`,
		`sum by (service, slot) (rate(urnetwork_arin_lookup_errors_total{env="$env",service=~"$service",block=~"$block",host=~"$host"}[$__rate_interval])) / sum by (service, slot) (rate(urnetwork_arin_lookups_total{env="$env",service=~"$service",block=~"$block",host=~"$host"}[$__rate_interval]))`,
		`time() - max by (service, host, block, instance) (urnetwork_arin_old_database_build_epoch_seconds{env="$env",service=~"$service",block=~"$block",host=~"$host"} > 0)`,
		`time() - max by (service, host, block, instance) (urnetwork_arin_database_build_epoch_seconds{env="$env",service=~"$service",block=~"$block",host=~"$host"} > 0)`,
	} {
		if !strings.Contains(joined, read) {
			t.Errorf("services overview lacks %s", read)
		}
	}
}
