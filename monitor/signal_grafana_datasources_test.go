package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
)

func TestGrafanaDatasourcesSignalDetectsMissingLokiPlugin(t *testing.T) {
	const adminPassword = "synthetic-admin-password"
	seen := map[string]string{}
	var seenLock sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "admin" || password != adminPassword {
			t.Errorf("Grafana datasource auth = %q/%q/%t", username, password, ok)
		}
		var payload struct {
			Queries []struct {
				Expr       string `json:"expr"`
				Datasource struct {
					UID  string `json:"uid"`
					Type string `json:"type"`
				} `json:"datasource"`
			} `json:"queries"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode query: %s", err)
			return
		}
		if len(payload.Queries) != 1 {
			t.Errorf("query count = %d", len(payload.Queries))
			return
		}
		query := payload.Queries[0]
		seenLock.Lock()
		seen[query.Datasource.UID] = query.Datasource.Type + ":" + query.Expr
		seenLock.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if query.Datasource.UID == "warp-loki" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"statusCode":404,"messageId":"plugin.notRegistered","message":"Plugin not registered"}`))
			return
		}
		w.Write([]byte(`{"results":{"A":{"status":200,"frames":[]}}}`))
	}))
	defer server.Close()

	settings := syntheticSettings(&syntheticSource{})
	settings.Environment = "main"
	settings.PublicDomain = "example.com"
	settings.Grafana.AdminPassword = adminPassword
	alerts, err := newGrafanaDatasourcesSignal(server.Client(), server.URL).Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "grafana-plugin-unregistered")
	if alert.Target != "main-grafana.example.com/warp-loki" || alert.Frame != "loki" {
		t.Fatalf("Loki plugin alert identity = %s/%s", alert.Target, alert.Frame)
	}
	for _, want := range []string{
		"plugin=\"loki\"",
		"image/plugin packaging failure",
		"catalog-checksum-pinned loki plugin",
		"Do not recreate warp-loki",
		"deploy-readiness gate",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("Loki plugin alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Markdown(), adminPassword) {
		t.Fatal("Grafana datasource alert leaked the admin password")
	}
	if got := seen["warp-mimir"]; got != "prometheus:vector(1)" {
		t.Fatalf("Mimir control query = %q", got)
	}
	if got := seen["warp-loki"]; got != `loki:sum(count_over_time({service="web"}[1m]))` {
		t.Fatalf("Loki control query = %q", got)
	}
}

func TestGrafanaDatasourcesSignalHealthy(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":{"A":{"status":200,"frames":[]}}}`))
	}))
	defer server.Close()

	settings := syntheticSettings(&syntheticSource{})
	settings.Environment = "main"
	settings.PublicDomain = "example.com"
	settings.Grafana.AdminPassword = "test"
	alerts, err := newGrafanaDatasourcesSignal(server.Client(), server.URL).Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy datasource alerts = %+v", alerts)
	}
	if requests != len(requiredGrafanaDatasourceQueries) {
		t.Fatalf("datasource requests = %d, want %d", requests, len(requiredGrafanaDatasourceQueries))
	}
}

func TestGrafanaDatasourcesSignalRejectsEmbeddedQueryFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":{"A":{"status":500,"error":"downstream timeout","frames":[]}}}`))
	}))
	defer server.Close()

	settings := syntheticSettings(&syntheticSource{})
	settings.Environment = "main"
	settings.PublicDomain = "example.com"
	settings.Grafana.AdminPassword = "test"
	alerts, err := newGrafanaDatasourcesSignal(server.Client(), server.URL).Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != len(requiredGrafanaDatasourceQueries) {
		t.Fatalf("embedded query alerts = %d, want %d", len(alerts), len(requiredGrafanaDatasourceQueries))
	}
	for _, alert := range alerts {
		if alert.Class != "grafana-datasource-query" || !strings.Contains(alert.Observed, "error_class=result-error") {
			t.Fatalf("embedded query alert = %+v", alert)
		}
		requireAlertOmits(t, alert, "downstream timeout")
	}
}

