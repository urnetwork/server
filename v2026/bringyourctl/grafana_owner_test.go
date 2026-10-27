// Native command diagnostics must distinguish the existing main receiver from incident ownership without printing credentials or claiming an unconfigured route succeeded.
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/grafana"
)

// The previous caller text incorrectly described the owner address as the notification transport after the library switched to native main routing.
func TestGrafanaReconcileReportsReceiverAndOwnerSeparately(t *testing.T) {
	webhookUrl := "https://hooks.slack.example/services/T0/B0/" + grafanaTestWebhookSecret
	var output bytes.Buffer
	if err := grafanaReconcileAlerting(context.Background(), &output, newGrafanaAlertingFake(t).URL, "secret", grafana.AlertRoutingSettings{SlackWebhookUrl: webhookUrl}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "receiver "+grafana.DefaultAlertContactName) || !strings.Contains(text, "incident owner "+grafana.CompetitionIncidentContact) {
		t.Fatalf("transport and owner were not reported separately: %q", text)
	}
	if strings.Contains(text, "route to "+grafana.CompetitionIncidentContact) || strings.Contains(text, grafanaTestWebhookSecret) {
		t.Fatal("caller misidentified transport or printed the synthetic credential")
	}
}

// A native-library fail-closed result cannot become success-shaped command output.
func TestGrafanaReconcileUnconfiguredReceiverHasNoSuccessOutput(t *testing.T) {
	var output bytes.Buffer
	if err := grafanaReconcileAlerting(context.Background(), &output, newGrafanaAlertingFake(t).URL, "secret", grafana.AlertRoutingSettings{}); err == nil {
		t.Fatal("unconfigured existing receiver was accepted")
	}
	if output.Len() != 0 {
		t.Fatalf("unconfigured receiver emitted success output: %q", output.String())
	}
}
