package grafana

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompetitionAlertRoutingPreservesTheMainPolicyTree(t *testing.T) {
	contacts := []grafanaContactPoint{
		{Uid: "main-on-call", Name: "main-on-call", Type: "webhook", Settings: map[string]any{"url": "https://alerts.invalid/main"}},
	}
	policy := map[string]any{
		"receiver": "main-on-call",
		"group_by": []any{"grafana_folder", "alertname"},
		"routes": []any{
			map[string]any{
				"receiver":        "main-on-call",
				"object_matchers": []any{[]any{"team", "=", "core"}},
			},
		},
	}
	contactWrites := 0
	policyWrites := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "admin" || password != "secret" {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Method != http.MethodGet && request.Header.Get("X-Disable-Provenance") != "true" {
			t.Error("mutation did not preserve Grafana UI editability")
		}
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/provisioning/contact-points":
			if err := json.NewEncoder(response).Encode(contacts); err != nil {
				t.Error(err)
			}
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/provisioning/contact-points":
			contact := grafanaContactPoint{}
			if err := json.NewDecoder(request.Body).Decode(&contact); err != nil {
				t.Error(err)
			}
			contacts = append(contacts, contact)
			contactWrites++
			response.WriteHeader(http.StatusAccepted)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/provisioning/policies":
			if err := json.NewEncoder(response).Encode(policy); err != nil {
				t.Error(err)
			}
		case request.Method == http.MethodPut && request.URL.Path == "/api/v1/provisioning/policies":
			if err := json.NewDecoder(request.Body).Decode(&policy); err != nil {
				t.Error(err)
			}
			policyWrites++
			response.WriteHeader(http.StatusAccepted)
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	for i := 0; i < 2; i++ {
		if err := ReconcileCompetitionAlertRouting(context.Background(), server.URL, "admin", "secret"); err != nil {
			t.Fatal(err)
		}
	}
	if contactWrites != 1 || policyWrites != 2 {
		t.Fatalf("contact writes = %d, policy writes = %d", contactWrites, policyWrites)
	}
	if len(contacts) != 2 || contacts[1].Uid != CompetitionAlertContactUid ||
		contacts[1].Settings["addresses"] != CompetitionIncidentContact {
		t.Fatalf("contacts = %+v", contacts)
	}
	routes, ok := policy["routes"].([]any)
	if !ok || len(routes) != 2 {
		t.Fatalf("policy routes = %#v", policy["routes"])
	}
	unrelated, _ := routes[0].(map[string]any)
	managed, _ := routes[1].(map[string]any)
	if unrelated["receiver"] != "main-on-call" || managed["receiver"] != CompetitionAlertContactName ||
		policy["receiver"] != "main-on-call" {
		t.Fatalf("policy = %#v", policy)
	}
	encoded, err := json.Marshal(managed["object_matchers"])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `[["service","=","sim-latency"],["severity","=~","warn|page"]]` {
		t.Fatalf("managed matchers = %s", encoded)
	}
}

func TestCompetitionAlertRoutingRejectsOwnedNameCollision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`[{"uid":"someone-else","name":"sim-latency-support","type":"email","settings":{"addresses":"other@example.com"}}]`))
	}))
	defer server.Close()
	if err := ReconcileCompetitionAlertRouting(context.Background(), server.URL, "admin", "secret"); err == nil {
		t.Fatal("contact-point name collision was accepted")
	}
}

func TestCompetitionAlertRoutingRejectsInsecureRemoteEndpointsAndRedirects(t *testing.T) {
	if _, err := newGrafanaProvisioningClient("http://grafana.example.com", "admin", "secret"); err == nil || !strings.Contains(err.Error(), "requires HTTPS") {
		t.Fatalf("insecure Grafana endpoint error = %v", err)
	}

	redirectFollowed := false
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		redirectFollowed = true
		response.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL+"/credential-capture", http.StatusFound)
	}))
	defer origin.Close()
	if err := ReconcileCompetitionAlertRouting(context.Background(), origin.URL, "admin", "secret"); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("redirecting Grafana endpoint error = %v", err)
	}
	if redirectFollowed {
		t.Fatal("Grafana provisioning followed a credential-bearing redirect")
	}
}
