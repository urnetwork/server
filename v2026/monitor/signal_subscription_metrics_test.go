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
	"testing"
	"time"
)

var subscriptionMetricsTestNow = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

func TestSubscriptionMetricsSignalHealthyAcrossTaskSnapshotAndDashboard(t *testing.T) {
	const password = "synthetic-dashboard-password"
	requestedPath := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		username, gotPassword, ok := request.BasicAuth()
		if !ok || username != "admin" || gotPassword != password {
			t.Errorf("Grafana auth = %q/%q/%t", username, gotPassword, ok)
		}
		requestedPath = request.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"dashboard": map[string]string{
				"uid": subscriptionMetricsDashboardUid, "title": subscriptionMetricsDashboardTitle,
			},
		})
	}))
	defer server.Close()

	settings, observedSql, observedPromql := subscriptionMetricsSyntheticSettings(
		[]Row{{"1", "0", "0", "300", "1", "0"}},
		subscriptionMetricsMimirFixture(t, subscriptionMetricsTestNow, subscriptionMetricsTestNow.Add(-time.Minute)),
	)
	settings.Grafana.AdminPassword = password
	alerts, err := newSubscriptionMetricsSignal(server.Client(), server.URL).Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy subscription metrics alerts = %+v", alerts)
	}
	if requestedPath != "/api/dashboards/uid/"+subscriptionMetricsDashboardUid {
		t.Fatalf("Grafana path = %q", requestedPath)
	}
	for _, required := range []string{
		"monitor-signal-2.27-subscription-metrics-task-state",
		subscriptionMetricsTaskFunction,
		"run_end_time >= now() - interval '2 hours'",
		"post_completed",
		"post_error IS NOT NULL",
	} {
		if !strings.Contains(*observedSql, required) {
			t.Errorf("task-state SQL lacks %q: %s", required, *observedSql)
		}
	}
	for _, forbidden := range []string{"args_json", "result_json", "client_address", "task_id"} {
		if strings.Contains(*observedSql, forbidden) {
			t.Errorf("task-state SQL selects private field %q: %s", forbidden, *observedSql)
		}
	}
	for _, required := range []string{
		"topk(1,",
		"urnetwork_subscription_snapshot_timestamp_seconds",
		`service="taskworker"`,
		"timestamp(",
		"time() - 90",
		"on(env,service,block,host,instance)",
	} {
		if !strings.Contains(*observedPromql, required) {
			t.Errorf("snapshot PromQL lacks %q: %s", required, *observedPromql)
		}
	}
}

func TestSubscriptionMetricsTaskStateClassifiesSingletonFailures(t *testing.T) {
	states := []subscriptionMetricsTaskState{
		{taskCount: 0},
		{taskCount: 2},
		{taskCount: 1, taskErrorCount: 1},
		{taskCount: 1, finishedPresent: true, finishedAge: time.Minute, postError: true},
		{taskCount: 1, finishedPresent: true, finishedAge: time.Minute},
	}
	for index, state := range states {
		finding := subscriptionMetricsTaskStateFinding(state)
		if finding.healthy || finding.class != "subscription-metrics-task-chain" || finding.sustain != 2 {
			t.Errorf("task failure %d finding = %+v", index, finding)
		}
		for _, forbidden := range []string{"args_json", "result_json", "task-id", "synthetic-private-error"} {
			if strings.Contains(finding.observed+finding.evidence, forbidden) {
				t.Errorf("task failure %d retained %q", index, forbidden)
			}
		}
	}
	healthy := subscriptionMetricsTaskStateFinding(subscriptionMetricsTaskState{
		taskCount: 1, finishedPresent: true, finishedAge: time.Minute, postCompleted: true,
	})
	if !healthy.healthy {
		t.Fatalf("healthy task state = %+v", healthy)
	}
}

