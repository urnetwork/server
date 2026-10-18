package controller

// Webhook verifiers fail closed. Each reads its secret from a synthetic vault
// resource pushed by the test; no test contacts a provider.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	stripewebhook "github.com/stripe/stripe-go/v82/webhook"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A vault value no deployment carries.
const syntheticWebhookSecret = "synthetic-webhook-secret-for-tests"

// Runs a body verifier on a request with body and headers. A panic fails the
// test: an unavailable secret must refuse, not panic.
func verifyWebhookBody(
	t *testing.T,
	verify func(*session.ClientSession, *http.Request) (io.Reader, error),
	body string,
	headers map[string]string,
) (verified []byte, err error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("verifier panicked instead of refusing: %v", r)
		}
	}()
	reader, err := verify(nil, req)
	if err != nil {
		if reader != nil {
			t.Fatal("refused webhook still returned its body")
		}
		return nil, err
	}
	verified, err = io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return verified, nil
}

// Resources whose secret at section.key is blank.
func blankWebhookSecrets(section string, key string) []string {
	return []string{
		section + ":\n  " + key + ": ''\n",
		section + ":\n  " + key + ": '   '\n",
	}
}

// Resources without a string at section.key: a missing key or mapping, a
// value of another type, and a malformed file.
func missingWebhookSecrets(section string, key string) []string {
	return []string{
		section + ":\n  other: synthetic\n",
		section + ":\n  " + key + ": 12345\n",
		section + ": synthetic\n",
		"other: synthetic\n",
		section + ": [\n",
	}
}

func coinbaseTestSignature(body string, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// A blank configured value used to equal the Authorization of a request that
// sends none, so anyone could post Solana payments. It refuses as unavailable.
func TestVerifyHeliusBodyRefusesBlankSecret(t *testing.T) {
	for _, resource := range blankWebhookSecrets("helius", "webhook_auth_header") {
		pop := server.Vault.PushSimpleResource("helius.yml", []byte(resource))
		for _, headers := range []map[string]string{nil, {"Authorization": ""}, {"Authorization": syntheticWebhookSecret}} {
			if _, err := verifyWebhookBody(t, VerifyHeliusBody, "[]", headers); !errors.Is(err, errWebhookVerificationUnavailable) {
				t.Errorf("helius.yml %q with %v: %v", resource, headers, err)
			}
		}
		pop()
	}
}

// A missing value panicked on every webhook, from the unchecked type
// assertion. It refuses as unavailable.
func TestVerifyHeliusBodyRefusesMissingSecretWithoutPanic(t *testing.T) {
	for _, resource := range missingWebhookSecrets("helius", "webhook_auth_header") {
		pop := server.Vault.PushSimpleResource("helius.yml", []byte(resource))
		for _, headers := range []map[string]string{nil, {"Authorization": ""}, {"Authorization": syntheticWebhookSecret}} {
			if _, err := verifyWebhookBody(t, VerifyHeliusBody, "[]", headers); !errors.Is(err, errWebhookVerificationUnavailable) {
				t.Errorf("helius.yml %q with %v: %v", resource, headers, err)
			}
		}
		pop()
	}
}

func TestVerifyHeliusBodyAcceptsConfiguredSecret(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("helius.yml", []byte("helius:\n  webhook_auth_header: "+syntheticWebhookSecret+"\n")))
	verified, err := verifyWebhookBody(t, VerifyHeliusBody, `[{"signature":"synthetic"}]`, map[string]string{"Authorization": syntheticWebhookSecret})
	if err != nil || !bytes.Equal(verified, []byte(`[{"signature":"synthetic"}]`)) {
		t.Fatalf("configured secret did not verify: %v %q", err, verified)
	}
}

