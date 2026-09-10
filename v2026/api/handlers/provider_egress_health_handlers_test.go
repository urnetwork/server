package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// validEgressHealthBody is a well-formed submission: the four scored classes
// sum to exactly ok_count/total_count, and the reputation figures sit outside
// them. Tests mutate a copy of this to exercise one rule at a time.
func validEgressHealthBody(clientId server.Id) map[string]any {
	return map[string]any{
		"client_id":   clientId.String(),
		"ok_count":    25,
		"total_count": 26,
		"class_results": map[string]any{
			"dns":          map[string]any{"ok": 4, "total": 4},
			"connectivity": map[string]any{"ok": 5, "total": 5},
			"cdn":          map[string]any{"ok": 4, "total": 5},
			"site":         map[string]any{"ok": 12, "total": 12},
		},
		"reputation_ok":              1,
		"reputation_total":           4,
		"failed_names":               "cachefly",
		"reputation_failed_names":    "akamai,etsy,canva",
		"tls_authentication_failure": false,
	}
}

func postEgressHealth(t testing.TB, secret string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %s", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/network/provider-egress-health", bytes.NewReader(buf))
	if secret != "" {
		req.Header.Set(operatorSecretHeader, secret)
	}
	w := httptest.NewRecorder()
	ProviderEgressHealthResult(w, req)
	return w
}

func TestProviderEgressHealthResultRejectsMissingSecret(t *testing.T) {
	w := postEgressHealth(t, "", validEgressHealthBody(server.NewId()))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the operator secret header is absent", w.Code)
	}
}

// TestProviderEgressHealthResultRejectsWrongSecret proves hmac.Equal is
// actually consulted once the vault IS configured. Without a configured
// secret the handler takes the secret=="" short-circuit, and the missing-header
// test above would pass even if the comparison were never made.
func TestProviderEgressHealthResultRejectsWrongSecret(t *testing.T) {
	const secret = "correct-operator-secret-0123456789"
	const wrongSecret = "correct-operator-secret-0123456780" // last char changed
	defer withStubOperatorIngestSecret(secret)()

	w := postEgressHealth(t, wrongSecret, validEgressHealthBody(server.NewId()))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the configured secret and the request's secret differ", w.Code)
	}
}

// TestProviderEgressHealthResultValidationRejections walks every rule that
// must fire BEFORE anything is written. The row is an upsert keyed on
// client_id, so an accepted bad submission does not sit beside the good one --
// it destroys the last good measurement for that provider.
//
// Each case asserts no row was stored, not merely that the status was 400: a
// store-then-flag implementation would pass a status-only assertion.
func TestProviderEgressHealthResultValidationRejections(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()
		ctx := context.Background()

		cases := []struct {
			name   string
			mutate func(body map[string]any)
		}{
			{
				// the headline rule: more destinations passed than ran
				name: "ok_count exceeds total_count",
				mutate: func(body map[string]any) {
					body["ok_count"] = 27
					body["total_count"] = 26
					body["class_results"] = map[string]any{
						"dns": map[string]any{"ok": 27, "total": 26},
					}
				},
			},
			{
				name: "class results do not sum to the totals",
				mutate: func(body map[string]any) {
					// drops the site class from the map but not from the total
					body["class_results"] = map[string]any{
						"dns":          map[string]any{"ok": 4, "total": 4},
						"connectivity": map[string]any{"ok": 5, "total": 5},
						"cdn":          map[string]any{"ok": 4, "total": 5},
					}
				},
			},
			{
				// {ok:5,total:2} + {ok:0,total:3} sums to a consistent 5/5
				// while describing a class where more passed than ran, so the
				// aggregate check alone does not catch this
				name: "a single class has ok above total while the sums agree",
				mutate: func(body map[string]any) {
					body["ok_count"] = 5
					body["total_count"] = 5
					body["class_results"] = map[string]any{
						"dns":  map[string]any{"ok": 5, "total": 2},
						"site": map[string]any{"ok": 0, "total": 3},
					}
				},
			},
			{
				// reputation is not health: it must never arrive as a scored
				// class, because the only ways to accept it are to break the
				// sum check or to fold it into the score
				name: "reputation smuggled in as a scored class",
				mutate: func(body map[string]any) {
					body["ok_count"] = 26
					body["total_count"] = 30
					body["class_results"] = map[string]any{
						"dns":          map[string]any{"ok": 4, "total": 4},
						"connectivity": map[string]any{"ok": 5, "total": 5},
						"cdn":          map[string]any{"ok": 4, "total": 5},
						"site":         map[string]any{"ok": 12, "total": 12},
						"reputation":   map[string]any{"ok": 1, "total": 4},
					}
				},
			},
			{
				name: "unknown field",
				mutate: func(body map[string]any) {
					body["okay_count"] = 25
				},
			},
			{
				name: "negative count",
				mutate: func(body map[string]any) {
					body["ok_count"] = -1
					body["total_count"] = -1
					body["class_results"] = map[string]any{}
				},
			},
			{
				name: "negative reputation count",
				mutate: func(body map[string]any) {
					body["reputation_ok"] = -1
				},
			},
			{
				name: "reputation ok exceeds reputation total",
				mutate: func(body map[string]any) {
					body["reputation_ok"] = 5
					body["reputation_total"] = 4
				},
			},
			{
				name: "negative class count",
				mutate: func(body map[string]any) {
					body["ok_count"] = 0
					body["total_count"] = -1
					body["class_results"] = map[string]any{
						"dns": map[string]any{"ok": 0, "total": -1},
					}
				},
			},
			{
				name: "missing client id",
				mutate: func(body map[string]any) {
					delete(body, "client_id")
				},
			},
		}

		for _, c := range cases {
			// a fresh client id per case, so "no row stored" is unambiguous
			clientId := server.NewId()
			body := validEgressHealthBody(clientId)
			c.mutate(body)

			w := postEgressHealth(t, secret, body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400; body = %s", c.name, w.Code, w.Body.String())
				continue
			}
			if health := model.GetProviderEgressHealth(ctx, clientId); health != nil {
				t.Errorf("%s: a rejected submission stored a row anyway: %+v", c.name, health)
			}
		}
	})
}

