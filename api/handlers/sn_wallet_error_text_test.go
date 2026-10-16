// The wallet routes keep the 500 their clients pin. A refusal is answered
// with its own words alone, and any other failure, such as the operator's
// configuration or a chain read that names its RPC url, with a fixed message.
package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

// A complete synthetic deployment domain with distinct root and artifact
// signers, served by the given RPC urls.
func snWalletErrorTextConfig(t testing.TB, rpcUrls ...string) *controller.StConfig {
	t.Helper()
	root, err := crypto.HexToECDSA(strings.Repeat("21", 32))
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := crypto.HexToECDSA(strings.Repeat("22", 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &controller.StConfig{Enabled: true, ChainId: 945, GenesisHash: [32]byte{1}, Netuid: 521, ContractAddress: common.Address{2}, SettlementVault: common.Address{3}, DeploymentId: "wallet-error-text", PolicyHash: [32]byte{4}, NoId: 1, RootKey: root, ArtifactKey: artifact, RpcUrls: rpcUrls}
	controller.SetStConfig(cfg)
	t.Cleanup(func() { controller.SetStConfig(nil) })
	return cfg
}

// Serves one wallet route through its production handler.
func serveSnWalletRoute(handler http.HandlerFunc, method string, token string, args any) (int, string) {
	var body []byte
	if args != nil {
		body, _ = json.Marshal(args)
	}
	request := httptest.NewRequest(method, "https://operator.example/sn/wallet", bytes.NewReader(body))
	request.RemoteAddr = "192.0.2.50:4500"
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler(response, request)
	return response.Code, response.Body.String()
}

// A network's consent challenge for a valid synthetic coldkey.
func snWalletErrorTextChallenge(t testing.TB) controller.SnNetworkWalletMappingChallengeArgs {
	t.Helper()
	coldkey, err := ss58.Encode([32]byte{0x51}, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return controller.SnNetworkWalletMappingChallengeArgs{ColdkeySs58: coldkey}
}

// No release operator serves the deployment. Its setup error was the 500
// body of the network consent challenge.
func TestSnNetworkWalletMappingChallengeHidesOperatorFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		account, _ := snAttemptUploadTestIdentity(tb)
		snWalletErrorTextConfig(tb)
		status, body := serveSnWalletRoute(SnNetworkWalletMappingChallenge, http.MethodPost, account.Testing_Sign(), snWalletErrorTextChallenge(tb))
		requireDeclaredAnswer(tb, status, body, http.StatusInternalServerError, router.UnexpectedErrorMessage, "client-key authority", "operator")
	})
}

// The operator's boundary read fails: the endpoint reports another chain. The
// read error, with the RPC url and its private path, was the 500 body.
func TestSnNetworkWalletMappingChallengeHidesRpcFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		account, _ := snAttemptUploadTestIdentity(tb)
		cfg := snWalletErrorTextConfig(tb, newSnClientKeyWrongChainRpc(tb))
		tb.Cleanup(controller.Testing_SetCoreStClient(cfg))
		status, body := serveSnWalletRoute(SnNetworkWalletMappingChallenge, http.MethodPost, account.Testing_Sign(), snWalletErrorTextChallenge(tb))
		requireDeclaredAnswer(tb, status, body, http.StatusInternalServerError, router.UnexpectedErrorMessage, "synthetic-rpc-secret", "127.0.0.1", "chain id")
	})
}

// A coldkey that is not ss58 is refused as an integrity contradiction. The
// decoder's text was joined to the refusal's own words.
func TestSnWalletMappingChallengeHidesJoinedCause(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		_, client := snAttemptUploadTestIdentity(tb)
		snWalletErrorTextConfig(tb)
		status, body := serveSnWalletRoute(SnWalletMappingChallenge, http.MethodPost, client.Testing_Sign(), controller.SnWalletMappingChallengeArgs{ColdkeySs58: "synthetic-not-ss58"})
		requireDeclaredAnswer(tb, status, body, http.StatusInternalServerError, protocol.ErrWalletMappingIntegrity.Error(), "ss58", "base58")
	})
}

// A stored network consent whose original does not decode is refused as an
// integrity contradiction. The decoder's text and the controller's Go name
// were the 500 body.
func TestSnGetWalletHidesStoredRecordCause(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		account, _ := snAttemptUploadTestIdentity(tb)
		cfg := snWalletErrorTextConfig(tb)
		digest, err := protocol.ClientKeyHistoryDomain{ChainID: cfg.ChainId, GenesisHash: cfg.GenesisHash, Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress, SettlementVault: cfg.SettlementVault, DeploymentIDHash: sha256.Sum256([]byte(cfg.DeploymentId)), PolicyHash: cfg.PolicyHash, NoID: cfg.NoId}.Digest()
		if err != nil {
			tb.Fatal(err)
		}
		nonce, originalHash := sha256.Sum256([]byte("synthetic nonce")), sha256.Sum256([]byte("synthetic original"))
		server.Tx(tb.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO network_wallet_mapping_challenge(nonce,domain_hash,network_id,generation,expires_at,message) VALUES($1,$2,$3,1,$4,'synthetic challenge')`, nonce[:], digest[:], account.NetworkId, time.Now().Add(time.Hour).Unix()))
			server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO network_wallet_mapping_consent(domain_hash,network_id,generation,original_hash,nonce,original,accepted_at) VALUES($1,$2,1,$3,$4,'synthetic corrupt original',$5)`, digest[:], account.NetworkId, originalHash[:], nonce[:], server.NowUtc()))
		})
		status, body := serveSnWalletRoute(SnGetWallet, http.MethodGet, account.Testing_Sign(), nil)
		requireDeclaredAnswer(tb, status, body, http.StatusInternalServerError, protocol.ErrWalletMappingIntegrity.Error(), "invalid character", "SnGetWallet")
	})
}
