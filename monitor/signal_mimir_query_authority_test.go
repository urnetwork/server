// Query annotations require an exact serving contract before either owning
// reader can use returned values as incident or recovery authority.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Change only envelope metadata, leaving every returned sample unchanged.
func mimirQueryAnnotationFixture(t *testing.T, payload, field string, annotations any) string {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		t.Fatal(err)
	}
	if field != "" {
		envelope[field] = annotations
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// Supply the exact generated query's projected gauge shape through its real
// signal reader. The fixture is consumer evidence, never serving-engine proof.
func mimirQueryArindbSettings(t *testing.T, state string, field string, annotations any) SignalSettings {
	t.Helper()
	result := []map[string]any{}
	for _, series := range arindbCanaryFixture(state, 2*time.Hour, map[string]float64{"speed": 0.4}, nil) {
		metric := map[string]string{"monitor_metric": series.metric, "env": "synthetic"}
		for key, value := range series.labels {
			metric[key] = value
		}
		result = append(result, map[string]any{"metric": metric, "value": []any{arindbCanaryFixtureNow.Unix(), fmt.Sprint(series.value)}})
	}
	body, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	if err != nil {
		t.Fatal(err)
	}
	payload := mimirQueryAnnotationFixture(t, string(body), field, annotations)
	settings := syntheticSettings(&syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-source.example" || !strings.Contains(command, "monitor_metric") {
			t.Fatal("ARIN annotation fixture missed the real query boundary")
		}
		return payload, nil
	}})
	settings.Now = func() time.Time { return arindbCanaryFixtureNow }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-source.example", Roles: []string{"services"}})
	return settings
}

// Known float values have no granted omission-only or arithmetic authority
// under arbitrary warning/info metadata, even when all projected rows exist.
func TestArindbCanaryUnclassifiedAnnotationsWithholdQueryAuthority(t *testing.T) {
	const marker = "synthetic-private-query-annotation"
	for _, field := range []string{"warnings", "infos"} {
		for _, annotations := range [][]string{{marker}, {""}, {marker, marker}, {marker, "synthetic-unknown-extra"}} {
			for _, state := range []string{"latest", "fallback"} {
				alerts, err := NewArindbCanarySignal().Run(context.Background(), mimirQueryArindbSettings(t, state, field, annotations))
				if err != nil {
					t.Fatal(err)
				}
				if len(alerts) != 1 || alerts[0].Class != "arindb-canary-unobservable" {
					t.Errorf("field=%s state=%s: unqualified query created incident or healthy authority", field, state)
					continue
				}
				alert := alerts[0]
				if !alert.unobservable || alert.Severity != SeverityWarn || alert.Sustain != 15 || !strings.Contains(alert.Observed, "publisher_values=unknown") {
					t.Error("ARIN annotation uncertainty changed its visibility policy or fabricated data values")
				}
				requireAlertOmits(t, alert, marker, "synthetic-unknown-extra")
				encoded, err := json.Marshal(alert)
				if err != nil || strings.Contains(string(encoded), marker) || strings.Contains(string(encoded), "synthetic-unknown-extra") {
					t.Error("ARIN annotation reached structured Alert output")
				}
			}
		}
	}
}

// Empty and absent metadata preserve the existing positive predicate contract.
func TestArindbCanaryEmptyAnnotationsPreserveQualifiedFindings(t *testing.T) {
	for _, field := range []string{"", "warnings", "infos"} {
		for _, annotations := range []any{nil, []string{}} {
			alerts, err := NewArindbCanarySignal().Run(context.Background(), mimirQueryArindbSettings(t, "fallback", field, annotations))
			if err != nil {
				t.Fatal(err)
			}
			requireAlertClass(t, alerts, "arindb-fallback-active")
			requireAlertClass(t, alerts, "arindb-risk-supply-loss")
			requireAlertClass(t, alerts, "arindb-decision-stalled")
			if requireAlertClassCount(alerts, "arindb-canary-unobservable") != 0 {
				t.Error("empty annotation container removed established query authority")
			}
		}
	}
}