func TestSubscriptionMetricsSnapshotSeparatesExecutionFromPublication(t *testing.T) {
	recentCompletion := &subscriptionMetricsTaskState{
		taskCount: 1, finishedPresent: true, finishedAge: 5 * time.Minute, postCompleted: true,
	}
	cases := []struct {
		name        string
		observation subscriptionMetricsSnapshotObservation
		taskState   *subscriptionMetricsTaskState
		wantClass   string
	}{
		{
			name: "publication absent after completion", taskState: recentCompletion,
			wantClass: "subscription-metrics-publication-gap",
		},
		{
			name: "publication predates completion", taskState: recentCompletion,
			observation: subscriptionMetricsSnapshotObservation{
				present: true, snapshotTime: subscriptionMetricsTestNow.Add(-20 * time.Minute),
			},
			wantClass: "subscription-metrics-publication-gap",
		},
		{
			name: "task and snapshot stale",
			observation: subscriptionMetricsSnapshotObservation{
				present: true, snapshotTime: subscriptionMetricsTestNow.Add(-31 * time.Minute),
			},
			wantClass: "subscription-metrics-snapshot-stale",
		},
		{
			name: "future snapshot",
			observation: subscriptionMetricsSnapshotObservation{
				present: true, snapshotTime: subscriptionMetricsTestNow.Add(time.Minute),
			},
			wantClass: "subscription-metrics-snapshot-future",
		},
	}
	for _, test := range cases {
		findings := subscriptionMetricsSnapshotFindings(subscriptionMetricsTestNow, test.observation, test.taskState)
		active := []finding{}
		for _, finding := range findings {
			if !finding.healthy {
				active = append(active, finding)
			}
		}
		if len(active) != 1 || active[0].class != test.wantClass {
			t.Errorf("%s findings = %+v, want %s", test.name, active, test.wantClass)
		}
	}

	healthy := subscriptionMetricsSnapshotFindings(
		subscriptionMetricsTestNow,
		subscriptionMetricsSnapshotObservation{
			present: true, snapshotTime: subscriptionMetricsTestNow.Add(-5 * time.Minute),
		},
		recentCompletion,
	)
	for _, finding := range healthy {
		if !finding.healthy {
			t.Fatalf("healthy snapshot finding = %+v", finding)
		}
	}
}

func TestSubscriptionMetricsParsersFailClosedOnMalformedEvidence(t *testing.T) {
	rows := [][]pgRow{
		nil,
		{{"1", "0"}},
		{{"1", "0", "2", "10", "1", "0"}},
		{{"1", "0", "0", "-1", "1", "0"}},
		{{"1", "0", "0", "10", "2", "0"}},
	}
	for index, fixture := range rows {
		if _, err := parseSubscriptionMetricsTaskState(fixture); err == nil {
			t.Errorf("malformed task fixture %d parsed", index)
		}
	}

	mimirFixtures := []string{
		`not-json`,
		`{"status":"error","error":"synthetic"}`,
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1788004800,"1"]},{"metric":{},"value":[1788004800,"2"]}]}}`,
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1788004800,"1.5"]}]}}`,
	}
	for index, fixture := range mimirFixtures {
		if _, err := parseSubscriptionMetricsSnapshot(fixture, subscriptionMetricsTestNow); err == nil {
			t.Errorf("malformed Mimir fixture %d parsed", index)
		}
	}
}