func TestVerifyHeliusBodyRefusesOtherAuthorization(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("helius.yml", []byte("helius:\n  webhook_auth_header: "+syntheticWebhookSecret+"\n")))
	for _, headers := range []map[string]string{nil, {"Authorization": "synthetic-other-secret"}, {"Authorization": syntheticWebhookSecret[:8]}} {
		if _, err := verifyWebhookBody(t, VerifyHeliusBody, "[]", headers); err == nil || errors.Is(err, errWebhookVerificationUnavailable) {
			t.Errorf("authorization %v was not refused as invalid: %v", headers, err)
		}
	}
}

// Anyone can sign with an empty key, and that signature was accepted under a
// blank configured secret. It refuses as unavailable.
func TestVerifyCoinbaseBodyRefusesBlankSecret(t *testing.T) {
	body := `{"event":{"type":"charge:confirmed"}}`
	for _, resource := range blankWebhookSecrets("webhook", "shared_secret") {
		pop := server.Vault.PushSimpleResource("coinbase.yml", []byte(resource))
		headers := map[string]string{"X-CC-Webhook-Signature": coinbaseTestSignature(body, "")}
		if _, err := verifyWebhookBody(t, VerifyCoinbaseBody, body, headers); !errors.Is(err, errWebhookVerificationUnavailable) {
			t.Errorf("coinbase.yml %q: %v", resource, err)
		}
		pop()
	}
}

// A missing secret panicked on every webhook. It refuses as unavailable.
func TestVerifyCoinbaseBodyRefusesMissingSecretWithoutPanic(t *testing.T) {
	body := `{"event":{"type":"charge:confirmed"}}`
	for _, resource := range missingWebhookSecrets("webhook", "shared_secret") {
		pop := server.Vault.PushSimpleResource("coinbase.yml", []byte(resource))
		headers := map[string]string{"X-CC-Webhook-Signature": coinbaseTestSignature(body, "")}
		if _, err := verifyWebhookBody(t, VerifyCoinbaseBody, body, headers); !errors.Is(err, errWebhookVerificationUnavailable) {
			t.Errorf("coinbase.yml %q: %v", resource, err)
		}
		pop()
	}
}

func TestVerifyCoinbaseBodyAcceptsConfiguredSecret(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("coinbase.yml", []byte("webhook:\n  shared_secret: "+syntheticWebhookSecret+"\n")))
	body := `{"event":{"type":"charge:confirmed"}}`
	verified, err := verifyWebhookBody(t, VerifyCoinbaseBody, body, map[string]string{"X-CC-Webhook-Signature": coinbaseTestSignature(body, syntheticWebhookSecret)})
	if err != nil || string(verified) != body {
		t.Fatalf("configured secret did not verify: %v %q", err, verified)
	}
}

func TestVerifyCoinbaseBodyRefusesOtherSignature(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("coinbase.yml", []byte("webhook:\n  shared_secret: "+syntheticWebhookSecret+"\n")))
	body := `{"event":{"type":"charge:confirmed"}}`
	for _, signature := range []string{coinbaseTestSignature(body, ""), coinbaseTestSignature(body, "synthetic-other-secret"), ""} {
		if _, err := verifyWebhookBody(t, VerifyCoinbaseBody, body, map[string]string{"X-CC-Webhook-Signature": signature}); err == nil {
			t.Errorf("signature %q was accepted", signature)
		}
	}
	if err := coinbaseSignature([]byte(body), coinbaseTestSignature(body, ""), ""); err == nil {
		t.Fatal("an empty key authenticated a body")
	}
}

func stripeTestSignature(body string, secret string) string {
	return stripewebhook.GenerateTestSignedPayload(&stripewebhook.UnsignedPayload{Payload: []byte(body), Secret: secret}).Header
}

