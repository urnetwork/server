// Tests for the blackhole check ingest: the consecutive-failure rule across
// submissions, and the strict body.
package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Posts one batch of checks under secret.
func postBlackholeChecks(t testing.TB, secret string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %s", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/network/provider-blackhole-checks", bytes.NewReader(buf))
	req.Header.Set(operatorSecretHeader, secret)
	w := httptest.NewRecorder()
	SubmitProviderBlackholeChecks(w, req)
	return w
}

// Failures submitted one batch at a time count up on the provider's row, a
// check that measured nothing leaves the count and only reschedules, and a
// pass clears it.
func TestSubmitProviderBlackholeChecksCarriesTheCount(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()
		ctx := context.Background()
		clientId := server.NewId()
		start := server.NowUtc().Add(-50 * time.Minute).Truncate(time.Millisecond)
		check := func(at time.Duration, ok bool, failure string, notMeasured bool) {
			t.Helper()
			w := postBlackholeChecks(t, secret, map[string]any{
				"checks": []map[string]any{{
					"client_id":    clientId.String(),
					"ok":           ok,
					"failure":      failure,
					"not_measured": notMeasured,
					"checked_at":   start.Add(at),
				}},
			})
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
		}

		check(0, false, "all_destinations_failed", false)
		check(20*time.Minute, false, "all_destinations_failed", false)
		check(25*time.Minute, false, "not_measured", true)
		row := model.GetProviderBlackholeCheck(ctx, clientId)
		if row == nil || row.ConsecutiveFailures != 2 || row.FirstFailedAt == nil || !row.FirstFailedAt.Equal(start) || row.NextDueAt == nil {
			t.Fatalf("after two failures and a check that measured nothing: %+v", row)
		}
		check(40*time.Minute, false, "all_destinations_failed", false)
		row = model.GetProviderBlackholeCheck(ctx, clientId)
		if row.ConsecutiveFailures != 3 || !row.IsDark(server.NowUtc(), model.DefaultProviderEgressRules()) {
			t.Fatalf("three failures over forty minutes = %+v, want dark", row)
		}
		check(45*time.Minute, true, "", false)
		row = model.GetProviderBlackholeCheck(ctx, clientId)
		if row.ConsecutiveFailures != 0 || row.FirstFailedAt != nil || row.IsDark(server.NowUtc(), model.DefaultProviderEgressRules()) {
			t.Fatalf("a pass did not clear the count: %+v", row)
		}
	})
}

// A field this server does not know is refused rather than decoded to
// nothing: a check whose not_measured flag was dropped would be stored as a
// failure counted against the provider.
func TestSubmitProviderBlackholeChecksRefusesUnknownFields(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()
		clientId := server.NewId()
		w := postBlackholeChecks(t, secret, map[string]any{
			"checks": []map[string]any{{
				"client_id":   clientId.String(),
				"ok":          false,
				"failure":     "all_destinations_failed",
				"not_measure": true,
				"checked_at":  server.NowUtc(),
			}},
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for an unknown field", w.Code)
		}
		if row := model.GetProviderBlackholeCheck(context.Background(), clientId); row != nil {
			t.Fatalf("a refused batch stored a row: %+v", row)
		}
	})
}
