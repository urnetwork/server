// Competition routing uses the existing native main receiver, preserves operator-owned routes and never provisions a new email transport.
package grafana

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Existing contacts include the legacy unused integration so the regression isolates routing rather than contact creation.
func existingCompetitionReceiverContacts() []grafanaContactPoint {
	return append(teamCoreContacts(),
		grafanaContactPoint{Uid: DefaultAlertContactUid, Name: DefaultAlertContactName, Type: "slack", Settings: map[string]any{"url": syntheticSlackWebhookUrl}},
		grafanaContactPoint{Uid: CompetitionAlertContactUid, Name: CompetitionAlertContactName, Type: "email", Settings: map[string]any{"addresses": CompetitionIncidentContact}},
	)
}

// This exact two-matcher signature is managed; additional matchers, routing settings or child routes remain operator-owned.
func competitionCanonicalRoute(receiver string) map[string]any {
	return map[string]any{"receiver": receiver, "object_matchers": []any{[]any{"service", "=", "sim-latency"}, []any{"severity", "=~", "warn|page"}}}
}

// The former email route deterministically fails this requirement even when an email contact point exists.
func TestCompetitionUsesExistingMainReceiverWithoutEmailWrites(t *testing.T) {
	contacts := existingCompetitionReceiverContacts()
	beforeContacts, err := json.Marshal(contacts)
	if err != nil {
		t.Fatal(err)
	}
	policy := map[string]any{"receiver": DefaultAlertContactName, "routes": []any{competitionCanonicalRoute(CompetitionAlertContactName)}}
	fake, server := newFakeAlertingGrafana(t, contacts, policy)
	for index := 0; index < 2; index++ {
		if _, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{}); err != nil {
			t.Fatal(err)
		}
		_, stored, _ := fake.snapshot()
		routes, ok := stored["routes"].([]any)
		if !ok || len(routes) != 1 {
			t.Fatalf("pass%d routes=%#v", index, stored["routes"])
		}
		route, ok := routes[0].(map[string]any)
		if !ok || route["receiver"] != DefaultAlertContactName {
			t.Fatalf("pass%d still routes competition outside the existing main receiver: %#v", index, route)
		}
	}
	afterContacts, _, _ := fake.snapshot()
	afterBytes, err := json.Marshal(afterContacts)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterBytes) != string(beforeContacts) {
		t.Fatal("existing contact integrations were changed")
	}
	for _, write := range fake.recordedWrites() {
		if write.path != "/api/v1/provisioning/policies" || write.method != http.MethodPut {
			t.Fatalf("unexpected non-policy mutation: %+v", write)
		}
	}
}

// These potentially consuming manual routes must remain byte-identical; reconciliation refuses before any write rather than silently reordering them.
func TestCompetitionPreservesManualRoutesAndCanonicalIdempotence(t *testing.T) {
	manuals := []any{}
	for _, extension := range []map[string]any{
		{"group_by": []any{"synthetic_team"}},
		{"routes": []any{map[string]any{"receiver": "team-core"}}},
		{"continue": true},
		{"group_wait": "5s"},
		{"group_interval": "10m"},
		{"mute_time_intervals": []any{"synthetic-weekend"}},
	} {
		route := competitionCanonicalRoute(CompetitionAlertContactName)
		for key, value := range extension {
			route[key] = value
		}
		manuals = append(manuals, route)
	}
	extraMatcher := competitionCanonicalRoute(CompetitionAlertContactName)
	extraMatcher["object_matchers"] = append(extraMatcher["object_matchers"].([]any), []any{"team", "=", "synthetic-team"})
	manuals = append(manuals, extraMatcher, competitionCanonicalRoute("team-core"), map[string]any{"receiver": CompetitionAlertContactName, "object_matchers": []any{[]any{"service", "=", "synthetic-sibling"}}})
	before, err := json.Marshal(manuals)
	if err != nil {
		t.Fatal(err)
	}
	routes := append(append([]any{}, manuals...), competitionCanonicalRoute(CompetitionAlertContactName))
	policy := map[string]any{"receiver": DefaultAlertContactName, "group_by": []any{"alertname"}, "routes": routes}
	beforePolicy, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	fake, server := newFakeAlertingGrafana(t, existingCompetitionReceiverContacts(), policy)
	for index := 0; index < 2; index++ {
		if _, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{}); err == nil {
			t.Fatal("shadowing manual routes reported readiness")
		}
		_, stored, _ := fake.snapshot()
		afterRoutes, ok := stored["routes"].([]any)
		if !ok || len(afterRoutes) != len(manuals)+1 {
			t.Fatalf("pass%d lost manual routes: %#v", index, stored["routes"])
		}
		after, err := json.Marshal(afterRoutes[:len(manuals)])
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatalf("pass%d changed manual routes", index)
		}
		managed, ok := afterRoutes[len(manuals)].(map[string]any)
		if !ok || managed["receiver"] != CompetitionAlertContactName {
			t.Fatalf("pass%d changed refused policy", index)
		}
		afterPolicy, err := json.Marshal(stored)
		if err != nil {
			t.Fatal(err)
		}
		if string(afterPolicy) != string(beforePolicy) || len(fake.recordedWrites()) != 0 {
			t.Fatalf("pass%d mutated refused operator state", index)
		}
	}
}

