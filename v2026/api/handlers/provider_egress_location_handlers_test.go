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

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
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
			ClientId:   server.NewId(),
			ExitIp:     "192.0.2.1",
			ObservedAt: server.NowUtc(),
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

// Main's full shard selects 1000 providers and may request a bounded 5000-row
// lookahead after it has already selected a batch. The API fallback of 500
// silently clipped both phases; exercise the actual uncached startup reader.
func TestProviderEgressDueConfiguredCapAdmitsFullLookup(t *testing.T) {
	pop := server.Config.PushSimpleResource("provider_egress_due.yml", []byte("max_due_limit: 5000\n"))
	defer pop()
	if got := readMaxProviderEgressDueLimit(); got != 5000 {
		t.Fatalf("configured due ceiling = %d, want 5000", got)
	}
	if got := min(1000, readMaxProviderEgressDueLimit()); got != 1000 {
		t.Fatalf("full shard clipped to %d providers, want 1000", got)
	}
	if got := min(5000, readMaxProviderEgressDueLimit()); got != 5000 {
		t.Fatalf("bounded successor lookahead clipped to %d providers, want 5000", got)
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
		testing_connectDueProvider(t, ctx, due, city.LocationId, "192.0.2.1:0")
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
		if !slices.Contains(testDueClientIds(result), due) {
			t.Fatalf("providers = %+v, want it to contain the never-probed provider %s", result.Providers, due)
		}
		// the wire names the prober reads, and the place its sample is drawn for
		if !strings.Contains(w.Body.String(), `"providers"`) || !strings.Contains(w.Body.String(), `"client_id"`) {
			t.Fatalf("body = %s, want providers with client ids", w.Body.String())
		}
		for _, provider := range result.Providers {
			if provider.ClientId == due && (provider.CountryCode != "us" || provider.Region != "California") {
				t.Fatalf("due provider = %+v, want it placed at us/California", provider)
			}
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

		testing_connectDueProvider(t, ctx, server.NewId(), city.LocationId, "192.0.2.1:0")
		testing_connectDueProvider(t, ctx, server.NewId(), city.LocationId, "192.0.2.2:0")
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
		if len(result.Providers) != 1 {
			t.Fatalf("len(providers) = %d, want 1 for limit=1; body = %s", len(result.Providers), w.Body.String())
		}
	})
}