// TestProviderEgressHealthResultRejectsOKAboveTotal pins the top-level
// ok_count <= total_count rule specifically.
//
// It asserts the REASON, not just the 400, and that is the whole point of the
// test. Under the full rule set the aggregate check is defence in depth: any
// payload with ok_count > total_count whose classes sum exactly to those two
// figures must contain a class with ok > total, so removing the aggregate
// check still gets a 400 -- from the per-class rule, describing a different
// invariant. A status-only assertion therefore cannot tell the two apart and
// would pass with the rule deleted.
//
// The rule earns its place by being checked BEFORE the payload is decomposed:
// it is a statement about the submission as a whole, it holds for any future
// class set, and it fires whether or not the class map agrees with itself.
func TestProviderEgressHealthResultRejectsOKAboveTotal(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()
		ctx := context.Background()

		clientId := server.NewId()
		body := validEgressHealthBody(clientId)
		body["ok_count"] = 27
		body["total_count"] = 26
		body["class_results"] = map[string]any{
			"dns": map[string]any{"ok": 27, "total": 26},
		}

		w := postEgressHealth(t, secret, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 when ok_count exceeds total_count; body = %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "ok_count must not exceed total_count") {
			t.Fatalf("body = %q, want the rejection to come from the ok_count <= total_count rule", strings.TrimSpace(w.Body.String()))
		}
		if health := model.GetProviderEgressHealth(ctx, clientId); health != nil {
			t.Fatalf("a rejected submission stored a row anyway: %+v", health)
		}
	})
}

// TestProviderEgressHealthResultStoresAValidRun is the accept path: a correct
// secret clears auth, a consistent payload passes validation, and the row
// lands with the reputation figures stored beside the health figures rather
// than inside them.
func TestProviderEgressHealthResultStoresAValidRun(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()
		ctx := context.Background()

		clientId := server.NewId()
		w := postEgressHealth(t, secret, validEgressHealthBody(clientId))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}

		health := model.GetProviderEgressHealth(ctx, clientId)
		if health == nil {
			t.Fatal("a 200 stored no row")
		}
		if health.OKCount != 25 || health.Total != 26 {
			t.Errorf("ok/total = %d/%d, want 25/26", health.OKCount, health.Total)
		}
		// reputation stored...
		if health.ReputationOK != 1 || health.ReputationTotal != 4 {
			t.Errorf("reputation = %d/%d, want 1/4", health.ReputationOK, health.ReputationTotal)
		}
		if health.ReputationFailedNames != "akamai,etsy,canva" {
			t.Errorf("reputation_failed_names = %q", health.ReputationFailedNames)
		}
		// ...and excluded from the health figures. 26/30 would be the shape of
		// a reputation-folded-in regression.
		if health.OKCount == 26 || health.Total == 30 {
			t.Errorf("reputation was folded into ok/total: %d/%d", health.OKCount, health.Total)
		}
		if _, present := health.ClassResults["reputation"]; present {
			t.Error("reputation was stored as a scored class")
		}
		if health.FailedNames != "cachefly" {
			t.Errorf("failed_names = %q, want the scored failures only", health.FailedNames)
		}
		if health.TLSAuthenticationFailure {
			t.Error("tls_authentication_failure = true for a payload that explicitly sent false")
		}
		if got := health.ClassResults["cdn"]; got.OK != 4 || got.Total != 5 {
			t.Errorf("class_results[cdn] = %+v, want 4/5", got)
		}
		if len(health.ClassResults) != 4 {
			t.Errorf("class_results has %d classes, want 4", len(health.ClassResults))
		}
		if health.MeasuredAt.IsZero() {
			t.Error("measured_at was not stamped on arrival")
		}
	})
}

// One forged certificate must survive the strict JSON boundary and storage as
// a first-class hard failure. Treating it as merely one failed destination
// lets a high aggregate score advertise the intercepting provider.
func TestProviderEgressHealthResultStoresTLSAuthenticationFailure(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		clientId := server.NewId()
		body := validEgressHealthBody(clientId)
		body["tls_authentication_failure"] = true
		w := postEgressHealth(t, secret, body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}

		health := model.GetProviderEgressHealth(context.Background(), clientId)
		if health == nil || !health.TLSAuthenticationFailure {
			t.Fatalf("stored health = %+v, want TLSAuthenticationFailure=true", health)
		}
	})
}