// A receiver with no native integration must refuse before contact creation or policy mutation, rather than reporting delivery readiness.
func TestCompetitionUnconfiguredMainReceiverRefusesBeforeMutation(t *testing.T) {
	fake, server := newFakeAlertingGrafana(t, teamCoreContacts(), unroutedToNobodyPolicy())
	if _, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{}); err == nil {
		t.Fatal("unconfigured existing main receiver was accepted")
	}
	if writes := fake.recordedWrites(); len(writes) != 0 {
		t.Fatalf("unconfigured receiver mutated Grafana: %+v", writes)
	}
}

// Only the canonical legacy/new signatures are owned; a shared service or contact name does not imply controller ownership.
func TestCompetitionManagedRouteRequiresExactSignature(t *testing.T) {
	for _, receiver := range []string{CompetitionAlertContactName, DefaultAlertContactName} {
		if !managedCompetitionRoute(competitionCanonicalRoute(receiver)) {
			t.Fatal("canonical managed signature was not recognized")
		}
	}
	for _, c := range []struct {
		Name  string
		Route map[string]any
	}{
		{Name: "custom-receiver", Route: competitionCanonicalRoute("synthetic-manual-receiver")},
		{Name: "unrelated-email-use", Route: map[string]any{"receiver": CompetitionAlertContactName, "object_matchers": []any{[]any{"service", "=", "synthetic-other"}}}},
		{Name: "only-service", Route: map[string]any{"receiver": DefaultAlertContactName, "object_matchers": []any{[]any{"service", "=", "sim-latency"}}}},
		{Name: "extra-setting", Route: map[string]any{"receiver": DefaultAlertContactName, "object_matchers": []any{[]any{"service", "=", "sim-latency"}, []any{"severity", "=~", "warn|page"}}, "continue": false}},
		{Name: "wrong-severity", Route: map[string]any{"receiver": DefaultAlertContactName, "object_matchers": []any{[]any{"service", "=", "sim-latency"}, []any{"severity", "=", "page"}}}},
	} {
		if managedCompetitionRoute(c.Route) {
			t.Errorf("%s was incorrectly classified as controller-owned", c.Name)
		}
	}
}

// Grafana's stored secure-field marker suffices; fixing routing must not require reading or rewriting the stored webhook.
func TestCompetitionStoredMainWebhookNeedsNoCredentialRewrite(t *testing.T) {
	var policyPuts atomic.Int64
	var contactWrites atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/provisioning/contact-points":
			_ = json.NewEncoder(response).Encode([]any{map[string]any{"uid": DefaultAlertContactUid, "name": DefaultAlertContactName, "type": "slack", "settings": map[string]any{}, "secureFields": map[string]bool{"url": true}}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/provisioning/policies":
			_ = json.NewEncoder(response).Encode(map[string]any{"receiver": DefaultAlertContactName})
		case request.Method == http.MethodPut && request.URL.Path == "/api/v1/provisioning/policies":
			policyPuts.Add(1)
			response.WriteHeader(http.StatusAccepted)
		default:
			contactWrites.Add(1)
			response.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	if _, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{}); err != nil {
		t.Fatal(err)
	}
	if policyPuts.Load() != 1 || contactWrites.Load() != 0 {
		t.Fatalf("policy writes=%d unexpected requests=%d", policyPuts.Load(), contactWrites.Load())
	}
}

// Empty or wrongly owned managed integrations cannot be mistaken for a configured main notification path.
func TestCompetitionMalformedMainIntegrationRefusesBeforeMutation(t *testing.T) {
	for _, c := range []struct {
		Name     string
		Uid      string
		Type     string
		Settings map[string]any
	}{
		{Name: "wrong-uid", Uid: "synthetic-unmanaged", Type: "slack", Settings: map[string]any{"url": syntheticSlackWebhookUrl}},
		{Name: "wrong-type", Uid: DefaultAlertContactUid, Type: "email", Settings: map[string]any{"addresses": "operator@notify.example"}},
		{Name: "missing-url", Uid: DefaultAlertContactUid, Type: "slack", Settings: map[string]any{}},
	} {
		var mutations atomic.Int64
		var gets atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			if request.Method != http.MethodGet {
				mutations.Add(1)
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			gets.Add(1)
			_ = json.NewEncoder(response).Encode([]any{map[string]any{"uid": c.Uid, "name": DefaultAlertContactName, "type": c.Type, "settings": c.Settings}})
		}))
		_, err := ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", AlertRoutingSettings{})
		server.Close()
		if err == nil || mutations.Load() != 0 || gets.Load() != 1 {
			t.Errorf("%s accepted=%v mutations=%d reads=%d", c.Name, err == nil, mutations.Load(), gets.Load())
		}
	}
}
