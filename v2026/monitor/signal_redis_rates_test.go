package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestRedisRatesSignalDetectsLiveDashboardAndAlertWindows(t *testing.T) {
	const password = "synthetic-grafana-password"
	const privateBodyMarker = "must-not-enter-alert"
	server, paths := newRedisRatesGrafanaServer(t, password, "$__rate_interval", "2m", http.StatusOK, privateBodyMarker)
	defer server.Close()

	settings, observedQuery := redisRatesSyntheticSettings(t, redisRatesMimirFixture(3, 0, 3))
	settings.Grafana.AdminPassword = password
	alerts, err := newRedisRatesSignal(server.Client(), server.URL).Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("Redis rate alerts = %d, want 2: %+v", len(alerts), alerts)
	}

	dashboard := requireAlertClass(t, alerts, "redis-dashboard-rate-window")
	for _, want := range []string{
		"revision=2",
		"expected_rate_windows=5",
		"fixed_5m=0",
		"dynamic=5",
		"commands_raw=3 commands_1m=0 commands_5m=3",
		"server revision containing commit 3e59900c",
		"Do not restart Redis",
	} {
		if !strings.Contains(dashboard.Markdown(), want) {
			t.Fatalf("dashboard alert missing %q:\n%s", want, dashboard.Markdown())
		}
	}
	rule := requireAlertClass(t, alerts, "redis-alert-rate-window")
	for _, want := range []string{
		"expected_rate_windows=2",
		"fixed_5m=0",
		"other=2",
		"Warp revision containing commit a314e4d",
		"alert coverage loss",
	} {
		if !strings.Contains(rule.Markdown(), want) {
			t.Fatalf("alert-rule alert missing %q:\n%s", want, rule.Markdown())
		}
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, password, privateBodyMarker)
	}

	query := *observedQuery
	for _, metric := range redisRateMetrics {
		for _, want := range []string{metric.name, "[1m]", "[5m]", "monitor_check"} {
			if !strings.Contains(query, want) {
				t.Fatalf("Mimir coverage query missing %q: %s", want, query)
			}
		}
	}
	wantPaths := []string{
		"/api/dashboards/uid/" + redisRatesDashboardUID,
		"/api/v1/provisioning/alert-rules/" + redisRatesAlertRuleUID,
	}
	if strings.Join(*paths, ",") != strings.Join(wantPaths, ",") {
		t.Fatalf("Grafana paths = %#v, want %#v", *paths, wantPaths)
	}
}

func TestRedisRatesSignalHealthyWithFixedWindows(t *testing.T) {
	server, _ := newRedisRatesGrafanaServer(t, "test", "5m", "5m", http.StatusOK, "")
	defer server.Close()
	settings, _ := redisRatesSyntheticSettings(t, redisRatesMimirFixture(3, 0, 3))
	settings.Grafana.AdminPassword = "test"
	alerts, err := newRedisRatesSignal(server.Client(), server.URL).Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy Redis rate alerts = %+v", alerts)
	}
}

func TestRedisRatesSignalSeparatesSourceCoverageFromSafeDefinitions(t *testing.T) {
	server, _ := newRedisRatesGrafanaServer(t, "test", "5m", "5m", http.StatusOK, "")
	defer server.Close()
	settings, _ := redisRatesSyntheticSettings(t, redisRatesMimirFixture(3, 0, 2))
	settings.Grafana.AdminPassword = "test"
	alerts, err := newRedisRatesSignal(server.Client(), server.URL).Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("source-coverage alerts = %d, want 1: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "redis-rate-source-coverage")
	for _, want := range []string{
		"expected_nodes=3",
		"commands_raw=3 commands_1m=0 commands_5m=2",
		"changing it cannot restore a missing exporter",
		"Do not restart Redis",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("source alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestRedisRatesSignalDoesNotLeakGrafanaErrorBody(t *testing.T) {
	const privateMarker = "private-response-marker"
	server, _ := newRedisRatesGrafanaServer(t, "test", "5m", "5m", http.StatusUnauthorized, privateMarker)
	defer server.Close()
	settings, _ := redisRatesSyntheticSettings(t, redisRatesMimirFixture(3, 0, 3))
	settings.Grafana.AdminPassword = "test"
	alerts, err := newRedisRatesSignal(server.Client(), server.URL).Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("Grafana visibility alerts = %d, want 2: %+v", len(alerts), alerts)
	}
	for _, alert := range alerts {
		if alert.Class != "cannot-observe" || !strings.Contains(alert.Observed, "HTTP 401") {
			t.Fatalf("Grafana visibility alert = %+v", alert)
		}
		requireAlertOmits(t, alert, privateMarker)
	}
}

func TestParseRedisRateCoverageFailsClosedOnAmbiguousPartitions(t *testing.T) {
	fixtures := []string{
		`{"status":"success","data":{"resultType":"vector","result":[]}}`,
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"monitor_check":"unexpected"},"value":[1,"3"]}]}}`,
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"monitor_check":"commands_raw"},"value":[1,"1.5"]}]}}`,
	}
	for _, fixture := range fixtures {
		if _, err := parseRedisRateCoverage(fixture, 3); err == nil {
			t.Errorf("ambiguous Redis rate coverage parsed successfully: %s", fixture)
		}
	}
}