// The retained route now claims URL work, not stale location observations.
// Fresh legacy-only evidence cannot defer a provider; ten current URL successes
// can. A success older than four hours still counts in eight-hour ranking but
// cannot satisfy the rolling quota, even when the location is fresh.
func TestProviderEgressLocationDueHonoursStalenessCutoff(t *testing.T) {
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

		legacyFresh, complete, expired := server.NewId(), server.NewId(), server.NewId()
		testing_connectDueProvider(t, ctx, legacyFresh, city.LocationId, "192.0.2.1:0")
		testing_connectDueProvider(t, ctx, complete, city.LocationId, "192.0.2.2:0")
		testing_connectDueProvider(t, ctx, expired, city.LocationId, "192.0.2.3:0")
		model.UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

		now := server.NowUtc().Truncate(time.Microsecond)
		for clientId, observedAt := range map[server.Id]time.Time{
			legacyFresh: now,
			complete:    now.Add(-providerEgressDueAge - time.Hour),
			expired:     now,
		} {
			model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
				ClientId: clientId, LocationId: city.LocationId,
				CountryCode: "us", ObservedAt: observedAt,
			})
		}
		model.SetProviderEgressHealth(ctx, &model.ProviderEgressHealth{
			ClientId: legacyFresh, MeasuredAt: now,
			OKCount: 1, Total: 1,
		})

		// These providers were admitted before the synthetic measurement window.
		// Keep the stable token and independently paced deadline explicit.
		token := now.Add(-6 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			result, err := tx.Exec(ctx, `UPDATE provider_egress_probe_cycle
				SET cycle_started_at=$1,next_attempt_at=$2 WHERE client_id IN ($3,$4,$5)`,
				token, now.Add(-time.Hour), legacyFresh, complete, expired)
			server.Raise(err)
			if result.RowsAffected() != 3 {
				t.Fatal("fixture did not initialize every URL admission row")
			}
		})
		destination := egresshealth.Destination{Name: "synthetic-document", Class: egresshealth.ClassSite, Url: "https://document.example.invalid/"}
		for _, clientId := range []server.Id{complete, expired} {
			for index := range 10 {
				measuredAt := now.Add(-time.Hour + time.Duration(index)*time.Minute)
				if clientId == expired && index == 0 {
					measuredAt = now.Add(-model.ProviderEgressProbeRefreshAge - time.Minute)
				}
				evidence := &egresshealth.UrlProbeEvidence{
					PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
					Destination: destination, MeasuredAt: measuredAt, ContentMatcherVersion: 1,
					Security:   []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: measuredAt, TlsAuthenticated: true}},
					StatusCode: 200, ByteCount: 32, WireByteCount: 32, WireSampleByteCount: 31, BodyComplete: true,
					RequestWritten: true, FirstByteReceived: true, TtfbMillis: 10, BodyMillis: 1, BodyBitsPerSecond: 248000,
					ContentClassification: "content", PerformanceClassification: "insufficient_sample",
				}
				w := postEgressHealth(t, secret, map[string]any{
					"client_id": clientId, "run_id": server.NewId(), "cycle_started_at": token,
					"url_probe_evidence": evidence, "ok_count": 1, "total_count": 1,
					"class_results": map[string]any{"site": map[string]int{"ok": 1, "total": 1}},
				})
				if w.Code != http.StatusOK {
					t.Fatalf("synthetic URL success rejected: status=%d body=%s", w.Code, w.Body.String())
				}
			}
		}
		if counts := model.GetAllProviderEgressHealthCounts(ctx)[expired]; counts.OKCount != 10 || counts.Total != 10 {
			t.Fatal("the expired quota success must still be valid eight-hour ranking evidence")
		}

		req := httptest.NewRequest(http.MethodGet, "/network/provider-egress-due?limit=100", nil)
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()

		ProviderEgressLocationDue(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		var result struct {
			Providers []model.ProviderUrlProbeDue `json:"providers"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode body %q: %s", w.Body.String(), err)
		}
		byId := map[server.Id]model.ProviderUrlProbeDue{}
		for _, provider := range result.Providers {
			byId[provider.ClientId] = provider
			if !provider.CycleStartedAt.Equal(token) || provider.CountryCode != "us" || provider.Region != "California" {
				t.Fatal("URL due response lost its durable admission token or provider place")
			}
		}
		if len(result.Providers) != 2 || len(byId) != 2 {
			t.Fatalf("URL due response count=%d want=2 distinct incomplete providers", len(result.Providers))
		}
		if _, included := byId[complete]; included {
			t.Fatal("ten current URL successes must defer a provider despite its stale location")
		}
		if provider, included := byId[legacyFresh]; !included || provider.RunsNeeded != 10 || provider.OutcomeCount != 0 {
			t.Fatal("fresh legacy-only evidence must remain due for ten URL successes")
		}
		if provider, included := byId[expired]; !included || provider.RunsNeeded != 1 || provider.OutcomeCount != 10 {
			t.Fatal("nine current URL successes plus one expired success must remain due for one")
		}
		if got := due(t, secret); len(got) != 0 {
			t.Fatal("an immediate repeated poll must not duplicate active URL claims")
		}
	})
}

func TestProviderEgressLocationAttemptRejectsMissingSecret(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"client_id": "019f8835-158d-6fd8-e9dd-fd0e4c6d6792",
	})
	req := httptest.NewRequest(http.MethodPost, "/network/provider-egress-attempt", bytes.NewReader(body))
	w := httptest.NewRecorder()

	ProviderEgressLocationAttempt(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the operator secret header is absent", w.Code)
	}
}

// The reject case with the vault *configured*, so the request gets past the
// secret == "" fail-closed short-circuit and hmac.Equal is what rejects.
func TestProviderEgressLocationAttemptRejectsAlteredSecret(t *testing.T) {
	const secret = "correct-operator-secret-0123456789"
	const wrongSecret = "correct-operator-secret-0123456780" // last char changed
	defer withStubOperatorIngestSecret(secret)()

	body, _ := json.Marshal(map[string]any{
		"client_id": "019f8835-158d-6fd8-e9dd-fd0e4c6d6792",
	})
	req := httptest.NewRequest(http.MethodPost, "/network/provider-egress-attempt", bytes.NewReader(body))
	req.Header.Set(operatorSecretHeader, wrongSecret)
	w := httptest.NewRecorder()

	ProviderEgressLocationAttempt(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the operator secret is configured but the request's secret is wrong", w.Code)
	}
}

// The whole point of the attempt endpoint, end to end over http: a provider
// that has never been probed successfully is due; the prober reports that it
// tried and failed; the provider stops being due. Without that, a provider
// whose probes always fail sits at the head of the queue on every poll forever
// (observed_at IS NULL sorts first) and starves every provider behind it.
func TestProviderEgressLocationAttemptDefersProvider(t *testing.T) {
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

		dead := server.NewId()
		testing_connectDueProvider(t, ctx, dead, city.LocationId, "192.0.2.1:0")
		model.UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

		if !slices.Contains(due(t, secret), dead) {
			t.Fatalf("the never-probed provider %s must be due before any attempt is reported", dead)
		}

		attemptBody, err := json.Marshal(controller.RecordProviderEgressProbeAttemptArgs{
			ClientId:     dead,
			ProbeFailure: "tunnel_failed",
		})
		if err != nil {
			t.Fatalf("marshal attempt: %s", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/network/provider-egress-attempt", bytes.NewReader(attemptBody))
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()

		ProviderEgressLocationAttempt(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}

		attempt := model.GetProviderEgressProbeAttempt(ctx, dead)
		if attempt == nil {
			t.Fatal("expected the attempt to be recorded")
		}
		if attempt.ProbeFailure != "tunnel_failed" {
			t.Fatalf("probe_failure = %q, want %q", attempt.ProbeFailure, "tunnel_failed")
		}

		if slices.Contains(due(t, secret), dead) {
			t.Fatalf("the provider %s must not be due again immediately after a failed attempt", dead)
		}
	})
}

// due drives the due endpoint over http and returns the batch.
func due(t testing.TB, secret string) []server.Id {
	req := httptest.NewRequest(http.MethodGet, "/network/provider-egress-due?limit=100", nil)
	req.Header.Set(operatorSecretHeader, secret)
	w := httptest.NewRecorder()

	ProviderEgressLocationDue(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("due: status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var result ProviderEgressLocationDueResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("due: decode body %q: %s", w.Body.String(), err)
	}
	return testDueClientIds(result)
}

// The client ids of a due result, in order.
func testDueClientIds(result ProviderEgressLocationDueResult) []server.Id {
	clientIds := []server.Id{}
	for _, provider := range result.Providers {
		clientIds = append(clientIds, provider.ClientId)
	}
	return clientIds
}

// An unknown client id must be rejected rather than writing an attempt row
// keyed to a client that does not exist, which nothing would ever read.
func TestProviderEgressLocationAttemptRejectsUnknownClient(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		body, err := json.Marshal(controller.RecordProviderEgressProbeAttemptArgs{
			ClientId:     server.NewId(),
			ProbeFailure: "tunnel_failed",
		})
		if err != nil {
			t.Fatalf("marshal attempt: %s", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/network/provider-egress-attempt", bytes.NewReader(body))
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()

		ProviderEgressLocationAttempt(w, req)

		if w.Code == http.StatusUnauthorized {
			t.Fatalf("status = %d, want the correct secret to clear auth (not 401)", w.Code)
		}
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for an unregistered client id; body = %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "Unknown client.") {
			t.Fatalf("body = %q, want it to report the unknown client", w.Body.String())
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