func TestSubscriptionMetricsDashboardFailuresStayDistinctAndPrivate(t *testing.T) {
	const password = "synthetic-dashboard-password"
	const privateMarker = "private-response-body-marker"
	cases := []struct {
		status    int
		body      string
		wantClass string
	}{
		{status: http.StatusUnauthorized, body: privateMarker, wantClass: "subscription-dashboard-auth"},
		{status: http.StatusForbidden, body: privateMarker, wantClass: "subscription-dashboard-auth"},
		{status: http.StatusNotFound, body: privateMarker, wantClass: "subscription-dashboard-missing"},
		{status: http.StatusFound, body: privateMarker, wantClass: "subscription-dashboard-http"},
		{status: http.StatusServiceUnavailable, body: privateMarker, wantClass: "subscription-dashboard-http"},
		{
			status:    http.StatusTooManyRequests,
			body:      `{"dashboard":{"uid":"` + subscriptionMetricsDashboardUid + `","title":"` + subscriptionMetricsDashboardTitle + `","private":"` + privateMarker + `"}}`,
			wantClass: "subscription-dashboard-http",
		},
		{
			status:    http.StatusOK,
			body:      `{"dashboard":{"uid":"synthetic-wrong-uid","title":"synthetic wrong title","private":"` + privateMarker + `"}}`,
			wantClass: "subscription-dashboard-identity",
		},
	}
	for _, test := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			username, gotPassword, ok := request.BasicAuth()
			if !ok || username != "admin" || gotPassword != password {
				t.Errorf("Grafana auth = %q/%q/%t", username, gotPassword, ok)
			}
			w.WriteHeader(test.status)
			_, _ = fmt.Fprint(w, test.body)
		}))
		settings, _, _ := subscriptionMetricsSyntheticSettings(
			[]Row{{"1", "0", "0", "300", "1", "0"}},
			subscriptionMetricsMimirFixture(t, subscriptionMetricsTestNow, subscriptionMetricsTestNow.Add(-time.Minute)),
		)
		settings.Grafana.AdminPassword = password
		alerts, err := newSubscriptionMetricsSignal(server.Client(), server.URL).Run(context.Background(), settings)
		server.Close()
		if err != nil {
			t.Errorf("dashboard status %d: %v", test.status, err)
			continue
		}
		if len(alerts) != 1 || alerts[0].Class != test.wantClass {
			t.Errorf("dashboard status %d alerts = %+v, want %s", test.status, alerts, test.wantClass)
			continue
		}
		alert := alerts[0]
		wantIdentity := "observability/subscription-metrics|" + test.wantClass + "|" + settings.Environment + "-grafana." + settings.PublicDomain + "/" + subscriptionMetricsDashboardUid + "|"
		if alert.Identity() != wantIdentity || alert.Severity != SeverityPage || alert.Sustain != 2 {
			t.Errorf("dashboard status %d changed alert identity, severity, or sustain: %+v", test.status, alert)
		}
		if 200 <= test.status && test.status < 300 {
			if !strings.Contains(alert.Observed, "identity_observed=true uid_matches=false title_matches=false") {
				t.Errorf("decoded mismatched identity is unqualified: %s", alert.Observed)
			}
		} else if alert.Observed != fmt.Sprintf("http_status=%d identity_observed=false", test.status) {
			t.Errorf("non-2xx response claimed dashboard identity: %s", alert.Observed)
		}
		switch test.wantClass {
		case "subscription-dashboard-http":
			if !strings.Contains(alert.Mechanism, "responding component, dashboard presence, and dashboard identity remain unknown") ||
				!strings.Contains(alert.Action, "bounded same-window") {
				t.Errorf("status-only response lacks attribution boundary: %+v", alert)
			}
			requireAlertOmits(t, alert, "Repair Grafana authentication", "load-defaults", "Grafana returned", "uid_matches=", "title_matches=")
		case "subscription-dashboard-auth":
			if !strings.Contains(alert.Action, "admin credential") || !strings.Contains(alert.Action, "authentication policy") {
				t.Errorf("authentication response lacks credential/policy action: %s", alert.Action)
			}
			requireAlertOmits(t, alert, "load-defaults", "uid_matches=", "title_matches=")
		case "subscription-dashboard-missing":
			if !strings.Contains(alert.Action, "Confirm the 404 came from the intended Grafana API") || !strings.Contains(alert.Action, "load-defaults") {
				t.Errorf("missing response lacks qualified publication action: %s", alert.Action)
			}
		case "subscription-dashboard-identity":
			if !strings.Contains(alert.Action, "load-defaults") {
				t.Errorf("identity mismatch lacks publication action: %s", alert.Action)
			}
		}
		requireAlertOmits(t, alerts[0], password, privateMarker, "synthetic-wrong-uid", "synthetic wrong title")
	}
}