// Every value is invented; none is a live credential, recipient, or URL.
var grafanaDatasourcePrivacySentinels = []string{
	"synthetic-grafana-credential-sentinel",
	"synthetic-grafana-bearer-sentinel",
	"synthetic-recipient@example.test",
	"synthetic-customer-sentinel",
	"https://signed.example.test/fixture?signature=synthetic-signed-url-sentinel",
}

// Check every string field before rendering, and both public renderers. Report
// only fixture indexes and field names so a failed privacy gate stays safe.
func assertGrafanaDatasourceAlertsPrivate(t *testing.T, name string, alerts Alerts) {
	t.Helper()
	check := func(field, value string) {
		for index, sentinel := range grafanaDatasourcePrivacySentinels {
			if strings.Contains(value, sentinel) {
				t.Errorf("%s %s retained upstream sentinel %d", name, field, index)
				break
			}
		}
	}
	for index, alert := range alerts {
		value := reflect.ValueOf(alert)
		for fieldIndex := 0; fieldIndex < value.NumField(); fieldIndex++ {
			field := value.Field(fieldIndex)
			if field.Kind() == reflect.String {
				check(fmt.Sprintf("alert %d field %s", index, value.Type().Field(fieldIndex).Name), field.String())
			}
		}
		check(fmt.Sprintf("alert %d Markdown", index), alert.Markdown())
	}
	check("collection Markdown", alerts.Markdown())
	var output strings.Builder
	if err := alerts.WriteJSONL(&output); err != nil {
		t.Fatal("synthetic alert JSONL rendering failed")
	}
	check("collection JSONL", output.String())
}

