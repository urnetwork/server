package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/grafana"
)

const grafanaTestWebhookSecret = "grafanaTestWebhookSecret01"

// A minimal Grafana alerting API whose root receiver has no integrations.
func newGrafanaAlertingFake(t *testing.T) *httptest.Server {
	var stateLock sync.Mutex
	contacts := []map[string]any{}
	policy := map[string]any{"receiver": "empty", "routes": []any{}}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		stateLock.Lock()
		defer stateLock.Unlock()
		body, _ := io.ReadAll(request.Body)
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/provisioning/contact-points":
			_ = json.NewEncoder(response).Encode(contacts)
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/provisioning/contact-points":
			contact := map[string]any{}
			_ = json.Unmarshal(body, &contact)
			contacts = append(contacts, contact)
			response.WriteHeader(http.StatusAccepted)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/provisioning/policies":
			_ = json.NewEncoder(response).Encode(policy)
		case request.Method == http.MethodPut && request.URL.Path == "/api/v1/provisioning/policies":
			policy = map[string]any{}
			_ = json.Unmarshal(body, &policy)
			response.WriteHeader(http.StatusAccepted)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/test"):
			_, _ = io.WriteString(response, `{"status":"failure","duration":"2s","error":"Post \"https://hooks.slack.example/services/T0/B0/`+grafanaTestWebhookSecret+`\": i/o timeout"}`)
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// The commands print routing names and verdicts, never the webhook, and an
// empty webhook warns that unrouted alerts reach no one.
func TestGrafanaAlertingCommandsNeverPrintTheWebhook(t *testing.T) {
	webhookUrl := "https://hooks.slack.example/services/T0/B0/" + grafanaTestWebhookSecret

	var skipped bytes.Buffer
	if err := grafanaReconcileAlerting(context.Background(), &skipped, newGrafanaAlertingFake(t).URL, "secret", grafana.AlertRoutingSettings{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(skipped.String(), "alerts.slack_webhook_url is empty") ||
		!strings.Contains(skipped.String(), "WARNING: the default receiver empty has no integrations") {
		t.Fatalf("empty-webhook output = %q", skipped.String())
	}

	grafanaUrl := newGrafanaAlertingFake(t).URL
	var configured bytes.Buffer
	if err := grafanaReconcileAlerting(context.Background(), &configured, grafanaUrl, "secret", grafana.AlertRoutingSettings{SlackWebhookUrl: webhookUrl}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(configured.String(), "default receiver "+grafana.DefaultAlertContactName+" (was empty)") {
		t.Fatalf("configured output = %q", configured.String())
	}

	var tested bytes.Buffer
	err := grafanaTestAlert(context.Background(), &tested, grafanaUrl, "secret", grafana.AlertRoutingSettings{SlackWebhookUrl: webhookUrl})
	if err == nil || !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("test-alert error = %v", err)
	}
	for _, output := range []string{skipped.String(), configured.String(), tested.String(), err.Error()} {
		if strings.Contains(output, grafanaTestWebhookSecret) || strings.Contains(output, "hooks.slack.example") {
			t.Fatalf("output quotes the webhook: %q", output)
		}
	}
}

// The webhook comes from vault monitor.yml; an absent resource is not an
// error, so environments without the webhook keep their routing.
func TestGrafanaAlertRoutingSettingsReadVaultMonitorYaml(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	if settings := grafanaAlertRoutingSettings(); settings.SlackWebhookUrl != "" {
		t.Fatalf("absent monitor.yml settings = %+v", settings)
	}
	pop := server.Vault.PushSimpleResource("monitor.yml", []byte("ssh:\n  user: monitor\nalerts:\n  slack_webhook_url: https://hooks.slack.example/services/T0/B0/"+grafanaTestWebhookSecret+"\n"))
	defer pop()
	if settings := grafanaAlertRoutingSettings(); !strings.HasSuffix(settings.SlackWebhookUrl, grafanaTestWebhookSecret) {
		t.Fatal("monitor.yml webhook was not read")
	}
}
