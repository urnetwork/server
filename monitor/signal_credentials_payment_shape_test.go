package monitor

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	server "github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// Each field below is read as a nonblank string by the actual inbound verifier.
type paymentWebhookScalarControl struct {
	key        string
	resource   string
	field      string
	verifiable func() error
}

func paymentWebhookScalarControls() []paymentWebhookScalarControl {
	return []paymentWebhookScalarControl{
		{key: "google-payment", resource: "google.yml", field: "webhook.publisher_email", verifiable: controller.PlayWebhookVerifiable},
		{key: "google-payment", resource: "google.yml", field: "webhook.audience", verifiable: controller.PlayWebhookVerifiable},
		{key: "stripe-payment", resource: "stripe.yml", field: "webhook.signing_secret", verifiable: controller.StripeWebhookVerifiable},
		{key: "coinbase-payment", resource: "coinbase.yml", field: "webhook.shared_secret", verifiable: controller.CoinbaseWebhookVerifiable},
		{key: "solana-payment", resource: "helius.yml", field: "helius.webhook_auth_header", verifiable: controller.HeliusWebhookVerifiable},
	}
}

// Build all other requirements from the inventory, then vary one fixed verifier field.
func paymentWebhookScalarDocument(t *testing.T, control paymentWebhookScalarControl, value any, omit bool) []byte {
	t.Helper()
	pop := server.Vault.PushSimpleResource(control.resource, []byte("{}\n"))
	fields := credentialRequirementByKey(t, loadCredentialRequirements("main", false, nil), control.key).MissingFields
	pop()
	if len(fields) == 0 {
		t.Fatal("empty resource has no required fields")
	}
	found := false
	document := map[string]any{}
	for _, field := range fields {
		node := document
		path := strings.Split(field, ".")
		for _, component := range path[:len(path)-1] {
			child, ok := node[component].(map[string]any)
			if !ok {
				child = map[string]any{}
				node[component] = child
			}
			node = child
		}
		if field == control.field {
			found = true
			if !omit {
				node[path[len(path)-1]] = value
			}
		} else {
			node[path[len(path)-1]] = "synthetic-other-private-value"
		}
	}
	if !found {
		t.Fatalf("fixed verifier field %s is absent from inventory", control.field)
	}
	content, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// Rejected nonstring values used to look complete despite actual verifier refusal.
func TestPaymentWebhookCredentialRejectsNonstringScalars(t *testing.T) {
	isolatePaymentWebhookCredentialResources(t)
	for _, control := range paymentWebhookScalarControls() {
		for _, shape := range []struct {
			name  string
			value any
			omit  bool
		}{
			{name: "missing", omit: true},
			{name: "null"},
			{name: "blank", value: ""},
			{name: "whitespace", value: " \t "},
			{name: "number", value: 12345},
			{name: "false", value: false},
			{name: "true", value: true},
			{name: "empty-map", value: map[string]any{}},
			{name: "map", value: map[string]any{"nested": "synthetic-rejected-private-marker"}},
			{name: "empty-sequence", value: []any{}},
			{name: "sequence", value: []any{"synthetic-rejected-private-marker"}},
		} {
			t.Run(control.key+"/"+control.field+"/"+shape.name, func(t *testing.T) {
				pop := server.Vault.PushSimpleResource(control.resource, paymentWebhookScalarDocument(t, control, shape.value, shape.omit))
				defer pop()
				if control.verifiable() == nil {
					t.Fatal("actual verifier accepted a rejected scalar shape")
				}
				requirement := credentialRequirementByKey(t, loadCredentialRequirements("main", false, nil), control.key)
				if !requirement.Present || requirement.Malformed || !requirement.Required || !reflect.DeepEqual(requirement.MissingFields, []string{control.field}) {
					t.Fatalf("inventory/verifier scalar authority differs: %+v", requirement)
				}
				settings := syntheticSettings(&syntheticSource{})
				settings.Credentials = []CredentialRequirement{requirement}
				alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
				if err != nil {
					t.Fatal(err)
				}
				alert := requireAlertClass(t, alerts, "credential-fields-missing")
				if len(alerts) != 1 || alert.Severity != SeverityPage || alert.Target != control.key || !strings.Contains(alert.Observed, "missing_fields="+control.field) {
					t.Fatalf("required rejected scalar did not remain visible: %+v", alerts)
				}
				requireAlertOmits(t, alert, "synthetic-other-private-value", "synthetic-rejected-private-marker", "12345")
			})
		}
	}
}

// Verifier acceptance proves local scalar readiness, not valid provider credentials.
func TestPaymentWebhookCredentialAcceptsNonblankStrings(t *testing.T) {
	isolatePaymentWebhookCredentialResources(t)
	for _, control := range paymentWebhookScalarControls() {
		t.Run(control.key+"/"+control.field, func(t *testing.T) {
			pop := server.Vault.PushSimpleResource(control.resource, paymentWebhookScalarDocument(t, control, " synthetic-accepted-private-marker ", false))
			defer pop()
			if err := control.verifiable(); err != nil {
				t.Fatalf("actual verifier refused nonblank string: %v", err)
			}
			requirement := credentialRequirementByKey(t, loadCredentialRequirements("main", false, nil), control.key)
			if !requirement.Present || requirement.Malformed || len(requirement.MissingFields) != 0 {
				t.Fatalf("valid string remained incomplete: %+v", requirement)
			}
			settings := syntheticSettings(&syntheticSource{})
			settings.Credentials = []CredentialRequirement{requirement}
			alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
			if err != nil || len(alerts) != 0 {
				t.Fatalf("healthy scalar readiness: alerts=%+v err=%v", alerts, err)
			}
		})
	}
}

// Optional absence remains off; a present invalid scalar remains an observable warning.
func TestPaymentWebhookCredentialOptionalScalarAuthority(t *testing.T) {
	isolatePaymentWebhookCredentialResources(t)
	for _, control := range paymentWebhookScalarControls() {
		t.Run(control.key+"/"+control.field, func(t *testing.T) {
			absent := credentialRequirementByKey(t, loadCredentialRequirements("local", false, nil), control.key)
			if absent.Required || absent.Present {
				t.Fatal("synthetic optional resource was not absent")
			}
			settings := syntheticSettings(&syntheticSource{})
			settings.Credentials = []CredentialRequirement{absent}
			alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
			if err != nil || len(alerts) != 0 {
				t.Fatalf("absent optional resource became an alert: %+v %v", alerts, err)
			}
			pop := server.Vault.PushSimpleResource(control.resource, paymentWebhookScalarDocument(t, control, false, false))
			defer pop()
			requirement := credentialRequirementByKey(t, loadCredentialRequirements("local", false, nil), control.key)
			if requirement.Required || !reflect.DeepEqual(requirement.MissingFields, []string{control.field}) || control.verifiable() == nil {
				t.Fatalf("optional present scalar lost verifier authority: %+v", requirement)
			}
			settings.Credentials = []CredentialRequirement{requirement}
			alerts, err = NewCredentialsSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			alert := requireAlertClass(t, alerts, "credential-fields-missing")
			if len(alerts) != 1 || alert.Severity != SeverityWarn {
				t.Fatalf("optional present invalid scalar did not warn: %+v", alerts)
			}
		})
	}
}
