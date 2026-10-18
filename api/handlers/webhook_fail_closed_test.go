package handlers

// Webhook routes answer 503 while their verification secret is unavailable.
// Each reads a synthetic vault resource pushed by the test; no test contacts a
// provider.

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Serves one request to a webhook route. A panic fails the test: an
// unavailable secret must be answered, not panic.
func serveWebhookRoute(t *testing.T, handler http.HandlerFunc, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("webhook route panicked instead of answering: %v", r)
		}
	}()
	handler(recorder, req)
	return recorder
}

// A blank secret used to let every Helius and Brevo request through, and a
// missing key panicked each webhook route; the routes that verify in a body
// formatter answered any refusal 400, as if the caller were at fault.
func TestWebhookRoutesAnswerUnavailableSecret(t *testing.T) {
	brevoBlankUser := map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(":"))}
	for _, route := range []struct {
		resource string
		handler  http.HandlerFunc
		body     string
		headers  map[string]string
		blank    string
	}{
		{resource: "helius.yml", handler: HeliusWebhook, body: "[]", blank: "helius:\n  webhook_auth_header: ''\n"},
		{resource: "coinbase.yml", handler: CoinbaseWebhook, body: "{}", blank: "webhook:\n  shared_secret: ''\n"},
		{resource: "stripe.yml", handler: StripeWebhook, body: "{}", blank: "webhook:\n  signing_secret: ''\n"},
		{resource: "google.yml", handler: PlayWebhook, body: "{}", blank: "webhook:\n  publisher_email: ''\n"},
		{resource: "brevo.yml", handler: BrevoWebhook, body: "{}", headers: brevoBlankUser, blank: "brevo:\n  webhook_bearers:\n    - ''\n"},
	} {
		for _, resource := range []string{route.blank, "other: synthetic\n"} {
			pop := server.Vault.PushSimpleResource(route.resource, []byte(resource))
			recorder := serveWebhookRoute(t, route.handler, route.body, route.headers)
			if recorder.Code != http.StatusServiceUnavailable || strings.TrimSpace(recorder.Body.String()) != "Service temporarily unavailable." {
				t.Errorf("%s %q answered %d %q", route.resource, resource, recorder.Code, recorder.Body.String())
			}
			pop()
		}
	}
}

func TestHeliusWebhookRouteServesConfiguredSecret(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("helius.yml", []byte("helius:\n  webhook_auth_header: synthetic-helius-webhook-secret\n")))
	recorder := serveWebhookRoute(t, HeliusWebhook, "[]", map[string]string{"Authorization": "synthetic-helius-webhook-secret"})
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != `{"message":"No transactions"}` {
		t.Fatalf("configured secret answered %d %q", recorder.Code, recorder.Body.String())
	}
	recorder = serveWebhookRoute(t, HeliusWebhook, "[]", nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("request without the secret answered %d %q", recorder.Code, recorder.Body.String())
	}
}
