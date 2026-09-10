package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
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
		if alert.Class != "grafana-datasource-query" || !strings.Contains(alert.Observed, "downstream timeout") {
			t.Fatalf("embedded query alert = %+v", alert)
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
