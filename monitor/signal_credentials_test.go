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
