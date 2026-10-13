// The real HTTP handler owns framing, native deadlines and durable receipt
// delivery. Fresh route instances recover exactly the first committed bytes.
package handlers

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// Synthetic independent request and historical server keys need no live signer.
func verifyClosureHttpFixture(t testing.TB) ([]byte, *controller.VerifyServerKey) {
	t.Helper()
	cfg := &controller.StConfig{Enabled: true, Profile: "testnet", ChainId: 945, GenesisHash: [32]byte{1}, ContractAddress: common.Address{1}, DeploymentId: "closure-http-test", PolicyHash: [32]byte{2}, Netuid: 521, NoId: 1}
	controller.SetStConfig(cfg)
	key := &controller.VerifyServerKey{ServerKeyId: 7, PrivateKey: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{81}, ed25519.SeedSize))}
	controller.SetVerifyServerKeys([]*controller.VerifyServerKey{key})
	controller.SetVerifySettings(model.DefaultVerifySettings())
	t.Cleanup(func() {
		controller.SetStConfig(nil)
		controller.SetVerifyServerKeys(nil)
		controller.SetVerifySettings(nil)
	})
	owner := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{82}, ed25519.SeedSize))
	message, err := connect.BuildVerifySeedMessage(owner.Public().(ed25519.PublicKey), bytes.Repeat([]byte{83}, connect.VerifyNonceSize), byte(connect.VerifyMMin))
	if err != nil {
		t.Fatal(err)
	}
	closure, err := protocol.SealProviderAttemptRequestClosure(t.Context(), protocol.ProviderAttemptRequestClosure{Schema: protocol.ProviderAttemptRequestCloseDomain, Scope: protocol.ProviderAttemptReceiptScope{Profile: cfg.Profile, GenesisHash: cfg.GenesisHash, DeploymentId: cfg.DeploymentId, DeploymentKey: string(cfg.DeploymentKey()), PolicyHash: cfg.PolicyHash, Netuid: cfg.Netuid, NoId: cfg.NoId}, ClientId: connect.Id(server.NewId()), Message: message, RequestSignature: ed25519.Sign(owner, message), CutHash: [32]byte{3}, Epoch: 7, EndBlock: 20}, owner)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(closure)
	if err != nil {
		t.Fatal(err)
	}
	return raw, key
}

// The same original survives actual connection loss, fresh handler state and
// server signing-key rotation; no database current-client row is required.
func TestVerifyRequestClosureHttpRestartKeepsExactReceipt(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		raw, key := verifyClosureHttpFixture(t)
		post := func(url string, body []byte) (int, []byte) {
			t.Helper()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/verify/original/close", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-UR-Forwarded-For", "192.0.2.44:4500")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			encoded, readErr := io.ReadAll(io.LimitReader(response.Body, 8193))
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil {
				t.Fatal(readErr, closeErr)
			}
			return response.StatusCode, encoded
		}
		first := httptest.NewServer(NewVerifyRequestClosureHandlers())
		defer first.Close()
		status, receipt := post(first.URL, raw)
		var result model.VerifyRequestClosureResult
		if status != http.StatusOK || json.Unmarshal(receipt, &result) != nil || result.ClosedUnreceived == nil || result.Original != nil {
			t.Fatal("actual HTTP closure did not deliver committed receipt", status, string(receipt))
		}
		first.Close()
		controller.SetVerifyServerKeys([]*controller.VerifyServerKey{{ServerKeyId: 8, PrivateKey: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{84}, ed25519.SeedSize))}, key})
		second := httptest.NewServer(NewVerifyRequestClosureHandlers())
		defer second.Close()
		status, retry := post(second.URL, raw)
		if status != http.StatusOK || !bytes.Equal(retry, receipt) {
			t.Fatal("fresh HTTP owner replaced original closure receipt", status, string(retry))
		}
		status, _ = post(second.URL, append(bytes.Clone(raw), '\n'))
		if status != http.StatusBadRequest {
			t.Fatal("noncanonical closure reached durable intake", status)
		}
	})
}

// Framing refusal occurs before body I/O; a full route instance cannot consume
// another API lifecycle's independent admission allowance.
func TestVerifyRequestClosureHttpBoundsAndOwnerIsolation(t *testing.T) {
	controller.SetStConfig(&controller.StConfig{Enabled: true})
	defer controller.SetStConfig(nil)
	busy := NewVerifyRequestClosureHandlers()
	for index := 0; index < cap(busy.slots); index++ {
		busy.slots <- struct{}{}
	}
	req := httptest.NewRequest(http.MethodPost, "https://operator.example/verify/original/close", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	busy.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatal("busy closure owner entered body read", w.Code)
	}
	other := NewVerifyRequestClosureHandlers()
	req = httptest.NewRequest(http.MethodPost, "https://operator.example/verify/original/close", nil)
	req.ContentLength = 8193
	w = httptest.NewRecorder()
	other.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatal("independent closure owner lost framing admission", w.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "https://operator.example/verify/original/close?extra=1", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	other.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatal("unexpected closure query reached body read", w.Code)
	}
}
