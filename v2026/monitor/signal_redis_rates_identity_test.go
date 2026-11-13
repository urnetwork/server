// Returned Grafana identities cannot supply transport or access-error tokens.
package monitor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Force the public signal through each successful HTTP identity audit while
// independently healthy sibling definitions and source coverage remain live.
func TestRedisRatesWrongIdentityCannotChooseObservationClass(t *testing.T) {
	for _, dashboardMismatch := range []bool{true, false} {
		for _, token := range []string{"forbidden", "permission denied", "unauthorized", "timeout", "exit status 255"} {
			marker := "synthetic-private-returned-uid-" + token
			requests := 0
			client := grafanaHTTPClientFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				recorder := httptest.NewRecorder()
				if strings.Contains(request.URL.Path, "/dashboards/") {
					writeRedisRatesDashboardFixture(t, recorder, "5m", "")
					if dashboardMismatch {
						var response map[string]any
						if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
							t.Fatal(err)
						}
						response["dashboard"].(map[string]any)["uid"] = marker
						body, err := json.Marshal(response)
						if err != nil {
							t.Fatal(err)
						}
						return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
					}
					return recorder.Result(), nil
				}
				writeRedisRatesAlertRuleFixture(t, recorder, "5m", "")
				if !dashboardMismatch {
					var response map[string]any
					if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					response["uid"] = marker
					body, err := json.Marshal(response)
					if err != nil {
						t.Fatal(err)
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
				}
				return recorder.Result(), nil
			})
			settings, _ := redisRatesSyntheticSettings(t, redisRatesMimirFixture(3, 0, 3))
			settings.Grafana.AdminPassword = "synthetic-password"
			alerts, err := newRedisRatesSignal(client, "https://grafana.example").Run(context.Background(), settings)
			if err != nil || requests != 2 || len(alerts) != 1 {
				t.Fatalf("identity audit changed bounded source calls or healthy siblings: err=%v requests=%d alerts=%d", err, requests, len(alerts))
			}
			target := "synthetic-grafana.example.com/" + redisRatesAlertRuleUID
			if dashboardMismatch {
				target = "synthetic-grafana.example.com/" + redisRatesDashboardUID
			}
			found := false
			for _, alert := range alerts {
				if alert.Target == target {
					found = true
					if alert.Class != "cannot-observe" || !strings.Contains(alert.Observed, "error_class="+observationErrorClassInvalidResponse) || !alert.unobservable || alert.Severity != SeverityWarn || alert.Sustain != 2 {
						t.Errorf("dashboard=%t token=%q: response identity chose a different failure category", dashboardMismatch, token)
					}
				}
				requireAlertOmits(t, alert, marker, settings.Grafana.AdminPassword)
			}
			if !found {
				t.Fatal("successful HTTP wrong-identity response lost its visibility finding")
			}
		}
	}
}

// Errors themselves stay fixed before the shared classifier or logger sees them.
func TestRedisRatesIdentityAuditErrorsOmitReturnedValues(t *testing.T) {
	const marker = "synthetic-private-uid-forbidden-timeout"
	var dashboard redisRatesDashboardEnvelope
	dashboard.Dashboard.UID = marker
	_, dashboardErr := auditRedisRatesDashboard(dashboard)
	_, ruleErr := auditRedisRatesAlertRule(redisRatesAlertRule{UID: marker})
	for _, err := range []error{dashboardErr, ruleErr} {
		if err == nil || strings.Contains(err.Error(), marker) || classifyObservationError(err) != observationErrorClassInvalidResponse {
			t.Error("Grafana identity audit did not retain a fixed private-safe invalid-response result")
		}
	}
}
