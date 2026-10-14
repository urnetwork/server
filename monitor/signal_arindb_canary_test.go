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
	series = append(series,
		arindbCanaryFixtureSeries{metric: "urnetwork_stats_arindb_fallback_active", value: fallback},
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
			"urnetwork_stats_arindb_emptied_country", "urnetwork_stats_arindb_decision_seconds", "%22synthetic%22", "monitor_metric"} {
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

// Missing gauges are not a healthy canary.
func TestArindbCanarySignalWarnsWhenNoTaskworkerExportsTheDecision(t *testing.T) {
	alerts := runArindbCanaryFixture(t, nil)
	if alert := requireAlertClass(t, alerts, "arindb-canary-unobservable"); alert.Severity != SeverityWarn || alert.Sustain != 15 {
		t.Fatalf("unobservable alert: %+v", alert)
	}
}
