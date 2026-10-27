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

// These controls pass synthetic YAML through the credential inventory and the
// payment webhooks' own verifiability checks. Other resources cannot escape the
// synthetic roots.
func isolatePaymentWebhookCredentialResources(t *testing.T) {
	t.Helper()
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())
}

// Main's google.yml had no webhook.audience when the 2026-10-19 API roll made
// the Play webhook bind each push token to its audience. The API refused every
// Google Play notification with 503 (about 74,000 refusals in seven days) while
// the credentials signal stayed green: its google-payment requirement did not
// list the field the Play webhook verifier requires.
func TestGooglePaymentCredentialRequiresPlayWebhookAudience(t *testing.T) {
	isolatePaymentWebhookCredentialResources(t)
	pop := server.Vault.PushSimpleResource("google.yml", []byte(
		"webhook:\n"+
			"  package_name: synthetic.package\n"+
			"  publisher_email: publisher@synthetic.example\n"+
			"oauth:\n"+
			"  client_id: synthetic-client-id\n"+
			"  client_secret: synthetic-client-secret\n"+
			"  refresh_token: synthetic-refresh-token\n",
	))
	defer pop()

	if controller.PlayWebhookVerifiable() == nil {
		t.Fatal("the Play webhook verifies without webhook.audience; this control no longer reproduces the production refusal")
	}
	requirement := credentialRequirementByKey(t, loadCredentialRequirements("main", false, nil), "google-payment")
	if !requirement.Present || requirement.Malformed || !requirement.Required ||
		!reflect.DeepEqual(requirement.MissingFields, []string{"webhook.audience"}) {
		t.Fatalf("google-payment readiness = %+v, want only webhook.audience missing", requirement)
	}

	settings := syntheticSettings(&syntheticSource{})
	settings.Credentials = []CredentialRequirement{requirement}
	alerts, err := NewCredentialsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "credential-fields-missing")
	if len(alerts) != 1 || alert.Severity != SeverityPage || alert.Target != "google-payment" ||
		alert.Frame != "resource=google.yml" || !strings.Contains(alert.Observed, "missing_fields=webhook.audience") {
		t.Fatalf("missing Play webhook audience did not page: %+v", alerts)
	}
	requireAlertOmits(t, alert, "publisher@synthetic.example", "synthetic-client-secret", "synthetic-refresh-token")
}

// A payment webhook refuses every push while a field its verifier reads is
// missing or blank, so the inventory for its Vault resource must include every
// such field: a resource the credentials signal calls complete must let the
// webhook verify. The inventory's own field list is the fixture, so a verifier
// that starts reading a new field fails here until the inventory lists it.
func TestPaymentWebhookCredentialInventoryCoversItsVerifier(t *testing.T) {
	isolatePaymentWebhookCredentialResources(t)
	for _, webhook := range []struct {
		key        string
		resource   string
		verifiable func() error
	}{
		{key: "google-payment", resource: "google.yml", verifiable: controller.PlayWebhookVerifiable},
		{key: "stripe-payment", resource: "stripe.yml", verifiable: controller.StripeWebhookVerifiable},
		{key: "coinbase-payment", resource: "coinbase.yml", verifiable: controller.CoinbaseWebhookVerifiable},
		{key: "solana-payment", resource: "helius.yml", verifiable: controller.HeliusWebhookVerifiable},
	} {
		// an empty document reports every field the inventory requires
		pop := server.Vault.PushSimpleResource(webhook.resource, []byte("{}\n"))
		required := credentialRequirementByKey(t, loadCredentialRequirements("main", false, nil), webhook.key).MissingFields
		pop()
		if len(required) == 0 {
			t.Fatalf("%s: the inventory requires no field of %s", webhook.key, webhook.resource)
		}

		document := map[string]any{}
		for _, name := range required {
			node := document
			path := strings.Split(name, ".")
			for _, component := range path[:len(path)-1] {
				child, ok := node[component].(map[string]any)
				if !ok {
					child = map[string]any{}
					node[component] = child
				}
				node = child
			}
			node[path[len(path)-1]] = "synthetic-" + strings.ReplaceAll(name, ".", "-")
		}
		content, err := yaml.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}

		pop = server.Vault.PushSimpleResource(webhook.resource, content)
		requirement := credentialRequirementByKey(t, loadCredentialRequirements("main", false, nil), webhook.key)
		verifyErr := webhook.verifiable()
		pop()
		if len(requirement.MissingFields) != 0 || requirement.Malformed {
			t.Fatalf("%s: the inventory's own fields left it incomplete: %+v", webhook.key, requirement)
		}
		if verifyErr != nil {
			t.Fatalf("%s: the credentials inventory calls %s complete, but its webhook refuses every push: %v",
				webhook.key, webhook.resource, verifyErr)
		}
	}
}