// A status-only refusal remains a failure until a separate qualified read.
func TestSubscriptionMetricsDashboard429DoesNotReplayAndLaterIdentityRecovers(t *testing.T) {
	const password = "synthetic-dashboard-password"
	body := `{"dashboard":{"uid":"` + subscriptionMetricsDashboardUid + `","title":"` + subscriptionMetricsDashboardTitle + `"}}`
	primaryCalls, ipv4Calls := 0, 0
	primary := grafanaHTTPClientFunc(func(request *http.Request) (*http.Response, error) {
		primaryCalls++
		username, gotPassword, ok := request.BasicAuth()
		if request.Method != http.MethodGet || request.URL.Path != "/api/dashboards/uid/"+subscriptionMetricsDashboardUid || !ok || username != "admin" || gotPassword != password {
			t.Error("dashboard request lost method, route, or synthetic authentication")
		}
		status := http.StatusOK
		if primaryCalls == 1 {
			status = http.StatusTooManyRequests
		}
		response := grafanaFixtureResponse(status, body)
		response.Header.Set("Retry-After", "120")
		return response, nil
	})
	ipv4 := grafanaHTTPClientFunc(func(*http.Request) (*http.Response, error) {
		ipv4Calls++
		return grafanaFixtureResponse(http.StatusOK, body), nil
	})
	settings, _, _ := subscriptionMetricsSyntheticSettings(
		[]Row{{"1", "0", "0", "300", "1", "0"}},
		subscriptionMetricsMimirFixture(t, subscriptionMetricsTestNow, subscriptionMetricsTestNow.Add(-time.Minute)),
	)
	settings.Grafana.AdminPassword = password
	signal := newSubscriptionMetricsSignal(newGrafanaObservationHTTPClientWithClients(primary, ipv4), "https://grafana.fixture.example")
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || alerts[0].Class != "subscription-dashboard-http" || alerts[0].Observed != "http_status=429 identity_observed=false" {
		t.Fatalf("429 did not retain the unknown dashboard boundary: alerts=%+v err=%v", alerts, err)
	}
	if primaryCalls != 1 || ipv4Calls != 0 {
		t.Fatalf("429 was replayed: primary=%d ipv4=%d", primaryCalls, ipv4Calls)
	}
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || primaryCalls != 2 || ipv4Calls != 0 {
		t.Fatalf("separate qualified identity read did not recover: alerts=%+v err=%v primary=%d ipv4=%d", alerts, err, primaryCalls, ipv4Calls)
	}
}

// A dashboard result cannot clear the separate snapshot observation failure.
func TestSubscriptionMetricsDashboard429PreservesSnapshotUnknown(t *testing.T) {
	settings, _, _ := subscriptionMetricsSyntheticSettings([]Row{{"1", "0", "0", "300", "1", "0"}}, "")
	settings.Source.(*syntheticSource).hostFn = func(HostSettings, string) (string, error) {
		return "", context.DeadlineExceeded
	}
	settings.Grafana.AdminPassword = "synthetic-dashboard-password"
	client := grafanaHTTPClientFunc(func(*http.Request) (*http.Response, error) {
		return grafanaFixtureResponse(http.StatusTooManyRequests, "synthetic refusal"), nil
	})
	alerts, err := newSubscriptionMetricsSignal(client, "https://grafana.fixture.example").Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatalf("dashboard and snapshot boundaries did not remain separate: alerts=%+v err=%v", alerts, err)
	}
	dashboard := requireAlertClass(t, alerts, "subscription-dashboard-http")
	snapshot := requireAlertClass(t, alerts, "cannot-observe")
	if dashboard.Observed != "http_status=429 identity_observed=false" || snapshot.Target != "subscription-metrics/snapshot" {
		t.Errorf("dashboard and snapshot observations were conflated: %+v", alerts)
	}
}

