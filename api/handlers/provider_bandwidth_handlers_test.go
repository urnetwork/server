package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// --- download endpoint -------------------------------------------------------
//
// These tests touch no database: the endpoint only streams bytes, so they run
// outside DefaultTestEnv.

func TestProviderBandwidthTestRejectsMissingSecret(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/network/provider-bandwidth-test?bytes=1024", nil)
	w := httptest.NewRecorder()

	ProviderBandwidthTest(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the operator secret header is absent", w.Code)
	}
	if 0 < w.Body.Len() && w.Body.Len() != len("Unauthorized\n") {
		t.Fatalf("body length = %d, want no payload streamed to an unauthenticated caller", w.Body.Len())
	}
}

func TestProviderBandwidthTestRejectsWrongSecret(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/network/provider-bandwidth-test?bytes=1024", nil)
	req.Header.Set(operatorSecretHeader, "definitely-not-the-secret")
	w := httptest.NewRecorder()

	ProviderBandwidthTest(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on a wrong operator secret", w.Code)
	}
}

// TestProviderBandwidthTestRejectsAlteredSecret proves hmac.Equal is actually
// consulted once the vault is configured, rather than the endpoint accepting
// anything once secret != "".
func TestProviderBandwidthTestRejectsAlteredSecret(t *testing.T) {
	const secret = "correct-operator-secret-0123456789"
	const wrongSecret = "correct-operator-secret-0123456780" // last char changed
	defer withStubOperatorIngestSecret(secret)()

	req := httptest.NewRequest(http.MethodGet, "/network/provider-bandwidth-test?bytes=1024", nil)
	req.Header.Set(operatorSecretHeader, wrongSecret)
	w := httptest.NewRecorder()

	ProviderBandwidthTest(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the configured secret and the request's secret differ", w.Code)
	}
}

// TestProviderBandwidthTestStreamsRequestedByteCount is the accept-path test
// with teeth: a handler that authenticates correctly but streams nothing (or
// the wrong amount) fails here, and so would an unconditional 401.
func TestProviderBandwidthTestStreamsRequestedByteCount(t *testing.T) {
	const secret = "correct-operator-secret-0123456789"
	defer withStubOperatorIngestSecret(secret)()

	req := httptest.NewRequest(http.MethodGet, "/network/provider-bandwidth-test?bytes=1048576", nil)
	req.Header.Set(operatorSecretHeader, secret)
	w := httptest.NewRecorder()

	ProviderBandwidthTest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with the correct operator secret", w.Code)
	}
	if w.Body.Len() != 1048576 {
		t.Fatalf("streamed %d bytes, want exactly the requested 1048576", w.Body.Len())
	}
	if contentType := w.Header().Get("Content-Type"); contentType != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", contentType)
	}
	// httptest.NewRecorder does not enforce Content-Length, so a handler that
	// claims one size and writes another would otherwise pass the check above.
	assertContentLengthMatchesBody(t, w)
}

// TestProviderBandwidthTestClampsToMaximum: an unclamped stream is an
// open-ended resource commitment per request.
func TestProviderBandwidthTestClampsToMaximum(t *testing.T) {
	const secret = "correct-operator-secret-0123456789"
	defer withStubOperatorIngestSecret(secret)()

	// two orders of magnitude beyond the cap
	req := httptest.NewRequest(http.MethodGet, "/network/provider-bandwidth-test?bytes=536870912", nil)
	req.Header.Set(operatorSecretHeader, secret)
	w := httptest.NewRecorder()

	ProviderBandwidthTest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.Len() != maxProviderBandwidthTestBytes {
		t.Fatalf("streamed %d bytes for a 536870912-byte request, want it clamped to %d",
			w.Body.Len(), maxProviderBandwidthTestBytes)
	}
	assertContentLengthMatchesBody(t, w)
}

