// Effective competition delivery must not be shadowed by earlier manual routes; failures preserve all operator-owned state rather than reordering it.
package grafana

import (
	"context"
	"encoding/json"
	"testing"
)

// This fixture uses the same native JSON boundary as read-only launch preflight.
func competitionRoutingProof(t *testing.T, prior map[string]any) error {
	t.Helper()
	contacts, err := json.Marshal(existingCompetitionReceiverContacts())
	if err != nil {
		t.Fatal(err)
	}
	policy, err := json.Marshal(map[string]any{"receiver": DefaultAlertContactName, "routes": []any{prior, competitionCanonicalRoute(DefaultAlertContactName)}})
	if err != nil {
		t.Fatal(err)
	}
	return ValidateCompetitionAlertRouting(contacts, policy)
}

// The previous existence-only validator accepts this unreachable canonical route, reproducing the false-readiness defect.
func TestCompetitionRoutingRejectsShadowingManualBeforeCanonical(t *testing.T) {
	manual := competitionCanonicalRoute(CompetitionAlertContactName)
	manual["group_wait"] = "5s"
	if err := competitionRoutingProof(t, manual); err == nil {
		t.Fatal("earlier consuming email route qualified unreachable canonical main routing")
	}
}

// Validation precedes every policy and contact write, including the normal native webhook upsert path.
func TestCompetitionReconcileRefusesShadowingBeforeAnyMutation(t *testing.T) {
	for _, withWebhook := range []bool{false, true} {
		contacts := existingCompetitionReceiverContacts()
		settings := AlertRoutingSettings{}
		if withWebhook {
			withoutMain := []grafanaContactPoint{}
			for _, contact := range contacts {
				if contact.Uid != DefaultAlertContactUid {
					withoutMain = append(withoutMain, contact)
				}
			}
			contacts = withoutMain
			settings.SlackWebhookUrl = syntheticSlackWebhookUrl
		}
		manual := competitionCanonicalRoute(CompetitionAlertContactName)
		manual["group_by"] = []any{"synthetic-team"}
		policy := map[string]any{"receiver": "empty", "routes": []any{manual, competitionCanonicalRoute(CompetitionAlertContactName)}}
		beforeContacts, err := json.Marshal(contacts)
		if err != nil {
			t.Fatal(err)
		}
		beforePolicy, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		fake, server := newFakeAlertingGrafana(t, contacts, policy)
		_, err = ReconcileAlertRouting(context.Background(), server.URL, "admin", "secret", settings)
		if err == nil {
			t.Errorf("webhook=%v: shadowed policy reported success", withWebhook)
		}
		storedContacts, storedPolicy, _ := fake.snapshot()
		afterContacts, marshalErr := json.Marshal(storedContacts)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		afterPolicy, marshalErr := json.Marshal(storedPolicy)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if len(fake.recordedWrites()) != 0 || string(beforeContacts) != string(afterContacts) || string(beforePolicy) != string(afterPolicy) {
			t.Errorf("webhook=%v: refused routing changed operator state", withWebhook)
		}
	}
}

// Exact service/severity comparisons and fully anchored regexes can prove a prior route disjoint without inventing other label values.
func TestCompetitionRoutingAllowsProvablyDisjointPriorRoutes(t *testing.T) {
	for _, c := range []struct {
		Name     string
		Matchers []any
	}{
		{Name: "different-service", Matchers: []any{[]any{"service", "=", "synthetic-api"}}},
		{Name: "service-exclusion", Matchers: []any{[]any{"service", "!=", "sim-latency"}}},
		{Name: "service-regex", Matchers: []any{[]any{"service", "=~", "synthetic-api|synthetic-worker"}}},
		{Name: "anchored-service-regex", Matchers: []any{[]any{"service", "=~", "sim"}}},
		{Name: "severity-exact", Matchers: []any{[]any{"severity", "=", "info"}}},
		{Name: "severity-regex", Matchers: []any{[]any{"severity", "=~", "warning|pages"}}},
		{Name: "negative-severity-regex", Matchers: []any{[]any{"severity", "!~", "warn|page"}}},
		{Name: "unknown-label-and-disjoint-service", Matchers: []any{[]any{"synthetic-team", "=", "synthetic-core"}, []any{"service", "=", "synthetic-api"}}},
	} {
		prior := map[string]any{"receiver": "synthetic-manual", "object_matchers": c.Matchers}
		if err := competitionRoutingProof(t, prior); err != nil {
			t.Errorf("%s disjoint route refused: %v", c.Name, err)
		}
	}
}

// A genuinely boolean continue=true leaves the canonical sibling reachable even when the earlier route may match.
func TestCompetitionRoutingAllowsSafeContinue(t *testing.T) {
	manual := competitionCanonicalRoute(CompetitionAlertContactName)
	manual["continue"] = true
	if err := competitionRoutingProof(t, manual); err != nil {
		t.Fatal(err)
	}
}

// Catchalls, unknown labels, partial warn/page coverage and malformed matcher/continuation types cannot prove reachability.
func TestCompetitionRoutingRejectsAmbiguousOrMalformedPriorRoutes(t *testing.T) {
	for _, c := range []struct {
		Name  string
		Route map[string]any
	}{
		{Name: "catchall", Route: map[string]any{"receiver": "synthetic-manual"}},
		{Name: "nested-catchall", Route: map[string]any{"receiver": "synthetic-manual", "routes": []any{map[string]any{"receiver": DefaultAlertContactName}}}},
		{Name: "unknown-label", Route: map[string]any{"receiver": "synthetic-manual", "object_matchers": []any{[]any{"synthetic-team", "=", "synthetic-core"}}}},
		{Name: "warn-only-consumed", Route: map[string]any{"receiver": "synthetic-manual", "object_matchers": []any{[]any{"severity", "=", "warn"}}}},
		{Name: "page-only-consumed", Route: map[string]any{"receiver": "synthetic-manual", "object_matchers": []any{[]any{"severity", "=~", "page"}}}},
		{Name: "invalid-regex", Route: map[string]any{"receiver": "synthetic-manual", "object_matchers": []any{[]any{"service", "=~", "["}}}},
		{Name: "unknown-operator", Route: map[string]any{"receiver": "synthetic-manual", "object_matchers": []any{[]any{"service", "contains", "other"}}}},
		{Name: "malformed-tuple", Route: map[string]any{"receiver": "synthetic-manual", "object_matchers": []any{[]any{"service", "="}}}},
		{Name: "malformed-value", Route: map[string]any{"receiver": "synthetic-manual", "object_matchers": []any{[]any{"service", "=", 42}}}},
		{Name: "string-continue", Route: map[string]any{"receiver": "synthetic-manual", "continue": "true"}},
		{Name: "legacy-unknown-matchers", Route: map[string]any{"receiver": "synthetic-manual", "match_re": map[string]any{"service": "other"}}},
	} {
		if err := competitionRoutingProof(t, c.Route); err == nil {
			t.Errorf("%s ambiguous route accepted", c.Name)
		}
	}
}
