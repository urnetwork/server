package handlers

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		validator := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{86}, ed25519.SeedSize))
		nonce := bytes.Repeat([]byte{87}, connect.VerifyNonceSize)
		message, err := connect.BuildVerifySeedMessage(validator.Public().(ed25519.PublicKey), nonce, byte(connect.VerifyMMin))
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(controller.VerifyArgs{ClientId: server.NewId(), Vpk: validator.Public().(ed25519.PublicKey), ClientNonce: nonce, SeedSig: ed25519.Sign(validator, message), M: connect.VerifyMMin})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "https://operator.example/verify", bytes.NewReader(body))
		req.RemoteAddr = "192.0.2.45:4500"
		w := httptest.NewRecorder()
		Verify(w, req)
		requirePrivateUnavailable(t, w.Code, w.Body.String(), "st_client_key_head", "SQLSTATE")
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