// TestProviderBandwidthTestDefaultsUnusableByteCounts: `bytes=0` and
// `bytes=-1` parse cleanly, so they are not caught by a malformed-input check.
// Streaming an empty body for them would hand the prober a zero-byte sample to
// divide by.
func TestProviderBandwidthTestDefaultsUnusableByteCounts(t *testing.T) {
	const secret = "correct-operator-secret-0123456789"
	defer withStubOperatorIngestSecret(secret)()

	for _, query := range []string{"", "?bytes=0", "?bytes=-1", "?bytes=not-a-number"} {
		req := httptest.NewRequest(http.MethodGet, "/network/provider-bandwidth-test"+query, nil)
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()

		ProviderBandwidthTest(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("query %q: status = %d, want 200", query, w.Code)
		}
		if w.Body.Len() != defaultProviderBandwidthTestBytes {
			t.Fatalf("query %q: streamed %d bytes, want the %d-byte default",
				query, w.Body.Len(), defaultProviderBandwidthTestBytes)
		}
	}
}

func assertContentLengthMatchesBody(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	contentLength := w.Header().Get("Content-Length")
	if contentLength == "" {
		return
	}
	declared, err := strconv.Atoi(contentLength)
	if err != nil {
		t.Fatalf("Content-Length = %q, not a number", contentLength)
	}
	if declared != w.Body.Len() {
		t.Fatalf("Content-Length = %d but %d bytes were written", declared, w.Body.Len())
	}
}

// --- result endpoint ---------------------------------------------------------

func TestProviderBandwidthResultRejectsMissingSecret(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"client_id":         "019f8835-158d-6fd8-e9dd-fd0e4c6d6792",
		"source":            model.ProviderBandwidthSourceActiveOperator,
		"bytes_per_second":  1000000.0,
		"sample_byte_count": 5242880,
	})
	req := httptest.NewRequest(http.MethodPost, "/network/provider-bandwidth-result", bytes.NewReader(body))
	w := httptest.NewRecorder()

	ProviderBandwidthResult(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the operator secret header is absent", w.Code)
	}
}

func TestProviderBandwidthResultRejectsWrongSecret(t *testing.T) {
	const secret = "correct-operator-secret-0123456789"
	const wrongSecret = "correct-operator-secret-0123456780"
	defer withStubOperatorIngestSecret(secret)()

	body, _ := json.Marshal(map[string]any{
		"client_id":         "019f8835-158d-6fd8-e9dd-fd0e4c6d6792",
		"source":            model.ProviderBandwidthSourceActiveOperator,
		"bytes_per_second":  1000000.0,
		"sample_byte_count": 5242880,
	})
	req := httptest.NewRequest(http.MethodPost, "/network/provider-bandwidth-result", bytes.NewReader(body))
	req.Header.Set(operatorSecretHeader, wrongSecret)
	w := httptest.NewRecorder()

	ProviderBandwidthResult(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on a wrong operator secret", w.Code)
	}
}

// TestProviderBandwidthResultRejectsNonPositiveMeasurement: a zero or negative
// measurement is not a usable sample, and storing one would overwrite a real
// figure with a meaningless one.
func TestProviderBandwidthResultRejectsNonPositiveMeasurement(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		ctx := context.Background()

		cases := []struct {
			name            string
			bytesPerSecond  float64
			sampleByteCount int64
		}{
			{"zero rate", 0, 5 * 1024 * 1024},
			{"negative rate", -1, 5 * 1024 * 1024},
			{"zero sample", 1000000, 0},
			{"negative sample", 1000000, -1},
		}
		for _, c := range cases {
			clientId := server.NewId()
			body, _ := json.Marshal(map[string]any{
				"client_id":         clientId,
				"source":            model.ProviderBandwidthSourceActiveOperator,
				"bytes_per_second":  c.bytesPerSecond,
				"sample_byte_count": c.sampleByteCount,
			})
			req := httptest.NewRequest(http.MethodPost, "/network/provider-bandwidth-result", bytes.NewReader(body))
			req.Header.Set(operatorSecretHeader, secret)
			w := httptest.NewRecorder()

			ProviderBandwidthResult(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: status = %d, want 400; body = %s", c.name, w.Code, w.Body.String())
			}
			if count := countProviderBandwidthRows(ctx, clientId); count != 0 {
				t.Fatalf("%s: %d provider_bandwidth rows written, want the submission rejected before storage", c.name, count)
			}
		}
	})
}

