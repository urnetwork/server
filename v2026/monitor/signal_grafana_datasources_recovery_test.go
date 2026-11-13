package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// The real notifier state reducer, without a sender, webhook or wall clock.
// Default clearing still requires two complete absent executions.
func newGrafanaDatasourceRecoveryReducer() *SlackAlertNotifier {
	return &SlackAlertNotifier{
		settings:              DefaultSlackAlertNotifierSettings(),
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
}

// Use the public signal, production failure projection and cadence gate,
// then the actual notifier reducer. All source and sender I/O stays synthetic.
func observeGrafanaDatasourceRecovery(t *testing.T, ctx context.Context, signal Signal, settings SignalSettings, gate *cadenceAlertGate, notifier *SlackAlertNotifier) (Alerts, SignalExecution, []slackTransition, error) {
	t.Helper()
	alerts, err := signal.Run(ctx, settings)
	if err != nil {
		alerts = append(alerts, visibilityAlert(settings, signal, err))
	}
	assertGrafanaDatasourceAlertsPrivate(t, "recovery", alerts)
	ready, held := gate.split(signal, alerts)
	execution := SignalExecution{Alerts: ready, HeldAlerts: held, Failed: err != nil}
	notifier.stateLock.Lock()
	defer notifier.stateLock.Unlock()
	transitions := notifier.observeWithLock(settings.Now(), signal.Key(), execution)
	return alerts, execution, transitions, err
}

// Every generic failure keeps the prior plugin incident open, including the
// first generic sample still held below its own cadence threshold. Success
// through the actual request/parser path is the eventual recovery control.
func TestGrafanaDatasourcesGenericFailureCannotClearPluginIncident(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "unauthorized", status: 401, body: "synthetic-private-credential-detail"},
		{name: "forbidden", status: 403, body: "synthetic-private-credential-detail"},
		{name: "gateway", status: 502, body: "synthetic-private-response-detail"},
		{name: "invalid json", status: 200, body: "{"},
		{name: "missing result", status: 200, body: `{"results":{}}`},
		{name: "null result", status: 200, body: `{"results":{"A":null}}`},
		{name: "result status", status: 200, body: `{"results":{"A":{"status":503}}}`},
		{name: "result error", status: 200, body: `{"results":{"A":{"status":200,"error":"synthetic-private-response-detail"}}}`},
	} {
		phase := 0
		client := grafanaHTTPClientFunc(func(request *http.Request) (*http.Response, error) {
			var payload struct {
				Queries []struct {
					Datasource struct {
						Uid string `json:"uid"`
					} `json:"datasource"`
				} `json:"queries"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || len(payload.Queries) != 1 {
				t.Fatal("invalid synthetic datasource request")
			}
			if payload.Queries[0].Datasource.Uid == "warp-loki" {
				if phase == 0 {
					return grafanaFixtureResponse(404, "plugin.notRegistered"), nil
				}
				if phase == 1 {
					return grafanaFixtureResponse(test.status, test.body), nil
				}
			}
			return grafanaFixtureResponse(200, `{"results":{"A":{"status":200,"frames":[]}}}`), nil
		})
		settings := syntheticSettings(&syntheticSource{})
		settings.PublicDomain = "service.example.test"
		settings.Grafana.AdminPassword = grafanaDatasourcePrivacySentinels[0]
		signal := newGrafanaDatasourcesSignal(client, "https://grafana.example.test/api/ds/query")
		gate := newCadenceAlertGate()
		notifier := newGrafanaDatasourceRecoveryReducer()
		if notifier.settings.ClearAfterCompleteRuns != 2 {
			t.Fatal("control requires the native default clear count")
		}
		alerts, _, _, err := observeGrafanaDatasourceRecovery(t, context.Background(), signal, settings, gate, notifier)
		if err != nil || len(alerts) != 1 {
			t.Fatal("plugin failure fixture did not produce exactly one alert")
		}
		plugin := requireAlertClass(t, alerts, "grafana-plugin-unregistered")
		key := slackIncidentKey(plugin)
		if key != "observability/grafana-datasources|grafana-plugin-unregistered|synthetic-grafana.service.example.test/warp-loki" || notifier.keyIncidents[key] == nil {
			t.Fatal("fixture did not open the actual plugin incident key")
		}
		phase = 1
		for run := 0; run < 3; run++ {
			alerts, execution, transitions, err := observeGrafanaDatasourceRecovery(t, context.Background(), signal, settings, gate, notifier)
			if err != nil || len(alerts) != 1 {
				t.Fatalf("%s: generic failure lost its independent finding", test.name)
			}
			query := requireAlertClass(t, alerts, "grafana-datasource-query")
			if !query.unobservable || query.Frame != "warp-loki" || query.Sustain != 2 {
				t.Errorf("%s: failed control authorized plugin execution", test.name)
			}
			requireAlertOmits(t, query, "synthetic-private-credential-detail", "synthetic-private-response-detail")
			if run == 0 && (len(execution.HeldAlerts) != 1 || !execution.HeldAlerts[0].unobservable) {
				t.Errorf("%s: first held sample lost recovery visibility", test.name)
			}
			if notifier.keyIncidents[key] == nil {
				t.Errorf("%s: generic failure cleared the unobserved plugin incident", test.name)
			}
			for _, transition := range transitions {
				if transition.kind == slackCleared && slackIncidentKey(transition.alert) == key {
					t.Errorf("%s: announced plugin recovery without successful execution", test.name)
				}
			}
		}
		phase = 2
		for run := 0; run < 2; run++ {
			alerts, execution, transitions, err := observeGrafanaDatasourceRecovery(t, context.Background(), signal, settings, gate, notifier)
			if err != nil || len(alerts) != 0 || execution.Failed {
				t.Fatalf("%s: healthy bounded query control failed", test.name)
			}
			clears := 0
			for _, transition := range transitions {
				if transition.kind == slackCleared && slackIncidentKey(transition.alert) == key {
					clears++
				}
			}
			if run == 0 && (notifier.keyIncidents[key] == nil || clears != 0) {
				t.Errorf("%s: one complete sample cleared the prior incident", test.name)
			}
			if run == 1 && (notifier.keyIncidents[key] != nil || clears != 1) {
				t.Errorf("%s: two successful samples did not clear exactly once", test.name)
			}
		}
	}
}

// Missing body authority on one datasource preserves its prior incident and
// the sibling's independently proved plugin failure, with private text omitted.
func TestGrafanaDatasourcesPartialObservationKeepsPluginIncidentAndSibling(t *testing.T) {
	privateDetail := strings.Join(grafanaDatasourcePrivacySentinels, " ")
	for _, unavailable := range []func() (*http.Response, error){
		func() (*http.Response, error) { return nil, errors.New(privateDetail) },
		func() (*http.Response, error) {
			response := grafanaFixtureResponse(200, "")
			response.Body = io.NopCloser(io.MultiReader(strings.NewReader(privateDetail), iotest.ErrReader(errors.New(privateDetail))))
			return response, nil
		},
		func() (*http.Response, error) {
			return grafanaFixtureResponse(200, privateDetail+strings.Repeat("x", grafanaDatasourceResponseLimit)), nil
		},
	} {
		phase, calls := 0, 0
		client := grafanaHTTPClientFunc(func(*http.Request) (*http.Response, error) {
			index := calls % 2
			calls++
			if phase == 0 {
				if index == 0 {
					return grafanaFixtureResponse(404, "plugin.notRegistered"), nil
				}
				return grafanaFixtureResponse(200, `{"results":{"A":{"status":200}}}`), nil
			}
			if index == 0 {
				return unavailable()
			}
			return grafanaFixtureResponse(404, "plugin.notRegistered "+privateDetail), nil
		})
		settings := syntheticSettings(&syntheticSource{})
		settings.PublicDomain = "service.example.test"
		settings.Grafana.AdminPassword = grafanaDatasourcePrivacySentinels[0]
		signal := newGrafanaDatasourcesSignal(client, "https://grafana.example.test/api/ds/query")
		gate, notifier := newCadenceAlertGate(), newGrafanaDatasourceRecoveryReducer()
		alerts, _, _, err := observeGrafanaDatasourceRecovery(t, context.Background(), signal, settings, gate, notifier)
		if err != nil {
			t.Fatal("initial plugin observation failed")
		}
		key := slackIncidentKey(requireAlertClass(t, alerts, "grafana-plugin-unregistered"))
		phase = 1
		for run := 0; run < 3; run++ {
			alerts, _, _, err := observeGrafanaDatasourceRecovery(t, context.Background(), signal, settings, gate, notifier)
			if err != nil || len(alerts) != 2 || notifier.keyIncidents[key] == nil {
				t.Fatal("partial observation lost prior incident or independent sibling")
			}
			unknown := requireAlertClass(t, alerts, "cannot-observe")
			sibling := requireAlertClass(t, alerts, "grafana-plugin-unregistered")
			if !unknown.unobservable || !strings.HasSuffix(unknown.Target, "/warp-mimir") || sibling.unobservable || !strings.HasSuffix(sibling.Target, "/warp-loki") {
				t.Fatal("partial observation changed sibling or unknown authority")
			}
		}
	}
}

// A canceled/expired parent, including cancellation during a valid source
// return, cannot clear a prior incident or launch another sibling request.
func TestGrafanaDatasourcesEndedParentCannotClearPluginIncident(t *testing.T) {
	for _, ending := range []string{"before", "deadline", "valid return", "invalid return"} {
		phase, calls := 0, 0
		var end func()
		client := grafanaHTTPClientFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if phase == 0 {
				return grafanaFixtureResponse(404, "plugin.notRegistered"), nil
			}
			end()
			if ending == "invalid return" {
				return grafanaFixtureResponse(200, "{"), nil
			}
			return grafanaFixtureResponse(200, `{"results":{"A":{"status":200}}}`), nil
		})
		settings := syntheticSettings(&syntheticSource{})
		settings.PublicDomain = "service.example.test"
		settings.Grafana.AdminPassword = grafanaDatasourcePrivacySentinels[0]
		signal := newGrafanaDatasourcesSignal(client, "https://grafana.example.test/api/ds/query")
		gate, notifier := newCadenceAlertGate(), newGrafanaDatasourceRecoveryReducer()
		alerts, _, _, err := observeGrafanaDatasourceRecovery(t, context.Background(), signal, settings, gate, notifier)
		if err != nil || len(alerts) != 2 {
			t.Fatal("initial plugin observations failed")
		}
		key := slackIncidentKey(alerts[0])
		phase, calls = 1, 0
		ctx, cancel := context.WithCancel(context.Background())
		end = cancel
		if ending == "before" {
			cancel()
		}
		if ending == "deadline" {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Unix(0, 0))
			end = cancel
		}
		for run := 0; run < 2; run++ {
			_, execution, _, err := observeGrafanaDatasourceRecovery(t, ctx, signal, settings, gate, notifier)
			if !errors.Is(err, ctx.Err()) || !execution.Failed || notifier.keyIncidents[key] == nil {
				t.Errorf("%s: ended parent authorized incident resolution", ending)
			}
		}
		cancel()
		wantCalls := 0
		if ending == "valid return" || ending == "invalid return" {
			wantCalls = 1
		}
		if calls != wantCalls {
			t.Errorf("%s: source calls=%d want=%d", ending, calls, wantCalls)
		}
	}
}
