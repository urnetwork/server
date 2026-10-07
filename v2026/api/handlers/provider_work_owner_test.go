// Public SDK enrollment is exercised through native HTTP writers and real SQL
// registration/custody, including process restart and registration cleanup.
package handlers

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	snprotocol "github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// Existing authenticated client identity and independently pinned registration
// authority are separate from the public SDK possession statement.
func providerWorkHttpOwnerFixture(t testing.TB) (model.StClientKeyRegistrationInput, protocol.OriginalWorkOwnerEnrollment, []byte) {
	t.Helper()
	clientId, networkId, userId, deviceId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	model.Testing_CreateNetwork(t.Context(), networkId, "work-owner-"+networkId.String(), userId)
	model.Testing_CreateDevice(t.Context(), networkId, deviceId, clientId, "work-owner", "test")
	root, err := crypto.HexToECDSA(strings.Repeat("14", 32))
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := crypto.HexToECDSA(strings.Repeat("15", 32))
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{125}, ed25519.SeedSize))
	deployment := "work-owner-http-test"
	input := model.StClientKeyRegistrationInput{Domain: snprotocol.ClientKeyHistoryDomain{ChainID: 945, GenesisHash: [32]byte{1}, Netuid: 521, Coordinator: common.Address{2}, SettlementVault: common.Address{3}, DeploymentIDHash: sha256.Sum256([]byte(deployment)), PolicyHash: [32]byte{4}, NoID: 1}, DeploymentID: deployment, ClientID: clientId, PublicKey: bytes.Clone(key.Public().(ed25519.PublicKey)), Boundary: snprotocol.ClientKeyEffectiveBoundary{Block: 100, Hash: [32]byte{5}}, RootKey: root, ArtifactKey: artifact, CreatedAt: time.Unix(1_800_000_000, 0).UTC()}
	domain, err := input.Domain.Digest()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := protocol.SignOriginalWorkOwnerEnrollment(t.Context(), protocol.OriginalWorkOwnerEnrollment{DomainHash: domain, ClientId: [16]byte(clientId), Generation: [16]byte(server.NewId())}, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := owner.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Vault.PushSimpleResource("provider_work.yml", []byte(fmt.Sprintf("schema: %s\ndomain_hash: %x\nrequest_public_key: %x\nclient_key_root_signer: %s\n", controller.ProviderWorkPolicySchema, domain, [32]byte{126}, crypto.PubkeyToAddress(root.PublicKey).Hex()))))
	return input, owner, raw
}

// The actual public endpoint retries pending registration and acknowledges
// only committed original bytes. A fresh handler then reads the same receipt.
func TestProviderWorkOwnerHttpEnrollmentRestartAndDiscovery(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		input, owner, raw := providerWorkHttpOwnerFixture(t)
		post := func(base string, body []byte) (int, []byte) {
			t.Helper()
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/provider-work/v1/owners", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			encoded, err := io.ReadAll(io.LimitReader(response.Body, 8*1024))
			if err != nil {
				t.Fatal(err)
			}
			return response.StatusCode, encoded
		}
		get := func(base, query string) (int, []byte) {
			t.Helper()
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/provider-work/v1/owners?"+query, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			encoded, err := io.ReadAll(io.LimitReader(response.Body, 16*1024))
			if err != nil {
				t.Fatal(err)
			}
			return response.StatusCode, encoded
		}
		first := httptest.NewServer(NewProviderWorkHandlers())
		defer first.Close()
		if status, _ := post(first.URL, raw); status != http.StatusServiceUnavailable {
			t.Fatal("missing original registration was not pending", status)
		}
		if _, err := model.StoreStClientKeyRegistration(t.Context(), input); err != nil {
			t.Fatal(err)
		}
		status, receiptRaw := post(first.URL, raw)
		var receipt protocol.OriginalWorkOwnerReceipt
		if status != http.StatusOK || json.Unmarshal(receiptRaw, &receipt) != nil || receipt.Schema != protocol.OriginalWorkOwnerReceiptSchema || receipt.OwnerHash != sha256.Sum256(raw) {
			t.Fatal("native HTTP enrollment lacks exact receipt", status, string(receiptRaw))
		}
		first.Close()
		second := httptest.NewServer(NewProviderWorkHandlers())
		defer second.Close()
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), `DELETE FROM network_client WHERE client_id=$1`, input.ClientID))
		})
		if status, retry := post(second.URL, raw); status != http.StatusOK || !bytes.Equal(retry, receiptRaw) {
			t.Fatal("restarted handler lost exact enrollment receipt", status)
		}
		query := fmt.Sprintf("domain=%x&client=%x&key=%x", owner.DomainHash, owner.ClientId, owner.PublicKey)
		status, indexRaw := get(second.URL, query)
		var index model.ProviderWorkOwnerIndex
		if status != http.StatusOK || json.Unmarshal(indexRaw, &index) != nil || index.Schema != model.ProviderWorkOwnerIndexSchema || len(index.Owners) != 1 || !bytes.Equal(index.Owners[0], raw) {
			t.Fatal("public discovery lost original SDK identity", status)
		}
		if status, exact := get(second.URL, query+fmt.Sprintf("&generation=%x", owner.Generation)); status != http.StatusOK || !bytes.Equal(exact, raw) {
			t.Fatal("exact owner read changed original signature", status)
		}
		if status, _ := get(second.URL, query+fmt.Sprintf("&generation=%x", [16]byte{127})); status != http.StatusNotFound {
			t.Fatal("unknown generation selected another owner", status)
		}
	})
}

// Selector spelling and native body limits are checked before SQL admission.
// An owner route cannot bypass the existing shared route-set operation budget.
func TestProviderWorkOwnerHttpBoundsAndExactSelectors(t *testing.T) {
	_, request := providerWorkHandlerFixture(t)
	handler := NewProviderWorkHandlers()
	for _, query := range []string{"domain=00", fmt.Sprintf("domain=%x&client=%x&key=%x&generation=", request.DomainHash, request.ClientId, request.PublicKey), fmt.Sprintf("domain=%x&client=%x&key=%x&generation=%x&generation=%x", request.DomainHash, request.ClientId, request.PublicKey, request.Generation, request.Generation), fmt.Sprintf("domain=%x&client=%x&key=%x&extra=1", request.DomainHash, request.ClientId, request.PublicKey)} {
		writer := &providerWorkDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/provider-work/v1/owners?"+query, nil))
		if writer.Code != http.StatusBadRequest || writer.writeDeadline.IsZero() {
			t.Fatal("owner selector admitted alternate identity", query, writer.Code)
		}
	}
	oversized := httptest.NewRequest(http.MethodPost, "/provider-work/v1/owners", bytes.NewReader([]byte("{}")))
	oversized.ContentLength = protocol.MaximumOriginalWorkOwnerBytes + 1
	oversized.Header.Set("Content-Type", "application/json")
	writer := &providerWorkDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(writer, oversized)
	if writer.Code != http.StatusRequestEntityTooLarge || !writer.readDeadline.IsZero() {
		t.Fatal("oversized enrollment entered body owner", writer.Code)
	}
	for range cap(handler.slots) {
		handler.slots <- struct{}{}
	}
	writer = &providerWorkDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/provider-work/v1/owners", nil))
	if writer.Code != http.StatusTooManyRequests {
		t.Fatal("SDK enrollment bypassed shared finite admission", writer.Code)
	}
	for range cap(handler.slots) {
		<-handler.slots
	}
}
