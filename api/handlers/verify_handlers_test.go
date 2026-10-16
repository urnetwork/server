package handlers

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// TestVerifyHandlersFailClosedWhileSubnetDisabled is the regression for main
// serving /verify/keys and /verify/stats without verify.yml.  st.yml explicitly
// disables the unreleased subsystem, so every route must stop before request
// parsing, vault key/settings loading, or database access and return a stable
// availability response instead of panicking into a 500.
func TestVerifyHandlersFailClosedWhileSubnetDisabled(t *testing.T) {
	controller.SetStConfig(&controller.StConfig{Enabled: false})
	t.Cleanup(func() { controller.SetStConfig(nil) })

	tests := []struct {
		name    string
		method  string
		target  string
		body    string
		handler http.HandlerFunc
	}{
		{name: "verify", method: http.MethodPost, target: "/verify", body: "not-json", handler: Verify},
		{name: "keys", method: http.MethodGet, target: "/verify/keys", handler: GetVerifyKeys},
		{name: "stats", method: http.MethodGet, target: "/verify/stats?from=not-a-time", handler: GetVerifyStats},
		{name: "proofs", method: http.MethodGet, target: "/verify/proofs?limit=not-a-number", handler: GetVerifyProofs},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
			w := httptest.NewRecorder()

			test.handler(w, req)

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d; body=%q", w.Code, http.StatusServiceUnavailable, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); got != "3600" {
				t.Fatalf("Retry-After = %q, want 3600", got)
			}
			if got := w.Body.String(); got != "Verification subsystem unavailable.\n" {
				t.Fatalf("body = %q", got)
			}
		})
	}
}

// Posts one verify step through the real handler from the given source.
func postVerify(t testing.TB, args controller.VerifyArgs, source string) (int, string) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "https://operator.example/verify", bytes.NewReader(encoded))
	req.RemoteAddr = source
	w := httptest.NewRecorder()
	Verify(w, req)
	return w.Code, w.Body.String()
}

// A signed seed under a synthetic validator key derived from one byte.
func verifySeedArgsFixture(t testing.TB, clientId server.Id, seed byte) controller.VerifyArgs {
	t.Helper()
	validator := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	nonce := bytes.Repeat([]byte{seed + 1}, connect.VerifyNonceSize)
	message, err := connect.BuildVerifySeedMessage(validator.Public().(ed25519.PublicKey), nonce, byte(connect.VerifyMMin))
	if err != nil {
		t.Fatal(err)
	}
	return controller.VerifyArgs{ClientId: clientId, Vpk: validator.Public().(ed25519.PublicKey), ClientNonce: nonce, SeedSig: ed25519.Sign(validator, message), M: connect.VerifyMMin}
}

// The registered-key lookup fails in storage, an unexpected failure that the
// seed step returned with the database's own text.
func TestVerifyHttpHidesStorageFailure(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		// The closure fixture's deployment, keys and settings serve every route.
		verifyClosureHttpFixture(t)
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), `ALTER TABLE st_client_key_head RENAME TO synthetic_unavailable_client_key_head`))
		})
		status, body := postVerify(t, verifySeedArgsFixture(t, server.NewId(), 86), "192.0.2.45:4500")
		requirePrivateUnavailable(t, status, body, "st_client_key_head", "SQLSTATE")
	})
}

