package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

func TestProviderEgressLocationSubmitRejectsMissingSecret(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"client_id": "019f8835-158d-6fd8-e9dd-fd0e4c6d6792",
	})
	req := httptest.NewRequest(http.MethodPost, "/network/provider-egress-location", bytes.NewReader(body))
	w := httptest.NewRecorder()

	ProviderEgressLocationSubmit(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the operator secret header is absent", w.Code)
	}
}

func TestProviderEgressLocationSubmitRejectsWrongSecret(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"client_id": "019f8835-158d-6fd8-e9dd-fd0e4c6d6792",
	})
	req := httptest.NewRequest(http.MethodPost, "/network/provider-egress-location", bytes.NewReader(body))
	req.Header.Set(operatorSecretHeader, "definitely-not-the-secret")
	w := httptest.NewRecorder()

	ProviderEgressLocationSubmit(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on a wrong operator secret", w.Code)
	}
}

// withStubOperatorIngestSecret swaps the package-level operatorIngestSecret
// memo for a stub that always returns secret, and returns a func to restore
// the original (real, still-memoized) reader. This lets a test exercise the
// "configured vault" path without the sync.OnceValue in the real reader ever
// touching the vault, and without the reject tests above (which rely on an
// unconfigured vault) observing any change.
func withStubOperatorIngestSecret(secret string) (restore func()) {
	prev := operatorIngestSecret
	operatorIngestSecret = func() string { return secret }
	return func() { operatorIngestSecret = prev }
}

// TestProviderEgressLocationSubmitAcceptsCorrectSecret proves the auth gate
// can ACCEPT a correct secret and hand off to the controller. Without this
// test, the two reject tests above (which both run with the vault
// unconfigured and take the secret=="" short-circuit) would pass unchanged
// even if the handler's entire body were replaced with an unconditional 401 -
// hmac.Equal would never be proven to run on a real match.
//
// Clearing auth hands the request to controller.SubmitProviderEgressLocation,
// which looks up the client in the database before it can return "Unknown
// client.", so this test needs a real (throwaway) test database - see
// server.DefaultTestEnv, the same harness
// controller/provider_egress_location_controller_test.go uses. t.Setenv makes
// it self-sufficient under a plain `go test`, matching the pattern in
// router/warp_handlers_status_test.go.
func TestProviderEgressLocationSubmitAcceptsCorrectSecret(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		// A syntactically valid, semantically unregistered submission: once
		// auth clears, the controller looks up the client and (since it does
		// not exist in the fresh test database) returns "Unknown client.",
		// surfaced by the handler as 400. That 400 is proof the request
		// reached the controller, i.e. proof auth passed.
		args := controller.SubmitProviderEgressLocationArgs{
			ClientId:         server.NewId(),
			CountryCode:      "US",
			Country:          "United States",
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		}
		body, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("marshal args: %s", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/network/provider-egress-location", bytes.NewReader(body))
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()

		ProviderEgressLocationSubmit(w, req)

		if w.Code == http.StatusUnauthorized {
			t.Fatalf("status = %d, want the correct secret to clear auth (not 401)", w.Code)
		}
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (Unknown client.) once auth clears for an unregistered client id; body = %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "Unknown client.") {
			t.Fatalf("body = %q, want it to report the unknown client", w.Body.String())
		}
	})
}

// TestProviderEgressLocationSubmitRejectsAlteredSecret proves that once the
// vault is configured, hmac.Equal is actually consulted rather than the
// endpoint accepting any request once secret != "". Same configured secret as
// the accept test above, but the request carries a one-character-altered
// value.
func TestProviderEgressLocationSubmitRejectsAlteredSecret(t *testing.T) {
	const secret = "correct-operator-secret-0123456789"
	const wrongSecret = "correct-operator-secret-0123456780" // last char changed
	defer withStubOperatorIngestSecret(secret)()

	body, _ := json.Marshal(map[string]any{
		"client_id": "019f8835-158d-6fd8-e9dd-fd0e4c6d6792",
	})
	req := httptest.NewRequest(http.MethodPost, "/network/provider-egress-location", bytes.NewReader(body))
	req.Header.Set(operatorSecretHeader, wrongSecret)
	w := httptest.NewRecorder()

	ProviderEgressLocationSubmit(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the operator secret is configured but the request's secret is wrong", w.Code)
	}
}

// TestProviderEgressLocationSubmitReadsSecretFromVault proves the vault
// plumbing itself - not the test stub - returns the configured secret:
// readOperatorIngestSecret (the un-memoized reader) reads a
// PushSimpleResource-injected provider_egress.yml.
func TestProviderEgressLocationSubmitReadsSecretFromVault(t *testing.T) {
	const secret = "vault-provisioned-secret-abcdef"
	pop := server.Vault.PushSimpleResource(
		"provider_egress.yml",
		[]byte(`ingest_secret: "`+secret+`"`),
	)
	defer pop()

	if got := readOperatorIngestSecret(); got != secret {
		t.Fatalf("readOperatorIngestSecret() = %q, want %q", got, secret)
	}
}
