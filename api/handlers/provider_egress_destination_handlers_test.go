// Tests for the destination pool route: the operator secret, and the prober
// module's own pool shape on the wire.
package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server/qualityprobe/egresshealth"

	"github.com/urnetwork/server"
)

// Without the operator secret the pool is not served.
func TestProviderEgressDestinationsRejectsMissingSecret(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/network/provider-egress-destinations", nil)
	w := httptest.NewRecorder()
	ProviderEgressDestinations(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without the operator secret", w.Code)
	}
}

// Under the secret a fresh deployment serves the seeded built-in table, in the
// shape the prober decodes and validates.
func TestProviderEgressDestinationsServesThePool(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		req := httptest.NewRequest(http.MethodGet, "/network/provider-egress-destinations", nil)
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()
		ProviderEgressDestinations(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		var pool egresshealth.Pool
		if err := json.Unmarshal(w.Body.Bytes(), &pool); err != nil {
			t.Fatalf("decode pool: %v", err)
		}
		if len(pool.Destinations) != len(egresshealth.Destinations()) || pool.Version < 1 || pool.GeneratedAt.IsZero() {
			t.Fatalf("served pool = %d destinations, version %d, generated %s", len(pool.Destinations), pool.Version, pool.GeneratedAt)
		}
		if err := egresshealth.ValidateDestinations(pool.Destinations); err != nil {
			t.Fatalf("the served pool is one the prober would refuse: %v", err)
		}

		post := httptest.NewRequest(http.MethodPost, "/network/provider-egress-destinations", nil)
		post.Header.Set(operatorSecretHeader, secret)
		w = httptest.NewRecorder()
		ProviderEgressDestinations(w, post)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405 for a post", w.Code)
		}
	})
}