// TestProviderBandwidthResultStoresAnActiveMeasurement is the accept-path test
// with teeth: it reads provider_bandwidth back with raw SQL, so a handler that
// authenticates correctly but never calls model.StoreProviderBandwidth fails
// here -- as would an unconditional 401. Both active targets are covered,
// because the source now decides which row is written.
func TestProviderBandwidthResultStoresAnActiveMeasurement(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		ctx := context.Background()

		cases := []struct {
			name   string
			source string
		}{
			{"operator target", model.ProviderBandwidthSourceActiveOperator},
			{"cdn target", model.ProviderBandwidthSourceActiveCDN},
		}
		for _, c := range cases {
			clientId := server.NewId()
			const bytesPerSecond = 1234567.5
			const sampleByteCount int64 = 3 * 1024 * 1024

			body, _ := json.Marshal(map[string]any{
				"client_id":         clientId,
				"source":            c.source,
				"bytes_per_second":  bytesPerSecond,
				"sample_byte_count": sampleByteCount,
			})
			req := httptest.NewRequest(http.MethodPost, "/network/provider-bandwidth-result", bytes.NewReader(body))
			req.Header.Set(operatorSecretHeader, secret)
			w := httptest.NewRecorder()

			ProviderBandwidthResult(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200 for a valid submission; body = %s", c.name, w.Code, w.Body.String())
			}

			stored := readProviderBandwidthBySource(ctx, clientId)
			row, ok := stored[c.source]
			if !ok {
				t.Fatalf("%s: no provider_bandwidth row under source %q; the handler never stored the measurement", c.name, c.source)
			}
			if row.bytesPerSecond != bytesPerSecond {
				t.Fatalf("%s: bytes_per_second = %f, want the submitted %f", c.name, row.bytesPerSecond, bytesPerSecond)
			}
			if row.sampleByteCount != sampleByteCount {
				t.Fatalf("%s: sample_byte_count = %d, want the submitted %d", c.name, row.sampleByteCount, sampleByteCount)
			}
		}
	})
}

// TestProviderBandwidthResultRejectsUnknownSource: the source is part of the
// storage key, so it is validated rather than merely stored.
//
// An unrecognised value is not a harmless label -- it creates a row under a tag
// nothing will ever read or replace, while the submitter goes on looking like
// it is working. "passive" is refused for a sharper reason: that figure is
// derived server-side from bytes the provider has already been paid to carry,
// which is precisely what makes it ungameable, and accepting a submitted one
// would let this endpoint overwrite the derived figure with an asserted one.
func TestProviderBandwidthResultRejectsUnknownSource(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		ctx := context.Background()

		cases := []struct {
			name   string
			source any
		}{
			{"absent", nil},
			{"empty", ""},
			{"the pre-split active tag", "active"},
			{"a typo", "active-cnd"},
			{"wrong case", "ACTIVE-CDN"},
			{"server-derived passive", model.ProviderBandwidthSourcePassive},
		}
		for _, c := range cases {
			clientId := server.NewId()
			payload := map[string]any{
				"client_id":         clientId,
				"bytes_per_second":  1234567.5,
				"sample_byte_count": 3 * 1024 * 1024,
			}
			if c.source != nil {
				payload["source"] = c.source
			}
			body, _ := json.Marshal(payload)
			req := httptest.NewRequest(http.MethodPost, "/network/provider-bandwidth-result", bytes.NewReader(body))
			req.Header.Set(operatorSecretHeader, secret)
			w := httptest.NewRecorder()

			ProviderBandwidthResult(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s (source=%v): status = %d, want 400; body = %s", c.name, c.source, w.Code, w.Body.String())
			}
			if count := countProviderBandwidthRows(ctx, clientId); count != 0 {
				t.Fatalf("%s (source=%v): %d provider_bandwidth rows written, want the submission rejected before storage", c.name, c.source, count)
			}
		}
	})
}

