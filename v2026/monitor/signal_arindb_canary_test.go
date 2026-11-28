package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

var arindbCanaryFixtureNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// One exported gauge sample as the signal's query returns it.
type arindbCanaryFixtureSeries struct {
	metric     string
	labels     map[string]string
	value      float64
	observedAt time.Time
	rawValue   string
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
		for _, database := range []string{"old", "latest"} {
			series = append(series, arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_providers", labels: map[string]string{"bucket": bucket, "database": database}, value: 100})
		}
		for _, metric := range []string{"loss_ratio", "gain_ratio", "churn_ratio"} {
			value := 0.0
			if metric == "loss_ratio" {
				value = losses[bucket]
			}
			series = append(series, arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_" + metric, labels: map[string]string{"bucket": bucket}, value: value})
		}
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
		arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_serving_epoch_seconds", value: func() float64 {
			if state == "single" {
				return 0
			}
			return 1700000000
		}()},
		arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_decision_seconds", value: float64(arindbCanaryFixtureNow.Add(-age).Unix())},
		arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_coverage_ratio", labels: map[string]string{"database": "paired"}, value: 0.99},
	)
	for _, database := range []string{"latest", "old"} {
		series = append(series, arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_coverage_ratio", labels: map[string]string{"database": database}, value: 0.99})
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
		observedAt := s.observedAt
		if observedAt.IsZero() {
			observedAt = arindbCanaryFixtureNow
		}
		value := s.rawValue
		if value == "" {
			value = fmt.Sprintf("%v", s.value)
		}
		result = append(result, map[string]any{
			"metric": metric,
			"value":  []any{float64(observedAt.Unix()), value},
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

// The nil branch of statsPublishArindbCanary removes comparisons but keeps
// the previous decision time; controller's owning nil-publication test pins
// that producer contract. It must never become a complete quiet execution.
func arindbCanaryNilFixture(age time.Duration) []arindbCanaryFixtureSeries {
	series := []arindbCanaryFixtureSeries{}
	for _, sample := range arindbCanaryFixture("", age, nil, nil) {
		switch sample.metric {
		case "urnetwork_stats_arindb_canary_state", "urnetwork_stats_arindb_decision_flags", "urnetwork_stats_arindb_decision_seconds", "urnetwork_stats_arindb_fallback_active", "urnetwork_stats_arindb_serving_epoch_seconds", "urnetwork_stats_arindb_cohort_loss_ratio", "urnetwork_stats_arindb_serving_changes", "urnetwork_stats_arindb_pair_since_seconds":
			if sample.metric != "urnetwork_stats_arindb_decision_seconds" {
				sample.value = 0
			}
			series = append(series, sample)
		}
	}
	return series
}

func TestArindbCanarySignalReadableThenNilRetainsVisibility(t *testing.T) {
	requireNoArindbCanaryAlerts(t, runArindbCanaryFixture(t, arindbCanaryFixture("latest", time.Minute, nil, nil)))
	for _, age := range []time.Duration{time.Minute, 31 * time.Minute, 91 * time.Minute} {
		alerts := runArindbCanaryFixture(t, arindbCanaryNilFixture(age))
		visibility := requireAlertClass(t, alerts, "arindb-canary-unobservable")
		if !isVisibilityAlert(visibility) {
			t.Fatal("incomplete frame lost public visibility")
		}
		if age > 30*time.Minute {
			requireAlertClass(t, alerts, "arindb-decision-stale")
		}
		if age > 90*time.Minute {
			requireAlertClass(t, alerts, "arindb-decision-stalled")
		}
	}
}

func TestArindbCanarySignalRequiresEveryPublisherField(t *testing.T) {
	complete := arindbCanaryFixture("latest", time.Minute, nil, nil)
	for i, sample := range complete {
		if sample.metric == arindbCanaryReadsNotOkMetric || sample.metric == arindbCanaryLatestLookupErrorsMetric {
			continue
		}
		partial := append([]arindbCanaryFixtureSeries{}, complete[:i]...)
		partial = append(partial, complete[i+1:]...)
		if requireAlertClassCount(runArindbCanaryFixture(t, partial), "arindb-canary-unobservable") != 1 {
			t.Errorf("missing required %s %v looked complete", sample.metric, sample.labels)
		}
	}
}

func TestArindbCanarySignalMalformedPublisherFieldsStayUnknown(t *testing.T) {
	for _, c := range []struct {
		metric string
		value  float64
		raw    string
	}{
		{metric: "canary_state", value: 0.5},
		{metric: "decision_flags", value: 2},
		{metric: "fallback_active", value: -1},
		{metric: "providers", value: 0.5},
		{metric: "coverage_ratio", value: 1.1},
		{metric: "gain_ratio", value: -0.1},
		{metric: "loss_ratio", value: math.Inf(1)},
		{metric: "decision_seconds", value: float64(arindbCanaryFixtureNow.Add(time.Hour).Unix())},
		{metric: "pair_since_seconds", value: -1},
		{metric: "serving_changes", raw: "synthetic-private-marker"},
	} {
		series := arindbCanaryFixture("latest", time.Minute, nil, nil)
		for i := range series {
			if series[i].metric == "urnetwork_stats_arindb_"+c.metric {
				series[i].value, series[i].rawValue = c.value, c.raw
				break
			}
		}
		alerts := runArindbCanaryFixture(t, series)
		requireAlertClass(t, alerts, "arindb-canary-unobservable")
		for _, alert := range alerts {
			encoded, err := json.Marshal(alert)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(alert.Markdown()+string(encoded), "synthetic-private-marker") {
				t.Fatal("raw malformed sample escaped")
			}
		}
	}
}

func TestArindbCanarySignalAmbiguousStatesStayUnknown(t *testing.T) {
	for _, state := range []string{"single", "synthetic-private-marker"} {
		series := arindbCanaryFixture("latest", time.Minute, nil, nil)
		if state == "single" {
			for i := range series {
				if series[i].metric == "urnetwork_stats_arindb_canary_state" && series[i].labels["state"] == state {
					series[i].value = 1
				}
			}
		} else {
			series = append(series, arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_canary_state", labels: map[string]string{"state": state}, value: 1})
		}
		requireAlertClass(t, runArindbCanaryFixture(t, series), "arindb-canary-unobservable")
	}
}

func TestArindbCanarySignalPartialFramePreservesIndependentPositives(t *testing.T) {
	series := arindbCanaryFixture("fallback", 91*time.Minute, map[string]float64{"speed": 0.2}, nil)
	for i := range series {
		if series[i].metric == arindbCanaryReadsNotOkMetric {
			series[i].value = 0.2
		}
	}
	// Remove only a gain comparison; the other positive families remain valid.
	for i := range series {
		if series[i].metric == "urnetwork_stats_arindb_gain_ratio" {
			series = append(series[:i], series[i+1:]...)
			break
		}
	}
	alerts := runArindbCanaryFixture(t, series)
	for _, class := range []string{"arindb-canary-unobservable", "arindb-fallback-active", "arindb-risk-supply-loss", "arindb-decision-stale", "arindb-decision-stalled", "arindb-decision-reads-not-ok"} {
		requireAlertClass(t, alerts, class)
	}
}

func TestArindbCanarySignalOptionalNoAttemptsAndSmallSamplesAreValid(t *testing.T) {
	for _, state := range []string{"latest", "single", "warming"} {
		series := arindbCanaryFixture(state, time.Minute, map[string]float64{"speed": 0.9}, nil)
		for i := range series {
			sample := &series[i]
			if sample.metric == "urnetwork_stats_arindb_providers" {
				sample.value = 0
			}
			if sample.metric == "urnetwork_stats_arindb_pair_since_seconds" {
				sample.value = 0
			}
			if state == "warming" && sample.metric == "urnetwork_stats_arindb_serving_epoch_seconds" {
				sample.value = 0
			}
			if sample.metric == arindbCanaryReadsNotOkMetric || sample.metric == arindbCanaryLatestLookupErrorsMetric {
				sample.value = math.NaN()
			}
		}
		for _, alert := range runArindbCanaryFixture(t, series) {
			if alert.Class != "arindb-canary-unavailable" && alert.Class != "arindb-canary-warming" {
				t.Errorf("unexpected small/no-attempt %s alert", alert.Class)
			}
		}
	}
}

func TestArindbCanarySignalExpiredEvaluationCannotAttestAbsence(t *testing.T) {
	series := arindbCanaryFixture("latest", time.Minute, nil, nil)
	for i := range series {
		series[i].observedAt = arindbCanaryFixtureNow.Add(-31 * time.Minute)
	}
	requireAlertClass(t, runArindbCanaryFixture(t, series), "arindb-canary-unobservable")
}

func TestArindbCanarySignalNilVisibilityPreventsSlackAbsenceClear(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	signal := NewArindbCanarySignal()
	fallback := runArindbCanaryFixture(t, arindbCanaryFixture("fallback", time.Minute, nil, nil))
	notifier.Handle(signal, SignalExecution{Alerts: fallback})
	// Held visibility has to block the same absence policy before sustain
	// matures, even when the source transport itself succeeded.
	for i := 0; i < 3; i++ {
		notifier.Handle(signal, SignalExecution{HeldAlerts: runArindbCanaryFixture(t, arindbCanaryNilFixture(time.Minute))})
		harness.advance(time.Minute)
	}
	key := slackIncidentKey(requireAlertClass(t, fallback, "arindb-fallback-active"))
	notifier.stateLock.Lock()
	incident, present := notifier.keyIncidents[key]
	absent := 0
	if present {
		absent = incident.signalKeyAbsentRuns[signal.Key()]
	}
	notifier.stateLock.Unlock()
	if !present {
		t.Fatal("nil frame cleared fallback incident state")
	}
	if absent != 0 {
		t.Fatalf("nil frame counted as %d complete absence runs", absent)
	}
	closeSlackNotifier(t, notifier)
	for _, text := range webhook.posted() {
		if strings.Contains(text, "[CLEARED]") {
			t.Fatal("nil frame cleared fallback")
		}
	}
}

func TestArindbCanarySignalAbsentOptionalRatesAreValid(t *testing.T) {
	series := []arindbCanaryFixtureSeries{}
	for _, sample := range arindbCanaryFixture("latest", time.Minute, nil, nil) {
		if sample.metric != arindbCanaryReadsNotOkMetric && sample.metric != arindbCanaryLatestLookupErrorsMetric {
			series = append(series, sample)
		}
	}
	requireNoArindbCanaryAlerts(t, runArindbCanaryFixture(t, series))
}

func TestArindbCanarySignalPublisherStateCoherence(t *testing.T) {
	for _, state := range []string{"single", "latest", "fallback"} {
		series := arindbCanaryFixture(state, time.Minute, nil, nil)
		for i := range series {
			if series[i].metric == "urnetwork_stats_arindb_serving_epoch_seconds" {
				if state == "single" {
					series[i].value = 1
				} else {
					series[i].value = 0
				}
			}
		}
		requireAlertClass(t, runArindbCanaryFixture(t, series), "arindb-canary-unobservable")
	}
}

func TestArindbCanarySignalDuplicatePublisherRowIsUnknown(t *testing.T) {
	series := arindbCanaryFixture("latest", time.Minute, nil, nil)
	series = append(series, series[0])
	requireAlertClass(t, runArindbCanaryFixture(t, series), "arindb-canary-unobservable")
}
