package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestProberCredentialRejectsMissingSecret(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/network/prober-credential", nil)
	w := httptest.NewRecorder()

	ProberCredential(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the operator secret header is absent", w.Code)
	}
}

func TestProberCredentialRejectsWrongSecret(t *testing.T) {
	defer withStubOperatorIngestSecret("correct-operator-secret-0123456789")()

	req := httptest.NewRequest(http.MethodGet, "/network/prober-credential", nil)
	req.Header.Set(operatorSecretHeader, "definitely-not-the-secret")
	w := httptest.NewRecorder()

	ProberCredential(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on a wrong operator secret", w.Code)
	}
	if strings.Contains(w.Body.String(), "jwt") {
		t.Fatalf("a rejected request must not be told anything about the credential: %q", w.Body.String())
	}
}

// TestProberCredentialRejectsAlteredSecret is the same-length, one-byte-off
// case. A comparison written with strings.HasPrefix, or one that only checked
// length, would pass the two tests above and fail this one.
func TestProberCredentialRejectsAlteredSecret(t *testing.T) {
	const secret = "correct-operator-secret-0123456789"
	defer withStubOperatorIngestSecret(secret)()

	altered := []byte(secret)
	altered[len(altered)-1] ^= 0x01

	req := httptest.NewRequest(http.MethodGet, "/network/prober-credential", nil)
	req.Header.Set(operatorSecretHeader, string(altered))
	w := httptest.NewRecorder()

	ProberCredential(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on a secret differing in one byte", w.Code)
	}
}

// TestProberCredentialResultExposesOnlyTheDeliveryCredential pins the response
// SHAPE, so the handler keeps returning a deliberate, documented field set
// rather than whatever a model struct happens to have.
//
// The failure it exists to catch is a quiet one: someone "simplifies" the
// handler by encoding model.ProberIdentity directly, or adds a field here for
// debugging. Either compiles, builds green, and passes every status-code test
// above. Asserting the exact field set is the only check that fails.
//
// It is NOT what makes this endpoint safe, and it is not a substitute for the
// auth tests above. See the note on ProberCredentialResult: network_id, user_id
// and network_name are already inside the by_client_jwt this response carries,
// so omitting them withholds nothing from a caller who got past the operator
// secret. That secret is the gate; this test is change detection.
func TestProberCredentialResultExposesOnlyTheDeliveryCredential(t *testing.T) {
	want := []string{"by_client_jwt", "client_id"}

	var got []string
	resultType := reflect.TypeOf(ProberCredentialResult{})
	for i := range resultType.NumField() {
		tag, _, _ := strings.Cut(resultType.Field(i).Tag.Get("json"), ",")
		got = append(got, tag)
	}
	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Fatalf(
			"ProberCredentialResult fields = %v, want exactly %v; "+
				"the response carries the revocable credential ONLY, never the account identity behind it",
			got,
			want,
		)
	}

	// belt and braces: whatever the field set becomes, these must never be in
	// it, under any spelling
	for _, forbidden := range []string{"seed", "network", "user", "password", "secret"} {
		if slices.ContainsFunc(got, func(f string) bool { return strings.Contains(f, forbidden) }) {
			t.Fatalf("ProberCredentialResult must never expose a %q field: %v", forbidden, got)
		}
	}
}

// TestProberCredentialNotReadyIs404 proves the auth gate can ACCEPT, and that
// the accepted-but-not-yet-bootstrapped case is a 404.
//
// Both halves matter. Without the accept half, a handler replaced wholesale by
// an unconditional 401 would pass all three reject tests above. And the 404 is
// the contract the prober polls against: it must be able to tell "the
// bootstrap task has not finished yet, keep polling" from "something is
// broken, wake someone". An empty 200 collapses those two into one answer.
func TestProberCredentialNotReadyIs404(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		// no bootstrap has run against this fresh test database, so
		// prober_identity is empty
		req := httptest.NewRequest(http.MethodGet, "/network/prober-credential", nil)
		req.Header.Set(operatorSecretHeader, secret)
		req = req.WithContext(context.Background())
		w := httptest.NewRecorder()

		ProberCredential(w, req)

		if w.Code == http.StatusUnauthorized {
			t.Fatalf("the correct operator secret was rejected; the auth gate is broken shut")
		}
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 before the bootstrap task has minted a credential", w.Code)
		}

		// and specifically NOT a decodable credential body
		var result ProberCredentialResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err == nil && result.ByClientJwt != "" {
			t.Fatalf("a not-ready response must carry no credential, got %q", result.ByClientJwt)
		}
	})
}