// TestProviderBandwidthResultStoresTheTwoTargetsSeparately is the end-to-end
// form of the property the second target exists for: two submissions for ONE
// provider, one per target, must land in two rows carrying two figures.
//
// Against the old client_id-only key the second submission overwrote the
// first, so a provider prioritising the operator's own path while starving the
// public internet looked identical to one that was simply fast -- the exact
// divergence this is here to expose.
func TestProviderBandwidthResultStoresTheTwoTargetsSeparately(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		ctx := context.Background()
		clientId := server.NewId()

		submitted := map[string]float64{
			model.ProviderBandwidthSourceActiveOperator: 12_000_000,
			model.ProviderBandwidthSourceActiveCDN:      3_000_000,
		}
		for source, bytesPerSecond := range submitted {
			body, _ := json.Marshal(map[string]any{
				"client_id":         clientId,
				"source":            source,
				"bytes_per_second":  bytesPerSecond,
				"sample_byte_count": 5 * 1024 * 1024,
			})
			req := httptest.NewRequest(http.MethodPost, "/network/provider-bandwidth-result", bytes.NewReader(body))
			req.Header.Set(operatorSecretHeader, secret)
			w := httptest.NewRecorder()

			ProviderBandwidthResult(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("source %q: status = %d, want 200; body = %s", source, w.Code, w.Body.String())
			}
		}

		stored := readProviderBandwidthBySource(ctx, clientId)
		if len(stored) != 2 {
			t.Fatalf("%d provider_bandwidth rows for one provider, want 2 (one per target) -- the two targets are overwriting each other", len(stored))
		}
		for source, want := range submitted {
			row, ok := stored[source]
			if !ok {
				t.Fatalf("no row under source %q", source)
			}
			if row.bytesPerSecond != want {
				t.Errorf("source %q stored %.0f B/s, want the submitted %.0f -- the figures are not being kept apart (an averaged pair would read %.0f)",
					source, row.bytesPerSecond, want, (submitted[model.ProviderBandwidthSourceActiveOperator]+submitted[model.ProviderBandwidthSourceActiveCDN])/2)
			}
		}
	})
}

type storedProviderBandwidth struct {
	bytesPerSecond  float64
	sampleByteCount int64
}

// readProviderBandwidthBySource reads the rows back with raw SQL, keyed by
// source, so the assertions are against the table rather than against the
// writer that produced it.
func readProviderBandwidthBySource(ctx context.Context, clientId server.Id) map[string]storedProviderBandwidth {
	bySource := map[string]storedProviderBandwidth{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT source, bytes_per_second, sample_byte_count FROM provider_bandwidth WHERE client_id = $1`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var source string
				var row storedProviderBandwidth
				server.Raise(result.Scan(&source, &row.bytesPerSecond, &row.sampleByteCount))
				bySource[source] = row
			}
		})
	})
	return bySource
}

// TestProviderBandwidthPostsRejectMissingClientId: an absent client_id
// unmarshals to the zero id, which would otherwise be written as a real row
// keyed on the nil uuid.
func TestProviderBandwidthPostsRejectMissingClientId(t *testing.T) {
	const secret = "correct-operator-secret-0123456789"
	defer withStubOperatorIngestSecret(secret)()

	resultBody, _ := json.Marshal(map[string]any{
		"source":            model.ProviderBandwidthSourceActiveOperator,
		"bytes_per_second":  1000000.0,
		"sample_byte_count": 5242880,
	})
	reserveBody, _ := json.Marshal(map[string]any{
		"byte_count": 5 * 1024 * 1024,
	})

	cases := []struct {
		name    string
		path    string
		body    []byte
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"result", "/network/provider-bandwidth-result", resultBody, ProviderBandwidthResult},
		{"reserve", "/network/provider-bandwidth-reserve", reserveBody, ProviderBandwidthReserve},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, c.path, bytes.NewReader(c.body))
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()

		c.handler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 for a submission with no client id", c.name, w.Code)
		}
	}
}

func countProviderBandwidthRows(ctx context.Context, clientId server.Id) int {
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COUNT(*) FROM provider_bandwidth WHERE client_id = $1`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

// --- reservation endpoint ----------------------------------------------------

func TestProviderBandwidthReserveRejectsMissingSecret(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"client_id":  "019f8835-158d-6fd8-e9dd-fd0e4c6d6792",
		"byte_count": 5 * 1024 * 1024,
	})
	req := httptest.NewRequest(http.MethodPost, "/network/provider-bandwidth-reserve", bytes.NewReader(body))
	w := httptest.NewRecorder()

	ProviderBandwidthReserve(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the operator secret header is absent", w.Code)
	}
}

