package monitor

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	server "github.com/urnetwork/server"
)

func TestCredentialsSignalPagesOnRequiredMissingFieldsWithoutValues(t *testing.T) {
	secretMarker := "synthetic-secret-must-not-render"
	settings := syntheticSettings(&syntheticSource{})
	settings.Credentials = []CredentialRequirement{
		{
			Key:           "synthetic-payment",
			Resource:      "synthetic-payment.yml",
			Purpose:       "Synthetic payment reconciliation",
			Required:      true,
			Present:       true,
			MissingFields: []string{"private_key", "issuer_id"},
		},
	}
	alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "credential-fields-missing")
	if alert.Severity != SeverityPage || alert.Target != "synthetic-payment" {
		t.Fatalf("unexpected alert identity: %+v", alert)
	}
	for _, expected := range []string{"issuer_id", "private_key", "resource=synthetic-payment.yml"} {
		if !strings.Contains(alert.Markdown(), expected) {
			t.Fatalf("alert omits %q:\n%s", expected, alert.Markdown())
		}
	}
	requireAlertOmits(t, alert, secretMarker)
}

func TestCredentialsSignalOptionalAbsentNoopsButPartialIsVisible(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.Credentials = []CredentialRequirement{
		{Key: "optional-absent", Resource: "optional-absent.yml", Required: false},
		{
			Key: "optional-partial", Resource: "optional-partial.yml", Purpose: "Synthetic optional reporting",
			Required: false, Present: true, MissingFields: []string{"private_key"},
		},
	}
	alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want only the partial optional resource: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "credential-fields-missing")
	if alert.Severity != SeverityWarn || alert.Target != "optional-partial" {
		t.Fatalf("unexpected optional alert: %+v", alert)
	}
}

func TestCredentialInspectionDiscardsValuesAndClassifiesMalformedResource(t *testing.T) {
	secretMarker := "synthetic-private-material-not-real"
	pop := server.Vault.PushSimpleResource("synthetic-credential.yml", []byte("identity: synthetic-id\nprivate_key: "+secretMarker+"\n"))
	requirement := inspectCredentialRequirement(credentialRequirementSpec{
		key: "synthetic", resource: "synthetic-credential.yml", purpose: "Synthetic test", required: true,
		fields: []credentialFieldSpec{
			{name: "identity", path: []string{"identity"}},
			{name: "private_key", path: []string{"private_key"}},
		},
	})
	pop()
	if !requirement.Present || requirement.Malformed || len(requirement.MissingFields) != 0 {
		t.Fatalf("unexpected complete requirement: %+v", requirement)
	}
	if strings.Contains(fmt.Sprintf("%+v", requirement), secretMarker) {
		t.Fatal("credential value escaped into readiness settings")
	}

	pop = server.Vault.PushSimpleResource("synthetic-credential.yml", []byte("private_key: [\n"))
	requirement = inspectCredentialRequirement(credentialRequirementSpec{
		key: "synthetic", resource: "synthetic-credential.yml", required: true,
		fields: []credentialFieldSpec{{name: "private_key", path: []string{"private_key"}}},
	})
	pop()
	if !requirement.Present || !requirement.Malformed {
		t.Fatalf("malformed requirement = %+v", requirement)
	}
	if strings.Contains(fmt.Sprintf("%+v", requirement), "private_key: [") {
		t.Fatal("malformed source contents escaped into readiness settings")
	}
}

func TestCredentialsSignalClassifiesMissingAndMalformedResources(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.Credentials = []CredentialRequirement{
		{Key: "required-absent", Resource: "required-absent.yml", Required: true},
		{Key: "required-malformed", Resource: "required-malformed.yml", Required: true, Present: true, Malformed: true},
	}
	alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "credential-resource-missing")
	requireAlertClass(t, alerts, "credential-resource-malformed")
}

func TestEnabledAnalyticsCredentialFieldsFollowProviderAndSiteGates(t *testing.T) {
	pop := server.Config.PushSimpleResource("analytics.yml", []byte(`
enabled: true
search:
  enabled: true
providers:
  google_search_console:
    enabled: true
    mode: api
  bing_webmaster:
    enabled: true
    mode: api
    protocol: rest
  yandex_webmaster:
    enabled: true
    mode: api
sites:
  - name: synthetic
    origin: synthetic-origin
    properties:
      google_search_console: synthetic-property
      yandex_host_id: synthetic-property
`))
	defer pop()

	fields := enabledAnalyticsCredentialFields()
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, field.name)
	}
	want := []string{
		"google_search_console.service_account_json",
		"yandex_webmaster.oauth_token",
		"yandex_webmaster.user_id",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("credential fields = %v, want %v", names, want)
	}
}