// Exercise the real request/parser/reducer/Alert path without opening a socket.
// Each case remains a failure; only fixed classes and status fields may escape.
func TestGrafanaDatasourcesSignalOmitsUpstreamResponseText(t *testing.T) {
	privateDetail := strings.Join(grafanaDatasourcePrivacySentinels, " ")
	encodedDetail, err := json.Marshal(privateDetail)
	if err != nil {
		t.Fatal("synthetic error encoding failed")
	}
	encodedPluginDetail, err := json.Marshal("PLUGIN \n NOT REGISTERED " + privateDetail)
	if err != nil {
		t.Fatal("synthetic plugin error encoding failed")
	}
	cases := []struct {
		name         string
		httpStatus   int
		body         string
		errorClass   string
		resultStatus int
		resultSeen   bool
	}{
		{name: "http-error", httpStatus: 502, body: privateDetail, errorClass: "http-status"},
		{name: "embedded-error", httpStatus: 200, body: `{"results":{"A":{"status":500,"error":` + string(encodedDetail) + `}}}`, errorClass: "result-error", resultStatus: 500, resultSeen: true},
		{name: "successful-status-with-error", httpStatus: 200, body: `{"results":{"A":{"status":200,"error":` + string(encodedDetail) + `}}}`, errorClass: "result-error", resultStatus: 200, resultSeen: true},
		{name: "invalid-json", httpStatus: 200, body: "{" + privateDetail, errorClass: "invalid-json"},
		{name: "invalid-schema", httpStatus: 200, body: `{"results":{"A":{"status":` + string(encodedDetail) + `}}}`, errorClass: "invalid-json"},
		{name: "missing-result", httpStatus: 200, body: `{"results":{},"message":` + string(encodedDetail) + `}`, errorClass: "missing-result"},
		{name: "result-status", httpStatus: 200, body: `{"results":{"A":{"status":503}},"message":` + string(encodedDetail) + `}`, errorClass: "result-status", resultStatus: 503, resultSeen: true},
		{name: "null-result", httpStatus: 200, body: `{"results":{"A":null},"message":` + string(encodedDetail) + `}`, errorClass: "result-status", resultSeen: true},
		{name: "http-plugin-error", httpStatus: 404, body: `{"messageId":"plugin.notRegistered","message":` + string(encodedDetail) + `}`, errorClass: "plugin-not-registered"},
		{name: "embedded-plugin-error", httpStatus: 200, body: `{"results":{"A":{"status":500,"error":` + string(encodedPluginDetail) + `}}}`, errorClass: "plugin-not-registered", resultStatus: 500, resultSeen: true},
	}
	for _, c := range cases {
		requests := 0
		client := grafanaHTTPClientFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			username, password, ok := request.BasicAuth()
			if !ok || username != "admin" || password != grafanaDatasourcePrivacySentinels[0] {
				t.Errorf("%s synthetic request credential mismatch", c.name)
			}
			return grafanaFixtureResponse(c.httpStatus, c.body), nil
		})
		settings := syntheticSettings(&syntheticSource{})
		settings.PublicDomain = "service.example.test"
		settings.Grafana.AdminPassword = grafanaDatasourcePrivacySentinels[0]
		alerts, err := newGrafanaDatasourcesSignal(client, "https://grafana.example.test/api/ds/query").Run(context.Background(), settings)
		if err != nil {
			t.Errorf("%s synthetic signal returned an execution error", c.name)
			continue
		}
		assertGrafanaDatasourceAlertsPrivate(t, c.name, alerts)
		if requests != 2 || len(alerts) != 2 {
			t.Errorf("%s requests=%d alerts=%d; want both independent datasource failures", c.name, requests, len(alerts))
			continue
		}
		for index, alert := range alerts {
			spec := requiredGrafanaDatasourceQueries[index]
			wantClass, wantFrame, wantSustain := "grafana-datasource-query", spec.uid, 2
			if c.errorClass == "plugin-not-registered" {
				wantClass, wantFrame, wantSustain = "grafana-plugin-unregistered", spec.pluginID, 1
			}
			// A generic failure proves the query symptom, while plugin execution
			// remains unknown; the exact plugin marker remains a concrete failure.
			wantUnobservable := c.errorClass != "plugin-not-registered"
			if alert.Class != wantClass || alert.Frame != wantFrame || alert.Sustain != wantSustain || alert.Severity != SeverityWarn || alert.unobservable != wantUnobservable {
				t.Errorf("%s alert %d changed failure classification or sustain", c.name, index)
			}
			wantIdentity := "observability/grafana-datasources|" + wantClass + "|synthetic-grafana.service.example.test/" + spec.uid + "|" + wantFrame
			if alert.Identity() != wantIdentity {
				t.Errorf("%s alert %d changed stable identity", c.name, index)
			}
			wantObserved := fmt.Sprintf("datasource=%s type=%s plugin=%q http_status=%d result_status=%d result_seen=%t error_class=%s", spec.uid, spec.typeName, spec.pluginID, c.httpStatus, c.resultStatus, c.resultSeen, c.errorClass)
			if alert.Observed != wantObserved {
				t.Errorf("%s alert %d omitted the fixed response classification", c.name, index)
			}
		}
	}
}

// An otherwise successful envelope can contain arbitrary frame/metadata text;
// it must stay healthy even when unused text mentions the plugin discriminator.
func TestGrafanaDatasourcesSignalPrivacyKeepsHealthyControls(t *testing.T) {
	encodedDetail, err := json.Marshal("plugin.notRegistered " + strings.Join(grafanaDatasourcePrivacySentinels, " "))
	if err != nil {
		t.Fatal("synthetic metadata encoding failed")
	}
	requests := 0
	client := grafanaHTTPClientFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return grafanaFixtureResponse(200, `{"results":{"A":{"status":200,"error":" \n\t ","frames":[],"metadata":`+string(encodedDetail)+`}}}`), nil
	})
	settings := syntheticSettings(&syntheticSource{})
	settings.PublicDomain = "service.example.test"
	settings.Grafana.AdminPassword = grafanaDatasourcePrivacySentinels[0]
	alerts, err := newGrafanaDatasourcesSignal(client, "https://grafana.example.test/api/ds/query").Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || requests != 2 {
		t.Fatal("unused upstream metadata changed the healthy datasource controls")
	}
}

