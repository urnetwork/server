package monitor

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	server "github.com/urnetwork/server"
)

// These controls pass actual YAML through the credential inventory and reducer.
// Other resources cannot escape the synthetic roots used by this boundary.
func isolateSignInCredentialResources(t *testing.T) {
	t.Helper()
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())
}

// Browser exchange fields are string scalars even when another credential in
// the same document is legitimately a sequence, number, or map.
func TestGoogleSignInCredentialsRejectNonstringBrowserValues(t *testing.T) {
	isolateSignInCredentialResources(t)
	for _, field := range []string{"client_id", "client_secret"} {
		for _, test := range []struct {
			name  string
			value string
		}{
			{name: "boolean", value: "false"},
			{name: "number", value: "123"},
			{name: "mapping", value: "{nested: synthetic-private-marker}"},
			{name: "string-sequence", value: "[synthetic-private-marker]"},
			{name: "empty-mapping", value: "{}"},
			{name: "empty-sequence", value: "[]"},
			{name: "null", value: "null"},
			{name: "blank", value: "'  '"},
		} {
			clientId, clientSecret := "synthetic-browser-id", "synthetic-browser-secret"
			if field == "client_id" {
				clientId = test.value
			} else {
				clientSecret = test.value
			}
			pop := server.Vault.PushSimpleResource("google.yml", []byte(fmt.Sprintf(
				"client_id: synthetic-audience\nsign_in_oauth:\n  client_id: %s\n  client_secret: %s\n",
				clientId, clientSecret,
			)))
			requirement := credentialRequirementByKey(t, loadCredentialRequirements("main", false, []string{"api"}), "google-sign-in")
			pop()
			wantMissing := []string{"sign_in_oauth." + field}
			if !requirement.Present || requirement.Malformed || !requirement.Required || !reflect.DeepEqual(requirement.MissingFields, wantMissing) {
				t.Fatalf("browser %s/%s false negative: missing fields = %v, want %v (present=%t malformed=%t required=%t)",
					field, test.name, requirement.MissingFields, wantMissing, requirement.Present, requirement.Malformed, requirement.Required)
			}
			settings := syntheticSettings(&syntheticSource{})
			settings.Credentials = []CredentialRequirement{requirement}
			alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			alert := requireAlertClass(t, alerts, "credential-fields-missing")
			if len(alerts) != 1 || alert.Severity != SeverityPage || alert.Target != "google-sign-in" || alert.Frame != "resource=google.yml" {
				t.Fatalf("browser %s/%s did not retain the sign-in readiness page", field, test.name)
			}
			for _, marker := range []string{"synthetic-private-marker", "synthetic-browser-id", "synthetic-browser-secret", "synthetic-audience"} {
				requireAlertOmits(t, alert, marker)
				if strings.Contains(fmt.Sprintf("%+v", requirement), marker) {
					t.Fatal("credential value escaped into readiness metadata")
				}
			}
			if !strings.Contains(alert.Evidence, "not proof of a running consumer's mounted generation or a failed live request") {
				t.Fatal("readiness evidence omitted the runtime authority boundary")
			}
		}
	}
}

// Both sign-in providers share the same audience loader. A nonempty sequence
// with no usable strings is not an enabled audience check.
func TestSignInCredentialsRejectUnusableAudienceValues(t *testing.T) {
	isolateSignInCredentialResources(t)
	for _, provider := range []string{"apple", "google"} {
		for _, test := range []struct {
			name  string
			value string
		}{
			{name: "boolean", value: "true"},
			{name: "number", value: "123"},
			{name: "mapping", value: "{nested: synthetic-audience}"},
			{name: "blank-sequence", value: "['', '  ']"},
			{name: "nonstring-sequence", value: "[false, 123, null, {nested: synthetic-audience}]"},
			{name: "nested-blank-sequence", value: "[[null, '  '], []]"},
		} {
			pop := server.Vault.PushSimpleResource(provider+".yml", []byte(
				"client_id: "+test.value+"\nsign_in_oauth:\n  client_id: synthetic-browser-id\n  client_secret: synthetic-browser-secret\n",
			))
			resource, err := server.Vault.SimpleResource(provider + ".yml")
			if err != nil {
				t.Fatal(err)
			}
			// This is the exported selection used by session.ssoAllowedClientIds;
			// its post-selection trim must find no audience in these controls.
			for _, value := range resource.String("client_id") {
				if strings.TrimSpace(value) != "" {
					t.Fatalf("%s/%s unexpectedly supplied an audience to the consumer", provider, test.name)
				}
			}
			requirement := credentialRequirementByKey(t, loadCredentialRequirements("main", false, []string{"api"}), provider+"-sign-in")
			pop()
			if !reflect.DeepEqual(requirement.MissingFields, []string{"client_id"}) {
				t.Fatalf("%s/%s audience false negative: missing fields = %v, want [client_id]", provider, test.name, requirement.MissingFields)
			}
		}
	}
}

