package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The pin endpoint is what stands between the prober and probing unpinned, so
// its auth has to fail closed in exactly the two ways an operator gets wrong:
// no header at all (a deployment that never configured the secret) and a wrong
// one (a rotated secret on one side only). Both are 401, and neither reaches
// the database.
func TestGeolocationSourcePinsRejectsMissingSecret(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/network/geolocation-source-pins", nil)
	w := httptest.NewRecorder()

	GeolocationSourcePins(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the operator secret header is absent", w.Code)
	}
}

func TestGeolocationSourcePinsRejectsWrongSecret(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/network/geolocation-source-pins", nil)
	req.Header.Set(operatorSecretHeader, "definitely-not-the-secret")
	w := httptest.NewRecorder()

	GeolocationSourcePins(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on a wrong operator secret", w.Code)
	}
}

// TestGeolocationSourcePinsRejectsAlteredSecret pins the comparison itself.
// The two tests above both run with the vault unconfigured and take the
// secret=="" short-circuit, so they would still pass if the comparison were
// `strings.HasPrefix` or dropped entirely. This one configures a real secret
// and offers a near miss.
func TestGeolocationSourcePinsRejectsAlteredSecret(t *testing.T) {
	const secret = "correct-operator-secret-0123456789"
	defer withStubOperatorIngestSecret(secret)()

	for _, wrong := range []string{
		secret + "x",
		secret[:len(secret)-1],
		"",
	} {
		req := httptest.NewRequest(http.MethodGet, "/network/geolocation-source-pins", nil)
		if wrong != "" {
			req.Header.Set(operatorSecretHeader, wrong)
		}
		w := httptest.NewRecorder()

		GeolocationSourcePins(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d for secret %q, want 401", w.Code, wrong)
		}
	}
}

// TestGeolocationSourcePinsServesTheObservedSet is the other half: the auth
// gate must be able to ACCEPT, and what comes back must be what the
// observation job stored, keyed by host, on the exact wire shape the prober
// decodes (`{host: {leaf, intermediate}}`).
//
// It asserts on the decoded JSON rather than on the handler's Go types,
// because the prober is a separate repository that only ever sees the bytes:
// a renamed json tag would be invisible to a Go-level assertion and would take
// the fleet's probing offline.
func TestGeolocationSourcePinsServesTheObservedSet(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		observedAt := server.NowUtc()
		model.SetGeolocationSourcePin(t.Context(), &model.GeolocationSourcePin{
			Host:             "ipinfo.io",
			LeafSpki:         "leaf-ipinfo",
			IntermediateSpki: "int-ipinfo",
			ObservedAt:       observedAt,
		})
		model.SetGeolocationSourcePin(t.Context(), &model.GeolocationSourcePin{
			Host:             "api.i.pn",
			LeafSpki:         "leaf-ipn",
			IntermediateSpki: "int-ipn",
			ObservedAt:       observedAt,
		})

		req := httptest.NewRequest(http.MethodGet, "/network/geolocation-source-pins", nil)
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()

		GeolocationSourcePins(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 with the correct operator secret. body = %s", w.Code, w.Body.String())
		}

		var got map[string]map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode response %q: %s", w.Body.String(), err)
		}
		for host, want := range map[string][2]string{
			"ipinfo.io": {"leaf-ipinfo", "int-ipinfo"},
			"api.i.pn":  {"leaf-ipn", "int-ipn"},
		} {
			pin, ok := got[host]
			if !ok {
				t.Fatalf("host %q missing from the served set %v; the prober treats a missing source host as a hard stop", host, got)
			}
			if pin["leaf"] != want[0] {
				t.Errorf("%s leaf = %q, want %q", host, pin["leaf"], want[0])
			}
			if pin["intermediate"] != want[1] {
				t.Errorf("%s intermediate = %q, want %q", host, pin["intermediate"], want[1])
			}
		}
	})
}

// A host that has never been observed must stay ABSENT from the answer. The
// endpoint must not invent a placeholder row to make the map look complete:
// the prober decides what to do about a missing host (refuse to probe), and it
// can only decide that if the absence reaches it.
func TestGeolocationSourcePinsOmitsUnobservedHosts(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		// exactly one of the source hosts observed
		model.SetGeolocationSourcePin(t.Context(), &model.GeolocationSourcePin{
			Host:             model.GeolocationSourceHosts[0],
			LeafSpki:         "leaf-only",
			IntermediateSpki: "int-only",
			ObservedAt:       server.NowUtc(),
		})

		req := httptest.NewRequest(http.MethodGet, "/network/geolocation-source-pins", nil)
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()

		GeolocationSourcePins(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200. body = %s", w.Code, w.Body.String())
		}
		var got map[string]map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode response %q: %s", w.Body.String(), err)
		}
		if len(got) != 1 {
			t.Fatalf("served %d host(s) %v, want only the one that was observed", len(got), got)
		}
		for _, host := range model.GeolocationSourceHosts[1:] {
			if _, ok := got[host]; ok {
				t.Errorf("unobserved host %q appears in the served set; a placeholder pin here would take the fail-closed decision away from the prober", host)
			}
		}
	})
}