func TestProviderBandwidthReserveRejectsWrongSecret(t *testing.T) {
	const secret = "correct-operator-secret-0123456789"
	const wrongSecret = "correct-operator-secret-0123456780"
	defer withStubOperatorIngestSecret(secret)()

	body, _ := json.Marshal(map[string]any{
		"client_id":  "019f8835-158d-6fd8-e9dd-fd0e4c6d6792",
		"byte_count": 5 * 1024 * 1024,
	})
	req := httptest.NewRequest(http.MethodPost, "/network/provider-bandwidth-reserve", bytes.NewReader(body))
	req.Header.Set(operatorSecretHeader, wrongSecret)
	w := httptest.NewRecorder()

	ProviderBandwidthReserve(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on a wrong operator secret", w.Code)
	}
}

// TestProviderBandwidthReserveReservesBudget is the accept-path test with
// teeth: it asserts the ledger row exists, so a handler that authenticates and
// returns 200 without reserving anything fails -- as would an unconditional
// 401.
func TestProviderBandwidthReserveReservesBudget(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		ctx := context.Background()
		clientId := server.NewId()

		body, _ := json.Marshal(map[string]any{
			"client_id":  clientId,
			"byte_count": model.MaxProviderBandwidthBytesPerProbe,
		})
		req := httptest.NewRequest(http.MethodPost, "/network/provider-bandwidth-reserve", bytes.NewReader(body))
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()

		ProviderBandwidthReserve(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for a reservation against an empty budget; body = %s", w.Code, w.Body.String())
		}

		reservedCount, reservedBytes := providerBandwidthQuotaFor(ctx, clientId)
		if reservedCount != 1 {
			t.Fatalf("%d provider_bandwidth_quota rows, want exactly 1 -- the handler must actually reserve budget", reservedCount)
		}
		if reservedBytes != model.MaxProviderBandwidthBytesPerProbe {
			t.Fatalf("reserved %d bytes, want %d", reservedBytes, model.MaxProviderBandwidthBytesPerProbe)
		}

		var result ReserveProviderBandwidthResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode response: %s; body = %s", err, w.Body.String())
		}
		if result.ReservationId == (server.Id{}) {
			t.Fatal("response carried no reservation id")
		}
	})
}

// TestProviderBandwidthReserveClampsByteCount: the byte count is caller-
// supplied, so an oversized (or fat-fingered) request must not be able to
// consume a whole bucket in one reservation.
func TestProviderBandwidthReserveClampsByteCount(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		ctx := context.Background()
		clientId := server.NewId()

		body, _ := json.Marshal(map[string]any{
			"client_id":  clientId,
			"byte_count": 100 * model.MaxProviderBandwidthBytesPerProbe,
		})
		req := httptest.NewRequest(http.MethodPost, "/network/provider-bandwidth-reserve", bytes.NewReader(body))
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()

		ProviderBandwidthReserve(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		_, reservedBytes := providerBandwidthQuotaFor(ctx, clientId)
		if reservedBytes != model.MaxProviderBandwidthBytesPerProbe {
			t.Fatalf("reserved %d bytes for an oversized request, want it clamped to %d",
				reservedBytes, model.MaxProviderBandwidthBytesPerProbe)
		}
	})
}

