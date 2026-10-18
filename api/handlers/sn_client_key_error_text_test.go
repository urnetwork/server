// Neither client-key route sends a failure's own text. A chain read error,
// which names every configured RPC url, and operator, storage and signing
// failures are a fixed 503 the validator retries; a malformed request is a
// declared 400.
package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// One request, under a quota with room for it.
func snClientKeyErrorTextBudget() model.StAttemptUploadBudget {
	return model.StAttemptUploadBudget{RequestsPerHour: 1, BytesPerHour: protocol.MaxClientKeyHistoryResponseBytes, AccountRequestsPerHour: 1, AccountBytesPerHour: protocol.MaxClientKeyHistoryResponseBytes}
}

// A JSON-RPC endpoint that answers every call as chain 1. A release client
// configured for chain 945 refuses it with an error that names its url,
// including the url's private path.
func newSnClientKeyWrongChainRpc(t testing.TB) string {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			Id json.RawMessage `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			http.Error(w, "synthetic rpc refusal", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":"0x1"}`, call.Id)
	}))
	t.Cleanup(endpoint.Close)
	return endpoint.URL + "/synthetic-rpc-secret"
}

// A canonical observation request for clientId at a synthetic decision.
func snClientKeyObservationTestBody(t testing.TB, clientId server.Id) []byte {
	t.Helper()
	request, err := json.Marshal(protocol.ClientKeyObservationRequest{ClientID: [16]byte(clientId), ValidatorHotkey: [32]byte{6}, NativeBlock: 101, NativeHash: [32]byte{7}, NativeEpoch: 2, DecisionBoundary: protocol.ClientKeyEffectiveBoundary{Epoch: 1, Block: 100, Hash: [32]byte{5}}, Nonce: [32]byte{8}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(controller.SnClientKeyObservationArgs{ClientID: clientId, Request: request})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// Posts one observation through the production handler.
func postSnClientKeyObservation(t testing.TB, token string, body []byte) (int, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/sn/client-key/observation", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response := snAttemptUploadTestRecorder()
	SnClientKeyObservation(response, request)
	return response.Code, response.Body.String()
}

// The actual release client cannot read the decision boundary: the endpoint
// reports another chain. The read error, with the RPC url and its private
// path, was the 500 body.
func TestSnClientKeyObservationHidesRpcFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		url := newSnClientKeyWrongChainRpc(tb)
		root, err := crypto.HexToECDSA(strings.Repeat("12", 32))
		if err != nil {
			tb.Fatal(err)
		}
		artifact, err := crypto.HexToECDSA(strings.Repeat("13", 32))
		if err != nil {
			tb.Fatal(err)
		}
		cfg := &controller.StConfig{Enabled: true, ChainId: 945, GenesisHash: [32]byte{1}, Netuid: 521, ContractAddress: common.Address{2}, SettlementVault: common.Address{3}, DeploymentId: "client-key-error-text", PolicyHash: [32]byte{4}, NoId: 1, RootKey: root, ArtifactKey: artifact, RpcUrls: []string{url}, AttemptUploadBudget: snClientKeyErrorTextBudget()}
		controller.SetStConfig(cfg)
		tb.Cleanup(func() { controller.SetStConfig(nil) })
		tb.Cleanup(controller.Testing_SetCoreStClient(cfg))
		status, body := postSnClientKeyObservation(tb, client.Testing_Sign(), snClientKeyObservationTestBody(tb, *client.ClientId))
		requirePrivateUnavailable(tb, status, body, "synthetic-rpc-secret", "127.0.0.1", "chain id", "SnClientKeyObservation")
	})
}

// No release operator is configured. Its setup error was the 500 body.
func TestSnClientKeyObservationHidesOperatorFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		controller.SetStConfig(&controller.StConfig{Enabled: true, ChainId: 945, ContractAddress: [20]byte{2}, AttemptUploadBudget: snClientKeyErrorTextBudget()})
		tb.Cleanup(func() { controller.SetStConfig(nil) })
		status, body := postSnClientKeyObservation(tb, client.Testing_Sign(), snClientKeyObservationTestBody(tb, *client.ClientId))
		requirePrivateUnavailable(tb, status, body, "client-key authority", "operator")
	})
}

// The same, through the batch route.
func TestSnClientKeyObservationsHidesOperatorFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, identity := snAttemptUploadTestIdentity(tb)
		controller.SetStConfig(&controller.StConfig{Enabled: true, ChainId: 945, ContractAddress: [20]byte{2}, AttemptUploadBudget: snClientKeyErrorTextBudget()})
		tb.Cleanup(func() { controller.SetStConfig(nil) })
		request := snAttemptUploadTestRequest(tb, identity.Testing_Sign(), "metadata", snClientKeyBatchTestBody(tb, 1))
		request.URL.Path = "/sn/client-key/observations"
		response := &snClientKeyBatchTestRecorder{snAttemptUploadRecorder: snAttemptUploadTestRecorder()}
		SnClientKeyObservations(response, request)
		requirePrivateUnavailable(tb, response.Code, response.Body.String(), "client-key authority", "operator", "SnClientKeyObservations")
	})
}

// An observation request that is not its own canonical encoding is the
// caller's error. It declared no status, so it was a 500.
func TestSnClientKeyObservationRefusesNoncanonicalRequestAsBadRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		controller.SetStConfig(&controller.StConfig{Enabled: true, ChainId: 945, ContractAddress: [20]byte{2}, AttemptUploadBudget: snClientKeyErrorTextBudget()})
		tb.Cleanup(func() { controller.SetStConfig(nil) })
		body, err := json.Marshal(controller.SnClientKeyObservationArgs{ClientID: *client.ClientId, Request: []byte("{}")})
		if err != nil {
			tb.Fatal(err)
		}
		status, text := postSnClientKeyObservation(tb, client.Testing_Sign(), body)
		requireDeclaredAnswer(tb, status, text, http.StatusBadRequest, "client-key observation request is noncanonical or names another client")
	})
}

// A body that does not decode as observation arguments was answered 500 with
// the decoder's text, which names the Go type.
func TestSnClientKeyObservationRefusesUndecodableBodyAsBadRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		controller.SetStConfig(&controller.StConfig{Enabled: true, ChainId: 945, ContractAddress: [20]byte{2}, AttemptUploadBudget: snClientKeyErrorTextBudget()})
		tb.Cleanup(func() { controller.SetStConfig(nil) })
		status, text := postSnClientKeyObservation(tb, client.Testing_Sign(), []byte("[]"))
		requireDeclaredAnswer(tb, status, text, http.StatusBadRequest, "Client-key observation is invalid.", "json", "SnClientKeyObservationArgs")
	})
}