// stripe-go accepts a signature under an empty key, and a blank configured
// secret was passed to it. It refuses as unavailable.
func TestVerifyStripeBodyRefusesBlankSecret(t *testing.T) {
	body := `{"id":"evt_synthetic","object":"event","type":"invoice.paid"}`
	for _, resource := range blankWebhookSecrets("webhook", "signing_secret") {
		pop := server.Vault.PushSimpleResource("stripe.yml", []byte(resource))
		if _, err := verifyWebhookBody(t, VerifyStripeBody, body, map[string]string{"Stripe-Signature": stripeTestSignature(body, "")}); !errors.Is(err, errWebhookVerificationUnavailable) {
			t.Errorf("stripe.yml %q: %v", resource, err)
		}
		pop()
	}
}

// A missing secret panicked on every webhook. It refuses as unavailable.
func TestVerifyStripeBodyRefusesMissingSecretWithoutPanic(t *testing.T) {
	body := `{"id":"evt_synthetic","object":"event","type":"invoice.paid"}`
	for _, resource := range missingWebhookSecrets("webhook", "signing_secret") {
		pop := server.Vault.PushSimpleResource("stripe.yml", []byte(resource))
		if _, err := verifyWebhookBody(t, VerifyStripeBody, body, map[string]string{"Stripe-Signature": stripeTestSignature(body, "")}); !errors.Is(err, errWebhookVerificationUnavailable) {
			t.Errorf("stripe.yml %q: %v", resource, err)
		}
		pop()
	}
}

func TestVerifyStripeBodyAcceptsConfiguredSecret(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("stripe.yml", []byte("webhook:\n  signing_secret: "+syntheticWebhookSecret+"\n")))
	body := `{"id":"evt_synthetic","object":"event","type":"invoice.paid"}`
	verified, err := verifyWebhookBody(t, VerifyStripeBody, body, map[string]string{"Stripe-Signature": stripeTestSignature(body, syntheticWebhookSecret)})
	if err != nil || string(verified) != body {
		t.Fatalf("configured secret did not verify: %v %q", err, verified)
	}
}

func TestVerifyStripeBodyRefusesOtherSignature(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("stripe.yml", []byte("webhook:\n  signing_secret: "+syntheticWebhookSecret+"\n")))
	body := `{"id":"evt_synthetic","object":"event","type":"invoice.paid"}`
	for _, signature := range []string{stripeTestSignature(body, ""), stripeTestSignature(body, "synthetic-other-secret"), ""} {
		if _, err := verifyWebhookBody(t, VerifyStripeBody, body, map[string]string{"Stripe-Signature": signature}); err == nil {
			t.Errorf("signature %q was accepted", signature)
		}
	}
}

// Replaces Google's tokeninfo for the rest of the test.
func setPlayIdTokenClaims(t *testing.T, claims func(ctx context.Context, idToken string) (map[string]any, error)) {
	t.Helper()
	previous := playIdTokenClaims
	playIdTokenClaims = claims
	t.Cleanup(func() { playIdTokenClaims = previous })
}

// A missing publisher panicked once Google had answered for the token, and a
// blank one compared equal to a token without an email. Both refuse as
// unavailable now, before Google is asked.
func TestVerifyPlayBodyRefusesUnavailablePublisher(t *testing.T) {
	asked := 0
	setPlayIdTokenClaims(t, func(context.Context, string) (map[string]any, error) {
		asked += 1
		return map[string]any{"email": ""}, nil
	})
	for _, resource := range append(blankWebhookSecrets("webhook", "publisher_email"), missingWebhookSecrets("webhook", "publisher_email")...) {
		pop := server.Vault.PushSimpleResource("google.yml", []byte(resource))
		if _, err := verifyWebhookBody(t, VerifyPlayBody, "{}", map[string]string{"Authorization": "Bearer synthetic-id-token"}); !errors.Is(err, errWebhookVerificationUnavailable) {
			t.Errorf("google.yml %q: %v", resource, err)
		}
		pop()
	}
	if asked != 0 {
		t.Fatalf("Google was asked %d times without a configured publisher", asked)
	}
}