// A stored proof whose hops are not JSON is corrupt storage, which the proof
// index returned with the trail id and the handler's name.
func TestVerifyProofsHttpHidesCorruptStoredProof(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		verifyClosureHttpFixture(t)
		trailId := server.NewId()
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO verify_trail (trail_id, vpk, server_key_id, server_nonce, depth, status, hops_json, create_time) VALUES ($1, $2, 7, $3, 1, 1, 'synthetic corrupt hops', $4)`, trailId, bytes.Repeat([]byte{88}, ed25519.PublicKeySize), bytes.Repeat([]byte{89}, 16), server.NowUtc().Add(-time.Minute)))
		})
		w := httptest.NewRecorder()
		GetVerifyProofs(w, httptest.NewRequest(http.MethodGet, "https://operator.example/verify/proofs", nil))
		requirePrivateUnavailable(t, w.Code, w.Body.String(), trailId.String(), "hops JSON", "GetVerifyProofs")
	})
}

// An invalid locator is the caller's error, documented as 400. It declared no
// status, so it was answered as an internal failure, 500.
func TestVerifyOriginalHttpRefusesInvalidLocatorAsBadRequest(t *testing.T) {
	controller.SetStConfig(&controller.StConfig{Enabled: true})
	t.Cleanup(func() { controller.SetStConfig(nil) })
	body, err := json.Marshal(model.VerifyOriginalRequest{Message: []byte("synthetic request"), Signature: make([]byte, ed25519.SignatureSize)})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	GetVerifyOriginalRequest(w, httptest.NewRequest(http.MethodPost, "https://operator.example/verify/original", bytes.NewReader(body)))
	if w.Code != http.StatusBadRequest || w.Body.String() != "invalid verification original request locator\n" {
		t.Fatalf("invalid locator was not refused as bad request: status=%d body=%q", w.Code, w.Body.String())
	}
}

// Closes the fixture's request through its own route and returns the closure.
func closeVerifyRequestFixture(t testing.TB) protocol.ProviderAttemptRequestClosure {
	t.Helper()
	raw, _ := verifyClosureHttpFixture(t)
	if status, body := postVerifyRequestClosure(t, NewVerifyRequestClosureHandlers(), raw); status != http.StatusOK {
		t.Fatalf("fixture request did not close: status=%d body=%q", status, body)
	}
	var closure protocol.ProviderAttemptRequestClosure
	if err := json.Unmarshal(raw, &closure); err != nil {
		t.Fatal(err)
	}
	return closure
}

// A closed request has no original and never will: its refusal is declared
// 409. It was raised from storage, so it reached the router as a panic.
func TestVerifyOriginalHttpAnswersClosedRequestWithConflict(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		closure := closeVerifyRequestFixture(t)
		body, err := json.Marshal(model.VerifyRequestClosureLocator(closure))
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		GetVerifyOriginalRequest(w, httptest.NewRequest(http.MethodPost, "https://operator.example/verify/original", bytes.NewReader(body)))
		if w.Code != http.StatusConflict || w.Body.String() != "verification request permanently closed\n" {
			t.Fatalf("closed request was not refused as a conflict: status=%d body=%q", w.Code, w.Body.String())
		}
	})
}

// Seeding a request its owner closed is refused the same way, from the same
// raised refusal.
func TestVerifyHttpAnswersClosedRequestWithConflict(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		closure := closeVerifyRequestFixture(t)
		vpk, err := protocol.VerifyProviderAttemptRequestWire(closure.Message, closure.RequestSignature)
		if err != nil {
			t.Fatal(err)
		}
		// The seed message is the context, its type, the key, the nonce and M.
		offset := len(connect.VerifyCtx) + 1 + ed25519.PublicKeySize
		status, body := postVerify(t, controller.VerifyArgs{ClientId: server.Id(closure.ClientId), Vpk: vpk[:], ClientNonce: closure.Message[offset : offset+connect.VerifyNonceSize], SeedSig: closure.RequestSignature, M: int(closure.Message[offset+connect.VerifyNonceSize])}, "192.0.2.46:4500")
		if status != http.StatusConflict || body != "verification request permanently closed\n" {
			t.Fatalf("closed request was not refused as a conflict: status=%d body=%q", status, body)
		}
	})
}

// A zero client id is malformed input. It passed the shape checks, reached
// the original-request index and raised there, a 500.
func TestVerifyHttpRefusesZeroClientIdAsBadRequest(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		verifyClosureHttpFixture(t)
		status, body := postVerify(t, verifySeedArgsFixture(t, server.Id{}, 90), "192.0.2.47:4500")
		if status != http.StatusBadRequest || body != "client_id must not be zero\n" {
			t.Fatalf("zero client id was not refused as bad request: status=%d body=%q", status, body)
		}
	})
}

// A broken simulator assignment filter is an unexpected configuration failure.
// The seed step declared it a 500 carrying the filter's own error.
func TestVerifyHttpHidesAssignmentFilterFailure(t *testing.T) {
	t.Setenv(controller.VerifySimulationAssignmentFilterFileEnv, "synthetic-relative-filter.json")
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		verifyClosureHttpFixture(t)
		ctx := t.Context()
		// A registered validator seeding from an eligible provider's egress is
		// the one seed that reads the filter.
		validatorId, providerId := server.NewId(), server.NewId()
		args := verifySeedArgsFixture(t, validatorId, 92)
		model.SetClientPublicKey(ctx, validatorId, args.Vpk)
		modes, err := json.Marshal([]model.ProvideMode{model.ProvideModePublic})
		if err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, fmt.Sprintf("{pm_%s}pms", providerId), modes, 0).Err())
		})
		model.FeedVerifyEgress(ctx, providerId, netip.MustParseAddr("198.51.100.30"), controller.VerifySettings())
		status, body := postVerify(t, args, "198.51.100.30:4500")
		requirePrivateUnavailable(t, status, body, "filter", "absolute")
	})
}

// A stored trail with a corrupt field cannot rebuild a replayed step's
// message. The step declared that a 400 carrying the builder's description of
// the stored state.
func TestVerifyHttpHidesCorruptStoredTrail(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		verifyClosureHttpFixture(t)
		status, body := postVerify(t, verifySeedArgsFixture(t, server.NewId(), 94), "192.0.2.48:4500")
		var assign connect.VerifyAssignResult
		if status != http.StatusOK || json.Unmarshal([]byte(body), &assign) != nil {
			t.Fatalf("fixture seed was not assigned: status=%d body=%q", status, body)
		}
		trailId := server.Id(assign.TrailId)
		server.Redis(t.Context(), func(r server.RedisClient) {
			server.Raise(r.HSet(t.Context(), fmt.Sprintf("{vtr_%s}h", trailId), "server_nonce", "synthetic-short-nonce").Err())
		})
		confirmed := make([]server.Id, len(assign.Trail))
		for index, hop := range assign.Trail {
			confirmed[index] = server.Id(hop)
		}
		status, body = postVerify(t, controller.VerifyArgs{TrailId: &trailId, Trail: confirmed, ExtendSig: make([]byte, ed25519.SignatureSize)}, "192.0.2.48:4500")
		requirePrivateUnavailable(t, status, body, "server_nonce", "bytes")
	})
}
