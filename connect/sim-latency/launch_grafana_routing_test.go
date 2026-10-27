// Launch preflight authenticates the actual existing-main integration and canonical competition route, not receiver-name substrings.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	servergrafana "github.com/urnetwork/server/grafana"
)

// Synthetic GET-only fixtures keep routing semantics independent of real recipients, credentials and service endpoints.
func launchGrafanaRoutingServer(t *testing.T, contacts []any, policy map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("preflight attempted %s", request.Method)
			response.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if request.Header.Get("Authorization") != "Bearer synthetic-monitoring-token" {
			t.Error("missing synthetic credential")
		}
		response.Header().Set("Content-Type", "application/json")
		var body any
		switch request.URL.Path {
		case "/api/health":
			body = map[string]any{"database": "ok"}
		case "/api/search":
			body = []any{map[string]any{"uid": "urnetwork-competition"}}
		case "/api/v1/provisioning/alert-rules":
			body = []any{map[string]any{"uid": "competition-runner-heartbeat-stale"}, map[string]any{"uid": "competition-minio-capacity-warning"}, map[string]any{"uid": "competition-minio-capacity-critical"}}
		case "/api/v1/provisioning/contact-points":
			body = contacts
		case "/api/v1/provisioning/policies":
			body = policy
		default:
			response.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(response).Encode(body); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// A stored, redacted native main integration is sufficient; no SMTP recipient or webhook secret is needed.
func launchExistingMainContacts() []any {
	return []any{map[string]any{"uid": servergrafana.DefaultAlertContactUid, "name": servergrafana.DefaultAlertContactName, "type": "slack", "settings": map[string]any{"url": "[REDACTED]"}}}
}

// The typed policy must contain an actual canonical child route, not merely mention a known receiver elsewhere.
func launchCanonicalCompetitionRoute(receiver string) map[string]any {
	return map[string]any{"receiver": receiver, "object_matchers": []any{[]any{"service", "=", "sim-latency"}, []any{"severity", "=~", "warn|page"}}}
}

// The obsolete email-substring gate rejects this genuine native shape; the new gate must accept it without inventing delivery proof.
func TestLaunchGrafanaAcceptsActualExistingMainReceiver(t *testing.T) {
	server := launchGrafanaRoutingServer(t, launchExistingMainContacts(), map[string]any{"receiver": servergrafana.DefaultAlertContactName, "routes": []any{launchCanonicalCompetitionRoute(servergrafana.DefaultAlertContactName)}})
	evidence, err := checkCompetitionGrafana(context.Background(), &launchPreflightConfig{GrafanaUrl: server.URL, GrafanaToken: "synthetic-monitoring-token"})
	if err != nil {
		t.Fatal(err)
	}
	if evidence["contact_point"] != servergrafana.DefaultAlertContactName || evidence["incident_owner"] != servergrafana.CompetitionIncidentContact {
		t.Fatalf("wrong transport/owner evidence: %#v", evidence)
	}
	if delivered, _ := evidence["notification_delivery_proven"].(bool); delivered {
		t.Fatal("GET-only configuration check invented notification delivery")
	}
}

// Historical email names satisfy the old Contains gate even though the transport is not configured; they must no longer qualify.
func TestLaunchGrafanaRejectsDeadLegacyEmailReceiver(t *testing.T) {
	contacts := []any{map[string]any{"uid": servergrafana.CompetitionAlertContactUid, "name": servergrafana.CompetitionAlertContactName, "type": "email", "settings": map[string]any{"addresses": servergrafana.CompetitionIncidentContact}}}
	server := launchGrafanaRoutingServer(t, contacts, map[string]any{"receiver": servergrafana.CompetitionAlertContactName, "routes": []any{launchCanonicalCompetitionRoute(servergrafana.CompetitionAlertContactName)}})
	if _, err := checkCompetitionGrafana(context.Background(), &launchPreflightConfig{GrafanaUrl: server.URL, GrafanaToken: "synthetic-monitoring-token"}); err == nil {
		t.Fatal("legacy email-only routing was accepted")
	}
}

// A correctly named default receiver elsewhere cannot authenticate a missing, wrong, duplicate or manually scoped competition route.
func TestLaunchGrafanaRejectsNoncanonicalCompetitionRouting(t *testing.T) {
	contacts := append(launchExistingMainContacts(), map[string]any{"uid": servergrafana.CompetitionAlertContactUid, "name": servergrafana.CompetitionAlertContactName, "type": "email", "settings": map[string]any{"addresses": servergrafana.CompetitionIncidentContact}})
	manual := launchCanonicalCompetitionRoute(servergrafana.DefaultAlertContactName)
	manual["group_by"] = []any{"synthetic_team"}
	for _, c := range []struct {
		Name   string
		Routes []any
	}{
		{Name: "wrong-receiver", Routes: []any{launchCanonicalCompetitionRoute(servergrafana.CompetitionAlertContactName)}},
		{Name: "default-only-with-email-text", Routes: []any{map[string]any{"receiver": servergrafana.CompetitionAlertContactName, "object_matchers": []any{[]any{"synthetic", "=", "severity"}}}}},
		{Name: "manual-only", Routes: []any{manual, map[string]any{"receiver": servergrafana.CompetitionAlertContactName, "object_matchers": []any{[]any{"synthetic", "=", "severity"}}}}},
		{Name: "duplicate", Routes: []any{launchCanonicalCompetitionRoute(servergrafana.DefaultAlertContactName), launchCanonicalCompetitionRoute(servergrafana.DefaultAlertContactName), launchCanonicalCompetitionRoute(servergrafana.CompetitionAlertContactName)}},
	} {
		server := launchGrafanaRoutingServer(t, contacts, map[string]any{"receiver": servergrafana.DefaultAlertContactName, "routes": c.Routes})
		_, err := checkCompetitionGrafana(context.Background(), &launchPreflightConfig{GrafanaUrl: server.URL, GrafanaToken: "synthetic-monitoring-token"})
		server.Close()
		if err == nil {
			t.Errorf("%s routing was accepted", c.Name)
		}
	}
}