// A successful status without a complete document cannot observe identity.
func TestSubscriptionMetricsDashboardMalformedSuccessRemainsUnobservable(t *testing.T) {
	const privateMarker = "synthetic-private-dashboard-body"
	cases := []struct {
		body string
		err  error
	}{
		{body: privateMarker},
		{body: `{"dashboard":{"uid":"` + privateMarker},
		{body: privateMarker + strings.Repeat("x", subscriptionMetricsDashboardBodyLimit)},
		{err: context.DeadlineExceeded},
	}
	for index, test := range cases {
		client := grafanaHTTPClientFunc(func(*http.Request) (*http.Response, error) {
			if test.err != nil {
				return nil, test.err
			}
			return grafanaFixtureResponse(http.StatusOK, test.body), nil
		})
		settings, _, _ := subscriptionMetricsSyntheticSettings(
			[]Row{{"1", "0", "0", "300", "1", "0"}},
			subscriptionMetricsMimirFixture(t, subscriptionMetricsTestNow, subscriptionMetricsTestNow.Add(-time.Minute)),
		)
		settings.Grafana.AdminPassword = "synthetic-dashboard-password"
		alerts, err := newSubscriptionMetricsSignal(client, "https://grafana.fixture.example").Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 || alerts[0].SignalID != "monitor/visibility" || alerts[0].Class != "cannot-observe" {
			t.Errorf("unavailable dashboard case %d is not unknown: alerts=%+v err=%v", index, alerts, err)
			continue
		}
		requireAlertOmits(t, alerts[0], privateMarker, settings.Grafana.AdminPassword, "uid_matches=", "title_matches=")
	}
}

func TestSubscriptionMetricsDashboardMissingCredentialIsAuthFailure(t *testing.T) {
	settings, _, _ := subscriptionMetricsSyntheticSettings(
		[]Row{{"1", "0", "0", "300", "1", "0"}},
		subscriptionMetricsMimirFixture(t, subscriptionMetricsTestNow, subscriptionMetricsTestNow.Add(-time.Minute)),
	)
	alerts, err := NewSubscriptionMetricsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Class != "subscription-dashboard-auth" {
		t.Fatalf("missing dashboard credential alerts = %+v", alerts)
	}
	if alerts[0].Observed != "http_status=0 identity_observed=false" || !strings.Contains(alerts[0].Action, "admin credential") {
		t.Errorf("missing credential claimed dashboard identity or lost authentication action: %+v", alerts[0])
	}
	requireAlertOmits(t, alerts[0], "load-defaults", "uid_matches=", "title_matches=")
}

func subscriptionMetricsSyntheticSettings(taskRows []Row, mimirResponse string) (SignalSettings, *string, *string) {
	var observedSql string
	var observedPromql string
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			observedSql = query
			return taskRows, nil
		},
		hostFn: func(host HostSettings, command string) (string, error) {
			if host.Name != "metrics-1" {
				return "", fmt.Errorf("unexpected Mimir host %s", host.Name)
			}
			marker := "query="
			start := strings.Index(command, marker)
			if start < 0 {
				return "", fmt.Errorf("Mimir command omitted query")
			}
			encoded := strings.TrimSuffix(command[start+len(marker):], "'")
			query, err := url.QueryUnescape(encoded)
			if err != nil {
				return "", err
			}
			observedPromql = query
			return mimirResponse, nil
		},
	}
	settings := syntheticSettings(source)
	settings.PublicDomain = "example.com"
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	return settings, &observedSql, &observedPromql
}

func subscriptionMetricsMimirFixture(t *testing.T, observedTime, snapshotTime time.Time) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "vector",
			"result": []any{
				map[string]any{
					"metric": map[string]string{
						"env": "synthetic", "service": "taskworker", "host": "worker.fixture.example", "block": "g1", "instance": "fixture-instance",
					},
					"value": []any{observedTime.Unix(), strconv.FormatInt(snapshotTime.Unix(), 10)},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