// Scalar, mixed, and nested string sequences remain usable. The monitor must
// follow the reader's selection rather than rejecting ignored nonstring entries.
func TestSignInCredentialsAcceptConsumerUsableShapes(t *testing.T) {
	isolateSignInCredentialResources(t)
	for _, provider := range []string{"apple", "google"} {
		for _, audience := range []string{
			"' synthetic-audience '",
			"[synthetic-audience]",
			"[null, false, '', '  ', synthetic-audience, {ignored: synthetic-value}]",
			"[[null, [' synthetic-audience ']], []]",
		} {
			pop := server.Vault.PushSimpleResource(provider+".yml", []byte(
				"client_id: "+audience+"\nsign_in_oauth:\n  client_id: ' synthetic-browser-id '\n  client_secret: ' synthetic-browser-secret '\n",
			))
			resource, err := server.Vault.SimpleResource(provider + ".yml")
			if err != nil {
				t.Fatal(err)
			}
			usable := false
			for _, value := range resource.String("client_id") {
				usable = usable || strings.TrimSpace(value) != ""
			}
			if !usable {
				t.Fatal("healthy shape supplied no audience to the consumer")
			}
			requirement := credentialRequirementByKey(t, loadCredentialRequirements("main", false, []string{"api"}), provider+"-sign-in")
			pop()
			if !requirement.Present || requirement.Malformed || len(requirement.MissingFields) != 0 {
				t.Fatalf("%s usable consumer shape was reported incomplete: %+v", provider, requirement)
			}
			settings := syntheticSettings(&syntheticSource{})
			settings.Credentials = []CredentialRequirement{requirement}
			alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
			if err != nil || len(alerts) != 0 {
				t.Fatalf("%s healthy shape alerts = %d, err = %v", provider, len(alerts), err)
			}
		}
	}
}

// Preserve the original missing-field finding and its API activation boundary;
// malformed or absent resources must not turn into healthy shape observations.
func TestGoogleSignInCredentialsPreserveMissingAndInactiveStates(t *testing.T) {
	isolateSignInCredentialResources(t)
	wantMissing := []string{"sign_in_oauth.client_id", "sign_in_oauth.client_secret"}
	pop := server.Vault.PushSimpleResource("google.yml", []byte("client_id: [synthetic-audience]\n"))
	requirement := credentialRequirementByKey(t, loadCredentialRequirements("main", false, []string{"api"}), "google-sign-in")
	if !requirement.Required || !requirement.Present || requirement.Malformed || !reflect.DeepEqual(requirement.MissingFields, wantMissing) {
		t.Fatalf("original browser missing-field observation changed: %+v", requirement)
	}
	for _, requirement := range loadCredentialRequirements("main", false, []string{"taskworker"}) {
		if requirement.Key == "google-sign-in" || requirement.Key == "apple-sign-in" {
			t.Fatal("inactive API retained a sign-in requirement")
		}
	}
	pop()

	pop = server.Vault.PushSimpleResource("google.yml", []byte("client_id: [\n"))
	requirement = credentialRequirementByKey(t, loadCredentialRequirements("main", false, []string{"api"}), "google-sign-in")
	pop()
	if !requirement.Malformed {
		t.Fatal("malformed Google resource lost its unknown shape")
	}
	requirement = credentialRequirementByKey(t, loadCredentialRequirements("main", false, []string{"api"}), "google-sign-in")
	if requirement.Present || !requirement.Required {
		t.Fatal("absent required Google resource became healthy")
	}
}

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