func TestVerifyPlayBodyAcceptsPublisherToken(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("google.yml", []byte("webhook:\n  publisher_email: publisher@fixture.example\n  audience: https://push.fixture.example/play\n")))
	setPlayIdTokenClaims(t, func(_ context.Context, idToken string) (map[string]any, error) {
		if idToken != "synthetic-id-token" {
			t.Errorf("tokeninfo asked about %q", idToken)
		}
		return map[string]any{"email": "publisher@fixture.example", "email_verified": "true", "aud": "https://push.fixture.example/play", "iss": "https://accounts.google.com"}, nil
	})
	verified, err := verifyWebhookBody(t, VerifyPlayBody, `{"message":{}}`, map[string]string{"Authorization": "Bearer synthetic-id-token"})
	if err != nil || string(verified) != `{"message":{}}` {
		t.Fatalf("publisher token did not verify: %v %q", err, verified)
	}
}

func TestVerifyPlayBodyRefusesOtherToken(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("google.yml", []byte("webhook:\n  publisher_email: publisher@fixture.example\n  audience: https://push.fixture.example/play\n")))
	setPlayIdTokenClaims(t, func(context.Context, string) (map[string]any, error) {
		return map[string]any{"email": "other@fixture.example"}, nil
	})
	for _, authorization := range []string{"Bearer synthetic-id-token", "synthetic-id-token", ""} {
		if _, err := verifyWebhookBody(t, VerifyPlayBody, "{}", map[string]string{"Authorization": authorization}); err == nil || errors.Is(err, errWebhookVerificationUnavailable) {
			t.Errorf("authorization %q was not refused as invalid: %v", authorization, err)
		}
	}
}

func brevoTestSession(authorization string) *session.ClientSession {
	return &session.ClientSession{
		Ctx:    context.Background(),
		Header: map[string][]string{"Authorization": {authorization}},
	}
}

// Calls BrevoWebhook, failing the test where it panics instead of refusing.
func brevoTestWebhook(t *testing.T, authorization string) error {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("brevo webhook panicked instead of refusing: %v", r)
		}
	}()
	_, err := BrevoWebhook(&BrevoWebhookArgs{}, brevoTestSession(authorization))
	return err
}

// A blank configured user became the accepted credential "Basic Og==", which
// anyone can send, letting any caller unsubscribe any email; a malformed file
// panicked on every webhook. A resource without a usable user refuses as
// unavailable now.
func TestBrevoWebhookRefusesUnavailableUsers(t *testing.T) {
	for _, resource := range []string{
		"brevo:\n  webhook_bearers:\n    - ''\n",
		"brevo:\n  webhook_bearers: ''\n",
		"brevo:\n  webhook_bearers:\n    - '   '\n",
		"brevo:\n  webhook_bearers: []\n",
		"brevo:\n  api_key: synthetic\n",
		"brevo: [\n",
	} {
		pop := server.Vault.PushSimpleResource("brevo.yml", []byte(resource))
		if err := brevoTestWebhook(t, "Basic "+base64.StdEncoding.EncodeToString([]byte(":"))); !errors.Is(err, errWebhookVerificationUnavailable) {
			t.Errorf("brevo.yml %q: %v", resource, err)
		}
		pop()
	}
}

func TestBrevoWebhookAcceptsConfiguredUser(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("brevo.yml", []byte("brevo:\n  webhook_bearers:\n    - ''\n    - synthetic-brevo-user\n")))
	if err := brevoTestWebhook(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("synthetic-brevo-user:"))); err != nil {
		t.Fatalf("configured user did not verify: %v", err)
	}
}

func TestBrevoWebhookRefusesOtherUser(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("brevo.yml", []byte("brevo:\n  webhook_bearers:\n    - ''\n    - synthetic-brevo-user\n")))
	for _, user := range []string{"", "synthetic-other-user"} {
		err := brevoTestWebhook(t, "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":")))
		if err == nil || errors.Is(err, errWebhookVerificationUnavailable) {
			t.Errorf("user %q was not refused as unauthorized: %v", user, err)
		}
	}
}