// Injected HTTP responses keep all query-authority controls local and bounded.
func mimirQuerySubscriptionSignal(status int) Signal {
	return newSubscriptionMetricsSignal(grafanaHTTPClientFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"dashboard":{"uid":"` + subscriptionMetricsDashboardUid + `","title":"` + subscriptionMetricsDashboardTitle + `"}}`))}, nil
	}), "https://grafana.example")
}

// An unqualified top result cannot prove stale, gap, future or healthy fleet
// state. PostgreSQL and dashboard findings retain their independent authority.
func TestSubscriptionMetricsUnclassifiedAnnotationsWithholdSnapshotAuthority(t *testing.T) {
	const marker = "synthetic-private-snapshot-annotation"
	for _, field := range []string{"warnings", "infos"} {
		for _, annotations := range [][]string{{marker}, {""}, {marker, marker}, {marker, "synthetic-extra-annotation"}} {
			for _, offset := range []time.Duration{-time.Minute, -time.Hour, time.Hour} {
				payload := subscriptionMetricsMimirFixture(t, subscriptionMetricsTestNow, subscriptionMetricsTestNow.Add(offset))
				payload = mimirQueryAnnotationFixture(t, payload, field, annotations)
				settings, _, _ := subscriptionMetricsSyntheticSettings([]Row{{"1", "0", "0", "300", "1", "0"}}, payload)
				settings.Grafana.AdminPassword = "synthetic-password"
				alerts, err := mimirQuerySubscriptionSignal(http.StatusOK).Run(context.Background(), settings)
				if err != nil {
					t.Fatal(err)
				}
				if len(alerts) != 1 || alerts[0].Class != "cannot-observe" || alerts[0].Target != "subscription-metrics/snapshot" || !alerts[0].unobservable {
					t.Errorf("field=%s offset=%s: unqualified top result retained fleet authority", field, offset)
					continue
				}
				requireAlertOmits(t, alerts[0], marker, "synthetic-extra-annotation", settings.Grafana.AdminPassword)
				encoded, err := json.Marshal(alerts)
				if err != nil || strings.Contains(string(encoded), marker) || strings.Contains(string(encoded), "synthetic-extra-annotation") {
					t.Error("snapshot annotation reached structured Alert output")
				}
			}
		}
	}
}

// Empty arrays and null remain absence of annotations for this exact reader.
func TestSubscriptionMetricsEmptyAnnotationsPreserveHealthyAuthority(t *testing.T) {
	for _, field := range []string{"warnings", "infos"} {
		for _, annotations := range []any{nil, []string{}} {
			payload := mimirQueryAnnotationFixture(t, subscriptionMetricsMimirFixture(t, subscriptionMetricsTestNow, subscriptionMetricsTestNow), field, annotations)
			settings, _, _ := subscriptionMetricsSyntheticSettings([]Row{{"1", "0", "0", "300", "1", "0"}}, payload)
			settings.Grafana.AdminPassword = "synthetic-password"
			alerts, err := mimirQuerySubscriptionSignal(http.StatusOK).Run(context.Background(), settings)
			if err != nil || len(alerts) != 0 {
				t.Error("empty annotation metadata removed a qualified healthy snapshot")
			}
		}
	}
}

// Metadata with the wrong type remains decode uncertainty; it cannot become
// an empty annotation array or disclose arbitrary server text.
func TestMimirQueryMalformedAnnotationMetadataStaysUnknown(t *testing.T) {
	const marker = "synthetic-private-malformed-annotation"
	for _, field := range []string{"warnings", "infos"} {
		for _, annotations := range []any{marker, map[string]string{"value": marker}, []any{marker, 1}} {
			alerts, err := NewArindbCanarySignal().Run(context.Background(), mimirQueryArindbSettings(t, "fallback", field, annotations))
			if err == nil || len(alerts) != 0 || strings.Contains(err.Error(), marker) {
				t.Error("malformed ARIN metadata retained query authority or private text")
			}
			payload := mimirQueryAnnotationFixture(t, subscriptionMetricsMimirFixture(t, subscriptionMetricsTestNow, subscriptionMetricsTestNow), field, annotations)
			settings, _, _ := subscriptionMetricsSyntheticSettings([]Row{{"1", "0", "0", "300", "1", "0"}}, payload)
			settings.Grafana.AdminPassword = "synthetic-password"
			alerts, err = mimirQuerySubscriptionSignal(http.StatusOK).Run(context.Background(), settings)
			if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !alerts[0].unobservable {
				t.Error("malformed snapshot metadata retained query authority")
				continue
			}
			requireAlertOmits(t, alerts[0], marker)
		}
	}
}

// Source failure cannot suppress independent task and dashboard problems.
func TestSubscriptionMetricsAnnotationsPreserveIndependentPositiveFindings(t *testing.T) {
	payload := mimirQueryAnnotationFixture(t, subscriptionMetricsMimirFixture(t, subscriptionMetricsTestNow, subscriptionMetricsTestNow), "infos", []string{"synthetic-private-annotation"})
	settings, _, _ := subscriptionMetricsSyntheticSettings([]Row{{"0", "0", "0", "300", "1", "0"}}, payload)
	settings.Grafana.AdminPassword = "synthetic-password"
	alerts, err := mimirQuerySubscriptionSignal(http.StatusNotFound).Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"cannot-observe", "subscription-metrics-task-chain", "subscription-dashboard-missing"} {
		requireAlertClass(t, alerts, class)
	}
	if len(alerts) != 3 {
		t.Fatal("unqualified snapshot added an unsupported query finding")
	}
}

// Both held and visible uncertainty hold previously proved incidents; only
// later annotation-free healthy executions can advance the existing clears.
func mimirQueryAnnotationsHoldIncidents(t *testing.T, signal Signal, initial, uncertain, healthy Alerts) {
	t.Helper()
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	notifier.Handle(signal, SignalExecution{Alerts: initial})
	for i := 0; i < 3; i++ {
		notifier.Handle(signal, SignalExecution{HeldAlerts: uncertain})
		notifier.Handle(signal, SignalExecution{Alerts: uncertain})
	}
	notifier.stateLock.Lock()
	for _, alert := range initial {
		incident, present := notifier.keyIncidents[slackIncidentKey(alert)]
		if !present || incident.signalKeyAbsentRuns[signal.Key()] != 0 {
			t.Error("query uncertainty advanced a proved incident's healthy clear")
		}
	}
	notifier.stateLock.Unlock()
	for i := 0; i < 2; i++ {
		notifier.Handle(signal, SignalExecution{Alerts: healthy})
	}
	notifier.stateLock.Lock()
	for _, alert := range initial {
		if _, present := notifier.keyIncidents[slackIncidentKey(alert)]; present {
			t.Error("qualified healthy observations did not resume the existing clear policy")
		}
	}
	notifier.stateLock.Unlock()
	closeSlackNotifier(t, notifier)
	for _, text := range webhook.posted() {
		if strings.Contains(text, "synthetic-private") {
			t.Error("private annotation reached Slack")
		}
	}
}

// A projected fallback remains held while query metadata is unqualified.
func TestArindbCanaryAnnotationsHoldExistingIncidentClears(t *testing.T) {
	signal := NewArindbCanarySignal()
	initial := runArindbCanaryFixture(t, arindbCanaryFixture("fallback", time.Minute, nil, nil))
	uncertain, err := signal.Run(context.Background(), mimirQueryArindbSettings(t, "latest", "infos", []string{"synthetic-private-annotation"}))
	if err != nil {
		t.Fatal(err)
	}
	healthy := runArindbCanaryFixture(t, arindbCanaryFixture("latest", time.Minute, nil, nil))
	mimirQueryAnnotationsHoldIncidents(t, signal, initial, uncertain, healthy)
}

// Snapshot uncertainty preserves the current signal-wide hold for independently
// proved task/dashboard incidents, even when those siblings later look healthy.
func TestSubscriptionMetricsAnnotationsHoldIndependentIncidentClears(t *testing.T) {
	payload := subscriptionMetricsMimirFixture(t, subscriptionMetricsTestNow, subscriptionMetricsTestNow)
	initialSettings, _, _ := subscriptionMetricsSyntheticSettings([]Row{{"0", "0", "0", "300", "1", "0"}}, payload)
	initialSettings.Grafana.AdminPassword = "synthetic-password"
	initial, err := mimirQuerySubscriptionSignal(http.StatusNotFound).Run(context.Background(), initialSettings)
	if err != nil {
		t.Fatal(err)
	}
	settings, _, _ := subscriptionMetricsSyntheticSettings([]Row{{"1", "0", "0", "300", "1", "0"}}, mimirQueryAnnotationFixture(t, payload, "warnings", []string{"synthetic-private-annotation"}))
	settings.Grafana.AdminPassword = "synthetic-password"
	signal := mimirQuerySubscriptionSignal(http.StatusOK)
	uncertain, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	healthySettings, _, _ := subscriptionMetricsSyntheticSettings([]Row{{"1", "0", "0", "300", "1", "0"}}, payload)
	healthySettings.Grafana.AdminPassword = "synthetic-password"
	healthy, err := signal.Run(context.Background(), healthySettings)
	if err != nil {
		t.Fatal(err)
	}
	mimirQueryAnnotationsHoldIncidents(t, signal, initial, uncertain, healthy)
}