// Failed, partial, and oversized observations stay unknown while the sibling's
// concrete plugin or query failure survives. All error content remains private.
func TestGrafanaDatasourcesSignalPrivacyKeepsIndependentFailure(t *testing.T) {
	privateDetail := strings.Join(grafanaDatasourcePrivacySentinels, " ")
	cases := []struct {
		name         string
		failureClass string
		unavailable  func() (*http.Response, error)
	}{
		{name: "transport-error", failureClass: "grafana-plugin-unregistered", unavailable: func() (*http.Response, error) {
			return nil, errors.New(privateDetail)
		}},
		{name: "partial-body", failureClass: "grafana-datasource-query", unavailable: func() (*http.Response, error) {
			response := grafanaFixtureResponse(200, "")
			response.Body = io.NopCloser(io.MultiReader(strings.NewReader(privateDetail), iotest.ErrReader(errors.New(privateDetail))))
			return response, nil
		}},
		{name: "oversized-body", failureClass: "grafana-plugin-unregistered", unavailable: func() (*http.Response, error) {
			return grafanaFixtureResponse(200, privateDetail+strings.Repeat("x", grafanaDatasourceResponseLimit)), nil
		}},
	}
	for _, c := range cases {
		requests := 0
		client := grafanaHTTPClientFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			var payload struct {
				Queries []struct {
					Datasource struct {
						Uid string `json:"uid"`
					} `json:"datasource"`
				} `json:"queries"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || len(payload.Queries) != 1 {
				t.Errorf("%s invalid synthetic control request", c.name)
				return nil, errors.New("invalid synthetic control request")
			}
			if payload.Queries[0].Datasource.Uid == "warp-mimir" {
				if c.failureClass == "grafana-datasource-query" {
					return grafanaFixtureResponse(502, privateDetail), nil
				}
				return grafanaFixtureResponse(404, "plugin.notRegistered "+privateDetail), nil
			}
			return c.unavailable()
		})
		settings := syntheticSettings(&syntheticSource{})
		settings.PublicDomain = "service.example.test"
		settings.Grafana.AdminPassword = grafanaDatasourcePrivacySentinels[0]
		alerts, err := newGrafanaDatasourcesSignal(client, "https://grafana.example.test/api/ds/query").Run(context.Background(), settings)
		if err != nil {
			t.Errorf("%s discarded the independent failure", c.name)
			continue
		}
		assertGrafanaDatasourceAlertsPrivate(t, c.name, alerts)
		if requests != 2 || len(alerts) != 2 {
			t.Errorf("%s did not preserve both datasource outcomes", c.name)
			continue
		}
		positive, unknown := alerts[0], alerts[1]
		if positive.Class != c.failureClass || positive.Target != "synthetic-grafana.service.example.test/warp-mimir" || positive.unobservable != (c.failureClass == "grafana-datasource-query") {
			t.Errorf("%s lost the independent concrete failure", c.name)
		}
		if unknown.Class != "cannot-observe" || unknown.Target != "synthetic-grafana.service.example.test/warp-loki" || !unknown.unobservable {
			t.Errorf("%s turned missing evidence into a datasource health verdict", c.name)
		}
	}
}

func TestGrafanaDatasourcesSignalRequiresAdminCredential(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.Environment = "main"
	settings.PublicDomain = "example.com"
	_, err := newGrafanaDatasourcesSignal(http.DefaultClient, "https://example.invalid/api/ds/query").Run(context.Background(), settings)
	if err == nil || !strings.Contains(err.Error(), "admin password is not configured") {
		t.Fatalf("missing Grafana credential error = %v", err)
	}
}
