// The verified Google signature authenticates a claim set, not this endpoint.
// Every Play request must also bind the intended receiver and verified sender.
package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/server"
)

const syntheticPlayAudience = "https://push.fixture.example/play"

func TestVerifyPlayBodyBindsAudienceIssuerAndVerifiedEmail(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("google.yml", []byte("webhook:\n  publisher_email: publisher@fixture.example\n  audience: "+syntheticPlayAudience+"\n")))
	for _, field := range []string{"aud", "iss", "email_verified"} {
		for _, value := range []any{nil, "unrelated.example", false, 123} {
			claims := map[string]any{"email": "publisher@fixture.example", "email_verified": "true", "aud": syntheticPlayAudience, "iss": "https://accounts.google.com"}
			claims[field] = value
			setPlayIdTokenClaims(t, func(context.Context, string) (map[string]any, error) { return claims, nil })
			if _, err := verifyWebhookBody(t, VerifyPlayBody, "{}", map[string]string{"Authorization": "Bearer synthetic-id-token"}); err == nil || errors.Is(err, errWebhookVerificationUnavailable) {
				t.Errorf("claim %s=%v was not refused as invalid: %v", field, value, err)
			}
		}
	}
}

func TestVerifyPlayBodyRefusesUnavailableAudienceBeforeLookup(t *testing.T) {
	lookups := 0
	setPlayIdTokenClaims(t, func(context.Context, string) (map[string]any, error) {
		lookups++
		return map[string]any{"email": "publisher@fixture.example"}, nil
	})
	for _, value := range []string{"", "  audience: ''\n", "  audience: '   '\n", "  audience: 123\n"} {
		pop := server.Vault.PushSimpleResource("google.yml", []byte("webhook:\n  publisher_email: publisher@fixture.example\n"+value))
		if _, err := verifyWebhookBody(t, VerifyPlayBody, "{}", map[string]string{"Authorization": "Bearer synthetic-id-token"}); !errors.Is(err, errWebhookVerificationUnavailable) {
			t.Errorf("unavailable audience returned %v", err)
		}
		pop()
	}
	if lookups != 0 {
		t.Errorf("unavailable audience performed %d Google lookups", lookups)
	}
}

func TestVerifyPlayBodyAcceptsBoundVerifiedPublisher(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("google.yml", []byte("webhook:\n  publisher_email: publisher@fixture.example\n  audience: "+syntheticPlayAudience+"\n")))
	for _, issuer := range []string{"https://accounts.google.com", "accounts.google.com"} {
		for _, verified := range []any{true, "true"} {
			setPlayIdTokenClaims(t, func(context.Context, string) (map[string]any, error) {
				return map[string]any{"email": "publisher@fixture.example", "email_verified": verified, "aud": syntheticPlayAudience, "iss": issuer}, nil
			})
			body, err := verifyWebhookBody(t, VerifyPlayBody, "{}", map[string]string{"Authorization": "Bearer synthetic-id-token"})
			if err != nil || string(body) != "{}" {
				t.Errorf("bound verified token: %v, %q", err, body)
			}
		}
	}
}
