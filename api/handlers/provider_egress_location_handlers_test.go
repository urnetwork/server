package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
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

func TestProviderEgressLocationDueRejectsMissingSecret(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/network/provider-egress-due", nil)
	w := httptest.NewRecorder()

	ProviderEgressLocationDue(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the operator secret header is absent", w.Code)
	}
}

func TestProviderEgressLocationDueRejectsWrongSecret(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/network/provider-egress-due", nil)
	req.Header.Set(operatorSecretHeader, "definitely-not-the-secret")
	w := httptest.NewRecorder()

	ProviderEgressLocationDue(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on a wrong operator secret", w.Code)
	}
}

// TestProviderEgressLocationDueRejectsAlteredSecret is the reject case with
// the vault *configured*, so the request gets past the secret == "" fail-closed
// short-circuit and hmac.Equal is what does the rejecting.
func TestProviderEgressLocationDueRejectsAlteredSecret(t *testing.T) {
	const secret = "correct-operator-secret-0123456789"
	const wrongSecret = "correct-operator-secret-0123456780" // last char changed
	defer withStubOperatorIngestSecret(secret)()

	req := httptest.NewRequest(http.MethodGet, "/network/provider-egress-due", nil)
	req.Header.Set(operatorSecretHeader, wrongSecret)
	w := httptest.NewRecorder()

	ProviderEgressLocationDue(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the operator secret is configured but the request's secret is wrong", w.Code)
	}
}

// testing_connectDueProvider stands up a connected + valid provider holding a
// Public provide key and no probe result, i.e. a provider the due query must
// return. The caller runs model.UpdateClientLocationReliabilities afterward.
func testing_connectDueProvider(
	t testing.TB,
	ctx context.Context,
	clientId server.Id,
	locationId server.Id,
	clientAddress string,
) {
	model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")

	handlerId := model.CreateNetworkClientHandler(ctx)
	connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, clientAddress, handlerId)
	if err != nil {
		t.Fatalf("connect client: %s", err)
	}
	if err := model.SetConnectionLocation(ctx, connectionId, locationId, &model.ConnectionLocationScores{}); err != nil {
		t.Fatalf("set connection location: %s", err)
	}
	model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
		model.ProvideModePublic: []byte("provide-secret"),
	})
}

// TestProviderEgressLocationDueAcceptsCorrectSecret proves the auth gate can
// ACCEPT. This is the test that gives the three reject tests above their
// meaning: without it, a handler whose entire body was replaced with an
// unconditional `http.Error(w, "Unauthorized", 401)` would still pass all
// three, because a suite that only ever asserts rejections cannot tell a
// working auth check from a broken-shut one.
//
// It deliberately asserts more than "not 401": a real, never-probed provider
// is stood up in the test database and must come back in the response body, so
// the test also fails if the handler clears auth but never reaches the model
// query or writes the wrong json shape.
func TestProviderEgressLocationDueAcceptsCorrectSecret(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		ctx := context.Background()

		city := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		model.CreateLocation(ctx, city)

		due := server.NewId()
		testing_connectDueProvider(t, ctx, due, city.LocationId, "0.0.0.1:0")
		model.UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

		req := httptest.NewRequest(http.MethodGet, "/network/provider-egress-due?limit=10", nil)
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()

		ProviderEgressLocationDue(w, req)

		if w.Code == http.StatusUnauthorized {
			t.Fatalf("status = %d, want the correct secret to clear auth (not 401)", w.Code)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}

		var result ProviderEgressLocationDueResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode body %q: %s", w.Body.String(), err)
		}
		if !slices.Contains(result.ClientIds, due) {
			t.Fatalf("client_ids = %v, want it to contain the never-probed provider %s", result.ClientIds, due)
		}
		// the wire name the prober reads
		if !strings.Contains(w.Body.String(), `"client_ids"`) {
			t.Fatalf("body = %s, want a client_ids field", w.Body.String())
		}
	})
}

// The prober asks for a batch; the server must not hand back more than asked.
func TestProviderEgressLocationDueHonoursLimit(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		ctx := context.Background()

		city := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		model.CreateLocation(ctx, city)

		testing_connectDueProvider(t, ctx, server.NewId(), city.LocationId, "0.0.0.1:0")
		testing_connectDueProvider(t, ctx, server.NewId(), city.LocationId, "0.0.0.2:0")
		model.UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

		req := httptest.NewRequest(http.MethodGet, "/network/provider-egress-due?limit=1", nil)
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()

		ProviderEgressLocationDue(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		var result ProviderEgressLocationDueResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode body %q: %s", w.Body.String(), err)
		}
		if len(result.ClientIds) != 1 {
			t.Fatalf("len(client_ids) = %d, want 1 for limit=1; body = %s", len(result.ClientIds), w.Body.String())
		}
	})
}

// A limit that is not a positive integer is a caller bug. Silently clamping it
// to 1 (or to the default) would answer a question the prober did not ask --
// `limit=0` would come back as an empty list, indistinguishable from "nothing
// is due" -- so it is rejected instead.
func TestProviderEgressLocationDueRejectsBadLimit(t *testing.T) {
	const secret = "correct-operator-secret-0123456789"
	defer withStubOperatorIngestSecret(secret)()

	for _, raw := range []string{"0", "-1", "abc", "1.5"} {
		req := httptest.NewRequest(http.MethodGet, "/network/provider-egress-due?limit="+raw, nil)
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()

		ProviderEgressLocationDue(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("limit=%q: status = %d, want 400", raw, w.Code)
		}
	}
}