func TestEnabledAnalyticsCredentialFieldsNoopWithoutMatchingSites(t *testing.T) {
	pop := server.Config.PushSimpleResource("analytics.yml", []byte(`
enabled: true
search:
  enabled: true
providers:
  google_search_console:
    enabled: true
    mode: api
sites:
  - name: synthetic
    origin: synthetic-origin
`))
	defer pop()

	if fields := enabledAnalyticsCredentialFields(); len(fields) != 0 {
		t.Fatalf("credential fields = %+v, want none", fields)
	}
}

func TestCoinbaseCredentialRequirementsMatchRuntimeConsumers(t *testing.T) {
	t.Setenv("WARP_ENV", "main")
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())

	popLegacy := server.Vault.PushSimpleResource("coinbase.yml", []byte(`
api:
  account_id: retained-legacy-account
  key_name: retained-legacy-key
  private_key: retained-legacy-private-key
`))
	requirements := loadCredentialRequirements("main", false, nil)
	popLegacy()
	coinbase := credentialRequirementByKey(t, requirements, "coinbase-payment")
	wantMissing := []string{"api.host", "webhook.shared_secret"}
	if !reflect.DeepEqual(coinbase.MissingFields, wantMissing) {
		t.Fatalf("legacy-only Coinbase missing fields = %v, want %v", coinbase.MissingFields, wantMissing)
	}

	popRuntime := server.Vault.PushSimpleResource("coinbase.yml", []byte(`
api:
  host: exchange.synthetic.invalid
webhook:
  shared_secret: synthetic-webhook-secret
`))
	requirements = loadCredentialRequirements("main", false, nil)
	popRuntime()
	if missing := credentialRequirementByKey(t, requirements, "coinbase-payment").MissingFields; len(missing) != 0 {
		t.Fatalf("runtime-complete Coinbase missing fields = %v", missing)
	}
}

func TestMainSignInCredentialRequirementsFollowActiveAPI(t *testing.T) {
	t.Setenv("WARP_ENV", "main")
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())

	popApple := server.Vault.PushSimpleResource("apple.yml", []byte(`
app_store_notifications:
  bundle_id: app.synthetic.invalid
  app_apple_id: 123
  environments: [synthetic]
  product_ids: [synthetic-product]
app_store_server_api_key_id: synthetic-payment-key
issuer_id: synthetic-payment-issuer
private_key: synthetic-payment-private-key
`))
	defer popApple()
	popGoogle := server.Vault.PushSimpleResource("google.yml", []byte(`
webhook:
  publisher_email: publisher@example.invalid
  package_name: app.synthetic.invalid
oauth:
  client_id: synthetic-payment-client
  client_secret: synthetic-payment-secret
  refresh_token: synthetic-payment-refresh
`))
	defer popGoogle()

	requirements := loadCredentialRequirements("main", false, []string{"api"})
	apple := credentialRequirementByKey(t, requirements, "apple-sign-in")
	if !reflect.DeepEqual(apple.MissingFields, []string{"client_id"}) {
		t.Fatalf("Apple sign-in missing fields = %v", apple.MissingFields)
	}
	google := credentialRequirementByKey(t, requirements, "google-sign-in")
	wantGoogle := []string{"client_id", "sign_in_oauth.client_id", "sign_in_oauth.client_secret"}
	if !reflect.DeepEqual(google.MissingFields, wantGoogle) {
		t.Fatalf("Google sign-in missing fields = %v, want %v", google.MissingFields, wantGoogle)
	}

	popCompleteApple := server.Vault.PushSimpleResource("apple.yml", []byte(`
client_id: [synthetic-apple-client]
`))
	defer popCompleteApple()
	popCompleteGoogle := server.Vault.PushSimpleResource("google.yml", []byte(`
client_id: [synthetic-google-client]
sign_in_oauth:
  client_id: synthetic-browser-client
  client_secret: synthetic-browser-secret
`))
	defer popCompleteGoogle()
	requirements = loadCredentialRequirements("main", false, []string{"api"})
	if missing := credentialRequirementByKey(t, requirements, "apple-sign-in").MissingFields; len(missing) != 0 {
		t.Fatalf("complete Apple sign-in missing fields = %v", missing)
	}
	if missing := credentialRequirementByKey(t, requirements, "google-sign-in").MissingFields; len(missing) != 0 {
		t.Fatalf("complete Google sign-in missing fields = %v", missing)
	}

	requirements = loadCredentialRequirements("main", false, []string{"taskworker"})
	for _, key := range []string{"apple-sign-in", "google-sign-in"} {
		for _, requirement := range requirements {
			if requirement.Key == key {
				t.Fatalf("inactive API retained %s requirement", key)
			}
		}
	}
}

func credentialRequirementByKey(t *testing.T, requirements []CredentialRequirement, key string) CredentialRequirement {
	t.Helper()
	for _, requirement := range requirements {
		if requirement.Key == key {
			return requirement
		}
	}
	t.Fatalf("credential requirement %q not found", key)
	return CredentialRequirement{}
}