func TestProviderBandwidthReserveRejectsNonPositiveByteCount(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		ctx := context.Background()
		for _, byteCount := range []int64{0, -1} {
			clientId := server.NewId()
			body, _ := json.Marshal(map[string]any{
				"client_id":  clientId,
				"byte_count": byteCount,
			})
			req := httptest.NewRequest(http.MethodPost, "/network/provider-bandwidth-reserve", bytes.NewReader(body))
			req.Header.Set(operatorSecretHeader, secret)
			w := httptest.NewRecorder()

			ProviderBandwidthReserve(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("byte_count %d: status = %d, want 400", byteCount, w.Code)
			}
			if count, _ := providerBandwidthQuotaFor(ctx, clientId); count != 0 {
				t.Fatalf("byte_count %d: %d quota rows written for a rejected request", byteCount, count)
			}
		}
	})
}

// TestProviderBandwidthReserveReturns429WhenCurrentBucketIsFull: the prober
// probes over an already-open tunnel right now, so a reservation deferred to a
// later hour is of no use to it -- the endpoint cancels that deferred
// reservation and answers 429, which the prober treats as "skip this
// provider". Leaving the deferred reservation in place would burn budget on
// every skipped provider.
func TestProviderBandwidthReserveReturns429WhenCurrentBucketIsFull(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()

		ctx := context.Background()
		// fill the current hourly bucket exactly to its ceiling
		bucketStart := server.NowUtc().UTC().Truncate(time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO provider_bandwidth_quota (provider_bandwidth_quota_id, client_id, byte_count, bucket_start, create_time)
				VALUES ($1, $2, $3, $4, $5)
				`,
				server.NewId(), server.NewId(), model.MaxProviderBandwidthBytesPerBucket(), bucketStart, server.NowUtc(),
			))
		})

		clientId := server.NewId()
		body, _ := json.Marshal(map[string]any{
			"client_id":  clientId,
			"byte_count": model.MaxProviderBandwidthBytesPerProbe,
		})
		req := httptest.NewRequest(http.MethodPost, "/network/provider-bandwidth-reserve", bytes.NewReader(body))
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()

		ProviderBandwidthReserve(w, req)

		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 when the current hourly bucket has no room; body = %s", w.Code, w.Body.String())
		}
		if count, _ := providerBandwidthQuotaFor(ctx, clientId); count != 0 {
			t.Fatalf("%d quota rows left behind for a 429'd request, want 0 -- a deferred reservation must be cancelled", count)
		}
		if retryAfter := w.Header().Get("Retry-After"); retryAfter == "" {
			t.Fatal("no Retry-After header on the 429; the prober cannot tell when budget frees up")
		}
	})
}

func providerBandwidthQuotaFor(ctx context.Context, clientId server.Id) (count int, byteCount int64) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COUNT(*), COALESCE(SUM(byte_count), 0)::bigint FROM provider_bandwidth_quota WHERE client_id = $1`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count, &byteCount))
			}
		})
	})
	return count, byteCount
}

// TestProviderBandwidthTestClampFitsInsideOneProbesReservation guards the one
// invariant that ties the download endpoint to the byte budget now that they
// are no longer the same number.
//
// A probe is not one request any more: it is bandwidth.StreamCount parallel
// streams, because a single TCP flow cannot exceed (connect's 1 MiB window /
// RTT) and the single-stream probe was measuring that ceiling rather than the
// provider. So maxProviderBandwidthTestBytes bounds ONE STREAM and
// model.MaxProviderBandwidthBytesPerProbe bounds the whole probe.
//
// They may drift apart, but only in one direction. If a single request could
// be served larger than a whole probe reserves, one request could spend more
// budget than was ever admitted for it -- the reservation would be a number
// the deployment routinely exceeds, which is worse than having no budget at
// all.
func TestProviderBandwidthTestClampFitsInsideOneProbesReservation(t *testing.T) {
	if model.MaxProviderBandwidthBytesPerProbe < int64(maxProviderBandwidthTestBytes) {
		t.Fatalf(
			"the download endpoint serves up to %d bytes in ONE request but a probe only reserves %d in total: "+
				"a single request could transfer more than the byte budget admitted for the whole probe",
			maxProviderBandwidthTestBytes, model.MaxProviderBandwidthBytesPerProbe,
		)
	}
}
