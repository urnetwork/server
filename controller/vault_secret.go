package controller

// Checked reads of the vault secrets that authenticate inbound webhooks. A
// verifier reads its secret per request and refuses, rather than accepts or
// panics, when the secret is unavailable, so a vault fix takes effect without
// a restart.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/urnetwork/server"
)

// The refusal of a webhook whose verification secret is unavailable; the cause
// stays in the server log. An implementation's refusal is answered with this
// status. The router answers a body formatter's refusal 400, so a webhook that
// verifies in its body formatter is first refused at its route, as 503
// (api/handlers webhookVerifiable).
var errWebhookVerificationUnavailable = fmt.Errorf("%d Service temporarily unavailable.", http.StatusServiceUnavailable)

// The string scalar at path, or why there is none: the resource is malformed,
// a component is missing or not a mapping, or the value is not a string or is
// blank. Typed YAML decoding would coerce a numeric scalar into a string, which
// is unsafe for a credential. The error names the path, never a value.
func simpleResourceNonblankStringValue(resource *server.SimpleResource, path ...string) (string, error) {
	object, err := resource.ParseE()
	if err != nil {
		return "", err
	}
	var value any = object
	for i, component := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return "", fmt.Errorf("%s is not a mapping", strings.Join(path[:i], "."))
		}
		value, ok = object[component]
		if !ok {
			return "", fmt.Errorf("%s is missing", strings.Join(path[:i+1], "."))
		}
	}
	stringValue, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s is not a string", strings.Join(path, "."))
	}
	if strings.TrimSpace(stringValue) == "" {
		return "", fmt.Errorf("%s is blank", strings.Join(path, "."))
	}
	return stringValue, nil
}

// The non-blank string at path in the named vault resource, or why there is
// none, an absent resource included.
func vaultNonblankString(name string, path ...string) (string, error) {
	resource, err := server.Vault.SimpleResource(name)
	if err != nil {
		return "", err
	}
	value, err := simpleResourceNonblankStringValue(resource, path...)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return value, nil
}