func redisRatesSyntheticSettings(t *testing.T, mimirResponse string) (SignalSettings, *string) {
	t.Helper()
	var observedQuery string
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" {
			return "", fmt.Errorf("unexpected Mimir host %s", host.Name)
		}
		marker := "query="
		start := strings.Index(command, marker)
		if start < 0 {
			return "", fmt.Errorf("Mimir command omitted query: %s", command)
		}
		encoded := command[start+len(marker):]
		encoded = strings.TrimSuffix(encoded, "'")
		decoded, err := url.QueryUnescape(encoded)
		if err != nil {
			return "", err
		}
		observedQuery = decoded
		return mimirResponse, nil
	}}
	settings := syntheticSettings(source)
	settings.PublicDomain = "example.com"
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	return settings, &observedQuery
}

func redisRatesMimirFixture(raw, oneMinute, fiveMinutes int) string {
	result := []map[string]any{}
	for _, metric := range redisRateMetrics {
		for window, count := range map[string]int{
			"raw": raw,
			"1m":  oneMinute,
			"5m":  fiveMinutes,
		} {
			result = append(result, map[string]any{
				"metric": map[string]string{"monitor_check": metric.short + "_" + window},
				"value":  []any{1788774000, strconv.Itoa(count)},
			})
		}
	}
	body, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func newRedisRatesGrafanaServer(
	t *testing.T,
	password string,
	dashboardWindow string,
	ruleWindow string,
	status int,
	privateMarker string,
) (*httptest.Server, *[]string) {
	t.Helper()
	paths := []string{}
	var pathsLock sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, gotPassword, ok := r.BasicAuth()
		if !ok || username != "admin" || gotPassword != password {
			t.Errorf("Grafana auth = %q/%q/%t", username, gotPassword, ok)
		}
		pathsLock.Lock()
		paths = append(paths, r.URL.Path)
		pathsLock.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, privateMarker)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/dashboards/uid/" + redisRatesDashboardUID:
			writeRedisRatesDashboardFixture(t, w, dashboardWindow, privateMarker)
		case "/api/v1/provisioning/alert-rules/" + redisRatesAlertRuleUID:
			writeRedisRatesAlertRuleFixture(t, w, ruleWindow, privateMarker)
		default:
			http.NotFound(w, r)
		}
	}))
	return server, &paths
}

func writeRedisRatesDashboardFixture(t *testing.T, w http.ResponseWriter, window, privateMarker string) {
	t.Helper()
	dashboard := map[string]any{
		"uid": redisRatesDashboardUID, "version": 2, "private": privateMarker,
		"panels": []any{
			map[string]any{"id": 8, "targets": []any{map[string]string{"expr": "sum(rate(redis_commands_duration_seconds_total{env=\"$env\"}[" + window + "])) / sum(rate(redis_commands_processed_total{env=\"$env\"}[" + window + "]))"}}},
			map[string]any{"id": 9, "targets": []any{map[string]string{"expr": "sum(rate(redis_commands_processed_total{env=\"$env\"}[" + window + "]))"}}},
			map[string]any{"id": 11, "targets": []any{
				map[string]string{"expr": "sum(rate(redis_evicted_keys_total{env=\"$env\"}[" + window + "]))"},
				map[string]string{"expr": "sum(rate(redis_expired_keys_total{env=\"$env\"}[" + window + "]))"},
			}},
		},
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"dashboard": dashboard}); err != nil {
		t.Errorf("encode dashboard: %s", err)
	}
}

func writeRedisRatesAlertRuleFixture(t *testing.T, w http.ResponseWriter, window, privateMarker string) {
	t.Helper()
	rule := map[string]any{
		"uid": redisRatesAlertRuleUID, "title": "RedisNodeWedged", "private": privateMarker,
		"data": []any{map[string]any{"model": map[string]string{"expr": "sum(rate(redis_commands_duration_seconds_total[" + window + "])) / sum(rate(redis_commands_processed_total[" + window + "]))"}}},
	}
	if err := json.NewEncoder(w).Encode(rule); err != nil {
		t.Errorf("encode alert rule: %s", err)
	}
}
