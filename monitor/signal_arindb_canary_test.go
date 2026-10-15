package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

var arindbCanaryFixtureNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// One exported gauge sample as the signal's query returns it.
type arindbCanaryFixtureSeries struct {
	metric string
	labels map[string]string
	value  float64
}

// The gauges of a decision in state, decided age ago, with losses per bucket
// and the old supply of emptied countries ("bucket/cc").
func arindbCanaryFixture(state string, age time.Duration, losses map[string]float64, emptied map[string]float64) []arindbCanaryFixtureSeries {
	series := []arindbCanaryFixtureSeries{}
	for _, s := range []string{"single", "warming", "latest", "fallback"} {
		value := 0.0
		if s == state {
			value = 1
		}
		series = append(series, arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_canary_state", labels: map[string]string{"state": s}, value: value})
	}
	fallback := 0.0
	if state == "fallback" {
		fallback = 1
	}
	for _, bucket := range []string{"quality", "speed", "online"} {
		series = append(series, arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_providers", labels: map[string]string{"bucket": bucket, "database": "old"}, value: 100})
	}
	for _, flag := range []string{"warming_tripped", "warming_serves_latest", "latched", "accepted", "latest_unavailable", "pair_carried", "fallback_dropped"} {
		series = append(series, arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_decision_flags", labels: map[string]string{"flag": flag}, value: 0})
	}
	series = append(series,
		arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_fallback_active", value: fallback},
		arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_cohort_providers", labels: map[string]string{"reference": "current"}, value: 1000},
		arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_cohort_providers", labels: map[string]string{"reference": "reference"}, value: 1000},
		arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_cohort_loss_ratio", value: 0},
		arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_serving_changes", value: 1},
		arindbCanaryFixtureSeries{metric: arindbCanaryReadsNotOkMetric, value: 0},
		// a pair compared since yesterday, and no failing latest lookups
		arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_pair_since_seconds", value: float64(arindbCanaryFixtureNow.Add(-24 * time.Hour).Unix())},
		arindbCanaryFixtureSeries{metric: arindbCanaryLatestLookupErrorsMetric, value: 0},
		arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_serving_epoch_seconds", value: 1700000000},
		arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_decision_seconds", value: float64(arindbCanaryFixtureNow.Add(-age).Unix())},
		arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_coverage_ratio", labels: map[string]string{"database": "paired"}, value: 0.99},
	)
	for bucket, loss := range losses {
		series = append(series, arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_loss_ratio", labels: map[string]string{"bucket": bucket}, value: loss})
	}
	for key, old := range emptied {
		bucket, country, _ := strings.Cut(key, "/")
		series = append(series, arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_emptied_country", labels: map[string]string{"bucket": bucket, "country_code": country}, value: old})
	}
	return series
}

func runArindbCanaryFixture(t *testing.T, series []arindbCanaryFixtureSeries) Alerts {
	t.Helper()
	result := []map[string]any{}
	for _, s := range series {
		metric := map[string]string{"monitor_metric": s.metric, "env": "synthetic"}
		for key, value := range s.labels {
			metric[key] = value
		}
		result = append(result, map[string]any{
			"metric": metric,
			"value":  []any{float64(arindbCanaryFixtureNow.Unix()), fmt.Sprintf("%v", s.value)},
		})
	}
	payload, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	if err != nil {
		t.Fatal(err)
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		for _, want := range []string{"urnetwork_stats_arindb_canary_state", "urnetwork_stats_arindb_fallback_active", "urnetwork_stats_arindb_loss_ratio",
			"urnetwork_stats_arindb_emptied_country", "urnetwork_stats_arindb_decision_seconds", "urnetwork_stats_arindb_decision_flags",
			"urnetwork_stats_arindb_gain_ratio", "urnetwork_stats_arindb_churn_ratio", "urnetwork_stats_arindb_cohort_loss_ratio",
			"urnetwork_stats_arindb_serving_changes", "urnetwork_arindb_decision_reads_total", "urnetwork_stats_arindb_pair_since_seconds",
			"urnetwork_arin_lookup_errors_total", "urnetwork_arin_lookups_total", "%22synthetic%22", "monitor_metric"} {
			if host.Name != "metrics-1" || !strings.Contains(command, want) {
				return "", fmt.Errorf("unexpected Mimir command on %s: %s", host.Name, command)
			}
		}
		return string(payload), nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return arindbCanaryFixtureNow }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := NewArindbCanarySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func requireNoArindbCanaryAlerts(t *testing.T, alerts Alerts) {
	t.Helper()
	for _, alert := range alerts {
		t.Errorf("unexpected %s alert: %s", alert.Class, alert.Markdown())
	}
}

// A passing comparison with a fresh decision raises nothing, a Quality loss
// included: only the breaker's fallback pages for it.
func TestArindbCanarySignalHealthyLatest(t *testing.T) {
	requireNoArindbCanaryAlerts(t, runArindbCanaryFixture(t, arindbCanaryFixture("latest", time.Minute, map[string]float64{"quality": 0.08, "speed": 0.02, "online": 0}, nil)))
}

// A fallback pages and names the countries the latest would empty.
func TestArindbCanarySignalPagesOnFallbackAndNamesEmptiedCountries(t *testing.T) {
	alerts := runArindbCanaryFixture(t, arindbCanaryFixture("fallback", time.Minute,
		map[string]float64{"quality": 0.4, "speed": 0, "online": 0}, map[string]float64{"quality/zz": 17, "quality/yy": 3}))
	alert := requireAlertClass(t, alerts, "arindb-fallback-active")
	if alert.SignalNumber != "2.9d" || alert.SignalKey != "arindb-canary" || alert.Sustain != 10 {
		t.Fatalf("fallback alert identity: %+v", alert)
	}
	if alert.Severity != SeverityPage {
		t.Fatalf("fallback alert severity %s, want page", alert.Severity)
	}
	for _, want := range []string{"quality:zz(17),yy(3)", "quality=0.4000", "ARIN_PREVIOUS_DATABASE_FILE", "never readmits", "SIGNALS.md §2.9d"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("fallback alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if requireAlertClassCount(alerts, "arindb-risk-supply-loss") != 0 {
		t.Fatal("a Quality-only fallback paged as a risk-driven loss")
	}
}

// A Speed or Online loss above the threshold pages whatever the state, since
// only risk causes it and the fallback cannot repair it.
func TestArindbCanarySignalPagesOnRiskDrivenSupplyLoss(t *testing.T) {
	for _, bucket := range []string{"speed", "online"} {
		alerts := runArindbCanaryFixture(t, arindbCanaryFixture("latest", time.Minute, map[string]float64{bucket: 0.11}, map[string]float64{bucket + "/xx": 5}))
		alert := requireAlertClass(t, alerts, "arindb-risk-supply-loss")
		if alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), bucket+"=0.1100") || !strings.Contains(alert.Markdown(), bucket+":xx(5)") {
			t.Fatalf("%s risk loss alert:\n%s", bucket, alert.Markdown())
		}
	}
	// exactly the threshold holds, as the breaker does
	requireNoArindbCanaryAlerts(t, runArindbCanaryFixture(t, arindbCanaryFixture("latest", time.Minute, map[string]float64{"speed": 0.10, "online": 0.10}, nil)))
}

// An unavailable or warming canary warns after its hold, and a stale
// decision warns at once.
func TestArindbCanarySignalWarnsOnUnavailableWarmingAndStale(t *testing.T) {
	for _, c := range []struct {
		state   string
		age     time.Duration
		class   string
		sustain int
	}{
		{state: "single", age: time.Minute, class: "arindb-canary-unavailable", sustain: 60},
		{state: "warming", age: time.Minute, class: "arindb-canary-warming", sustain: 120},
		{state: "latest", age: 31 * time.Minute, class: "arindb-decision-stale", sustain: 1},
	} {
		alerts := runArindbCanaryFixture(t, arindbCanaryFixture(c.state, c.age, nil, nil))
		alert := requireAlertClass(t, alerts, c.class)
		if alert.Severity != SeverityWarn || alert.Sustain != c.sustain || len(alerts) != 1 {
			t.Errorf("%s: severity=%s sustain=%d alerts=%d", c.class, alert.Severity, alert.Sustain, len(alerts))
		}
	}
	requireNoArindbCanaryAlerts(t, runArindbCanaryFixture(t, arindbCanaryFixture("latest", 29*time.Minute, nil, nil)))
}

// The risk-loss page needs a sample: a bucket with less than 20 paired
// providers of old supply is too small to judge.
func TestArindbCanarySignalRiskLossNeedsASample(t *testing.T) {
	series := arindbCanaryFixture("latest", time.Minute, map[string]float64{"speed": 0.5}, nil)
	for i := range series {
		if series[i].metric == "urnetwork_stats_arindb_providers" && series[i].labels["bucket"] == "speed" {
			series[i].value = 19
		}
	}
	requireNoArindbCanaryAlerts(t, runArindbCanaryFixture(t, series))
}

// A tripped warming comparison, a lost protection, a cohort shrinking after a
// new pair or with failing latest lookups and a serving epoch switching back
// and forth page; a gain or gross churn above its threshold with a sample, an
// unverified latest served while warming and readers missing the decision
// warn; a decision 90 minutes old pages beside its stale warning.
func TestArindbCanarySignalPagesOnTheConditionsTheBreakerCannotSee(t *testing.T) {
	set := func(series []arindbCanaryFixtureSeries, metric string, labels map[string]string, value float64) []arindbCanaryFixtureSeries {
		for i := range series {
			if series[i].metric != metric {
				continue
			}
			matches := true
			for key, want := range labels {
				matches = matches && series[i].labels[key] == want
			}
			if matches {
				series[i].value = value
				return series
			}
		}
		return append(series, arindbCanaryFixtureSeries{metric: metric, labels: labels, value: value})
	}
	for _, c := range []struct {
		name     string
		edit     func([]arindbCanaryFixtureSeries) []arindbCanaryFixtureSeries
		age      time.Duration
		class    string
		severity Severity
		sustain  int
		want     string
	}{
		{name: "a flood", class: "arindb-supply-gain", severity: SeverityWarn, sustain: 30, want: "quality=0.3000(old 100)",
			edit: func(s []arindbCanaryFixtureSeries) []arindbCanaryFixtureSeries {
				return set(s, "urnetwork_stats_arindb_gain_ratio", map[string]string{"bucket": "quality"}, 0.3)
			}},
		{name: "a reshuffle", class: "arindb-supply-churn", severity: SeverityWarn, sustain: 30, want: "online=0.4000(old 100)",
			edit: func(s []arindbCanaryFixtureSeries) []arindbCanaryFixtureSeries {
				return set(s, "urnetwork_stats_arindb_churn_ratio", map[string]string{"bucket": "online"}, 0.4)
			}},
		{name: "a tripped warming comparison", class: "arindb-warming-tripped", severity: SeverityPage, sustain: 10, want: "warming_tripped",
			edit: func(s []arindbCanaryFixtureSeries) []arindbCanaryFixtureSeries {
				return set(s, "urnetwork_stats_arindb_decision_flags", map[string]string{"flag": "warming_tripped"}, 1)
			}},
		{name: "an unopened latest", class: "arindb-protection-lost", severity: SeverityPage, sustain: 10, want: "latest_unavailable",
			edit: func(s []arindbCanaryFixtureSeries) []arindbCanaryFixtureSeries {
				return set(s, "urnetwork_stats_arindb_decision_flags", map[string]string{"flag": "latest_unavailable"}, 1)
			}},
		{name: "single after a fallback", class: "arindb-protection-lost", severity: SeverityPage, sustain: 10, want: "fallback_dropped",
			edit: func(s []arindbCanaryFixtureSeries) []arindbCanaryFixtureSeries {
				return set(s, "urnetwork_stats_arindb_decision_flags", map[string]string{"flag": "fallback_dropped"}, 1)
			}},
		{name: "a cohort shrinking after a new pair", class: "arindb-cohort-shrink", severity: SeverityPage, sustain: 10, want: "12.0% below",
			edit: func(s []arindbCanaryFixtureSeries) []arindbCanaryFixtureSeries {
				s = set(s, "urnetwork_stats_arindb_cohort_providers", map[string]string{"reference": "current"}, 880)
				s = set(s, "urnetwork_stats_arindb_pair_since_seconds", nil, float64(arindbCanaryFixtureNow.Add(-time.Hour).Unix()))
				return set(s, "urnetwork_stats_arindb_cohort_loss_ratio", nil, 0.12)
			}},
		{name: "a cohort shrinking with failing latest lookups", class: "arindb-cohort-shrink", severity: SeverityPage, sustain: 10, want: "12.0% below",
			edit: func(s []arindbCanaryFixtureSeries) []arindbCanaryFixtureSeries {
				s = set(s, "urnetwork_stats_arindb_cohort_providers", map[string]string{"reference": "current"}, 880)
				s = set(s, arindbCanaryLatestLookupErrorsMetric, nil, 0.05)
				return set(s, "urnetwork_stats_arindb_cohort_loss_ratio", nil, 0.12)
			}},
		{name: "an unverified latest while warming", class: "arindb-warming-serves-latest", severity: SeverityWarn, sustain: 30, want: "unverified latest",
			edit: func(s []arindbCanaryFixtureSeries) []arindbCanaryFixtureSeries {
				return set(s, "urnetwork_stats_arindb_decision_flags", map[string]string{"flag": "warming_serves_latest"}, 1)
			}},
		{name: "flapping", class: "arindb-serving-flapping", severity: SeverityPage, sustain: 5, want: "changed 5 times",
			edit: func(s []arindbCanaryFixtureSeries) []arindbCanaryFixtureSeries {
				return set(s, "urnetwork_stats_arindb_serving_changes", nil, 5)
			}},
		{name: "readers missing the decision", class: "arindb-decision-reads-not-ok", severity: SeverityWarn, sustain: 15, want: "6.0% of decision reads",
			edit: func(s []arindbCanaryFixtureSeries) []arindbCanaryFixtureSeries {
				return set(s, arindbCanaryReadsNotOkMetric, nil, 0.06)
			}},
		{name: "a stalled export", class: "arindb-decision-stalled", severity: SeverityPage, sustain: 1, age: 91 * time.Minute, want: "1h31m0s old",
			edit: func(s []arindbCanaryFixtureSeries) []arindbCanaryFixtureSeries { return s }},
	} {
		age := c.age
		if age == 0 {
			age = time.Minute
		}
		alerts := runArindbCanaryFixture(t, c.edit(arindbCanaryFixture("latest", age, nil, nil)))
		alert := requireAlertClass(t, alerts, c.class)
		if alert.Severity != c.severity || alert.Sustain != c.sustain || !strings.Contains(alert.Markdown(), c.want) {
			t.Errorf("%s: severity=%s sustain=%d, want %s %d with %q:\n%s", c.name, alert.Severity, alert.Sustain, c.severity, c.sustain, c.want, alert.Markdown())
		}
	}
	// just below each threshold, or without a sample, nothing pages
	quiet := arindbCanaryFixture("latest", 89*time.Minute, nil, nil)
	quiet = set(quiet, "urnetwork_stats_arindb_gain_ratio", map[string]string{"bucket": "quality"}, 0.10)
	quiet = set(quiet, "urnetwork_stats_arindb_churn_ratio", map[string]string{"bucket": "quality"}, 0.20)
	quiet = set(quiet, "urnetwork_stats_arindb_serving_changes", nil, 4)
	quiet = set(quiet, arindbCanaryReadsNotOkMetric, nil, 0.05)
	quiet = set(quiet, "urnetwork_stats_arindb_cohort_providers", map[string]string{"reference": "reference"}, 99)
	quiet = set(quiet, "urnetwork_stats_arindb_cohort_loss_ratio", nil, 0.5)
	for _, alert := range runArindbCanaryFixture(t, quiet) {
		if alert.Class != "arindb-decision-stale" {
			t.Errorf("unexpected %s alert at the thresholds", alert.Class)
		}
	}
	// a cohort shrinking two hours after its pair, with no failing latest
	// lookups, is a Connect outage or a restart, not the release
	unrelated := arindbCanaryFixture("latest", time.Minute, nil, nil)
	unrelated = set(unrelated, "urnetwork_stats_arindb_pair_since_seconds", nil, float64(arindbCanaryFixtureNow.Add(-2*time.Hour).Unix()))
	unrelated = set(unrelated, "urnetwork_stats_arindb_cohort_loss_ratio", nil, 0.5)
	requireNoArindbCanaryAlerts(t, runArindbCanaryFixture(t, unrelated))
	// nor is a background of failed latest lookups, such as GeoLite2 misses
	// on classifier-v0 records, at 1% or less of them
	background := set(unrelated, arindbCanaryLatestLookupErrorsMetric, nil, 0.01)
	requireNoArindbCanaryAlerts(t, runArindbCanaryFixture(t, background))
}

// Missing gauges are not a healthy canary.
func TestArindbCanarySignalWarnsWhenNoTaskworkerExportsTheDecision(t *testing.T) {
	alerts := runArindbCanaryFixture(t, nil)
	if alert := requireAlertClass(t, alerts, "arindb-canary-unobservable"); alert.Severity != SeverityWarn || alert.Sustain != 15 {
		t.Fatalf("unobservable alert: %+v", alert)
	}
}
