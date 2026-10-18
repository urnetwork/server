// Sign-in readiness follows each runtime consumer's usable value shapes;
// unrelated credential types retain their existing presence rules.
package monitor

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Real requirement assembly with synthetic resources only; the resulting
// metadata is passed through the ordinary credentials signal.
func signInSchemaFixture(t *testing.T) {
	t.Helper()
	t.Setenv("WARP_ENV", "main")
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())
}

// Returns only the selected sign-in requirement and its alert, keeping the
// production loader's other credential requirements intact.
func signInSchemaReadiness(t *testing.T, key string) (CredentialRequirement, Alerts) {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{})
	settings.Credentials = loadCredentialRequirements("main", false, []string{"api"})
	requirement := credentialRequirementByKey(t, settings.Credentials, key)
	alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	selected := Alerts{}
	for _, alert := range alerts {
		if alert.Target == key {
			selected = append(selected, alert)
		}
	}
	return requirement, selected
}

// The browser callback accepts only trimmed scalar strings. Presence alone
// formerly accepted numbers, booleans, maps and collections it rejects.
func TestGoogleSignInSchemaRejectsNonstringBrowserCredentials(t *testing.T) {
	signInSchemaFixture(t)
	for _, value := range []string{
		"42", "true", "false", "{}", "[]", "[synthetic-private-browser]",
		"{key: synthetic-private-browser}", "null", `"   "`,
	} {
		pop := server.Vault.PushSimpleResource("google.yml", []byte("client_id: synthetic-audience\nsign_in_oauth:\n  client_id: "+value+"\n  client_secret: "+value+"\n"))
		requirement, alerts := signInSchemaReadiness(t, "google-sign-in")
		pop()
		want := []string{"sign_in_oauth.client_id", "sign_in_oauth.client_secret"}
		if !reflect.DeepEqual(requirement.MissingFields, want) {
			t.Errorf("value shape %s: missing fields=%v, want %v", value, requirement.MissingFields, want)
		}
		if len(alerts) != 1 || alerts[0].Class != "credential-fields-missing" || alerts[0].Severity != SeverityPage {
			t.Errorf("value shape %s: unusable browser client did not PAGE: %+v", value, alerts)
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "synthetic-private-browser", "synthetic-audience")
		}
	}
}

// Both identity-token consumers discard every nonstring or blank audience,
// including values buried in nested collections.
func TestSignInSchemaRejectsAudienceWithoutUsableString(t *testing.T) {
	signInSchemaFixture(t)
	for _, value := range []string{
		"42", "true", "false", "{}", "[]", "[42, true, null]",
		`[" ", ["", false], {key: synthetic-private-audience}]`, "null", `"   "`,
	} {
		for _, provider := range []string{"google", "apple"} {
			pop := server.Vault.PushSimpleResource(provider+".yml", []byte("client_id: "+value+"\nsign_in_oauth:\n  client_id: synthetic-browser\n  client_secret: synthetic-private-browser\n"))
			requirement, alerts := signInSchemaReadiness(t, provider+"-sign-in")
			pop()
			if !reflect.DeepEqual(requirement.MissingFields, []string{"client_id"}) {
				t.Errorf("%s audience shape %s: missing fields=%v", provider, value, requirement.MissingFields)
			}
			if len(alerts) != 1 || alerts[0].Class != "credential-fields-missing" || alerts[0].Severity != SeverityPage {
				t.Errorf("%s audience shape %s did not PAGE: %+v", provider, value, alerts)
			}
			for _, alert := range alerts {
				requireAlertOmits(t, alert, "synthetic-private-audience", "synthetic-private-browser")
			}
		}
	}
}

// Scalar, list and recursively nested list strings are actual supported
// audience forms. Browser strings are trimmed by their separate consumer.
func TestSignInSchemaAcceptsSupportedUsableStrings(t *testing.T) {
	signInSchemaFixture(t)
	for _, value := range []string{
		`" synthetic-audience "`,
		`[" ", "synthetic-audience"]`,
		`[false, [42, ["synthetic-audience", ""]]]`,
	} {
		for _, provider := range []string{"google", "apple"} {
			pop := server.Vault.PushSimpleResource(provider+".yml", []byte("client_id: "+value+"\nsign_in_oauth:\n  client_id: ' synthetic-browser '\n  client_secret: ' synthetic-private-browser '\n"))
			requirement, alerts := signInSchemaReadiness(t, provider+"-sign-in")
			pop()
			if len(requirement.MissingFields) != 0 || len(alerts) != 0 {
				t.Errorf("%s supported shape %s rejected: requirement=%+v alerts=%+v", provider, value, requirement, alerts)
			}
			if strings.Contains(fmt.Sprintf("%+v", requirement), "synthetic-private-browser") {
				t.Fatal("browser secret escaped into readiness metadata")
			}
		}
	}
}

// Strict sign-in fields must not change generic structured or numeric
// credentials consumed by unrelated integrations.
func TestSignInSchemaPreservesGenericCredentialPresence(t *testing.T) {
	signInSchemaFixture(t)
	t.Cleanup(server.Vault.PushSimpleResource("synthetic-generic.yml", []byte("number: 42\ncollection: [synthetic]\nobject: {key: synthetic}\n")))
	requirement := inspectCredentialRequirement(credentialRequirementSpec{
		key: "synthetic-generic", resource: "synthetic-generic.yml", required: true,
		fields: []credentialFieldSpec{
			{name: "number", path: []string{"number"}},
			{name: "collection", path: []string{"collection"}},
			{name: "object", path: []string{"object"}},
		},
	})
	if !requirement.Present || requirement.Malformed || len(requirement.MissingFields) != 0 {
		t.Fatalf("generic credential semantics changed: %+v", requirement)
	}
}
