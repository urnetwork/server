// Real durable intents and actual HTTP receipt decoding expose receipt
// substitutions before the account reconciler is allowed to mutate storage.
package controller

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The requested transaction is genuinely signed and stored. Only the RPC
// receipt is substituted, so the original consumer can falsely finalize it.
func stReceiptIdentityObservation(t testing.TB, fault string) (bool, bool, string, error, *model.StTransactionIntent, *model.StTransactionAttempt) {
	t.Helper()
	ctx := context.Background()
	cfg := stTransactionReconcileConfig()
	key, err := crypto.ToECDSA(crypto.Keccak256([]byte("inert receipt-identity fixture/" + fault)))
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	to := cfg.ContractAddress
	calldata := []byte{1, 2, 3}
	intent := model.ReserveStTransactionIntent(ctx, "receipt-identity-"+fault, "testnet", cfg.DeploymentId, cfg.DeploymentKey(), cfg.ChainId, "0x"+hex.EncodeToString(cfg.GenesisHash[:]), strings.ToLower(from.Hex()), strings.ToLower(to.Hex()), crypto.Keccak256Hash(calldata).Hex(), calldata, 7)
	transaction, err := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: 7, To: &to, Gas: 50000, GasPrice: big.NewInt(100), Value: new(big.Int), Data: calldata}), types.LatestSignerForChainID(big.NewInt(945)), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := transaction.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	price := "100"
	attempt := model.AddStTransactionAttempt(ctx, &model.StTransactionAttempt{IntentId: intent.IntentId, Attempt: 1, Kind: model.StTxAttemptExecution, TxHash: strings.ToLower(transaction.Hash().Hex()), RawTransaction: raw, GasLimit: transaction.Gas(), GasPrice: &price})
	if attempt == nil {
		t.Fatal("actual signed attempt was not stored")
	}
	model.MarkStTransactionBroadcast(ctx, intent.IntentId, attempt.Attempt)
	receipt := &types.Receipt{Type: transaction.Type(), Status: types.ReceiptStatusSuccessful, TxHash: transaction.Hash(), BlockHash: stTransactionReconcileBlockHash(99), BlockNumber: big.NewInt(99), GasUsed: transaction.Gas(), CumulativeGasUsed: transaction.Gas(), EffectiveGasPrice: transaction.GasPrice(), Logs: []*types.Log{}}
	switch fault {
	case "transaction":
		receipt.TxHash[0] ^= 1
	case "zero-transaction":
		receipt.TxHash = common.Hash{}
	case "overflow":
		receipt.BlockNumber = new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(99))
	case "signed-overflow":
		receipt.BlockNumber = new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 63), big.NewInt(99))
	case "zero-number":
		receipt.BlockNumber = new(big.Int)
		receipt.BlockHash = stTransactionReconcileBlockHash(0)
	case "missing-number":
		receipt.BlockNumber = nil
	case "zero-block":
		receipt.BlockHash = common.Hash{}
	}
	fixture := &stTransactionReconcileRPC{t: t, finalizedBlock: 100, finalizedNonce: 8, receipts: map[string]*types.Receipt{strings.ToLower(transaction.Hash().Hex()): receipt}}
	if fault == "finalized-overflow" {
		fixture.finalizedBlock = uint64(1) << 63
	}
	var client *CoreStClient
	var rpcClient *ethclient.Client
	if fault == "finalized-overflow" {
		// The generic fixture derives timestamp from height. Keep the timestamp
		// representable so the real decoder reaches the number/storage boundary.
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			response := httptest.NewRecorder()
			fixture.ServeHTTP(response, r)
			var envelope struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Result  json.RawMessage `json:"result"`
			}
			if json.Unmarshal(response.Body.Bytes(), &envelope) != nil {
				http.Error(w, "invalid fixture envelope", 500)
				return
			}
			var block map[string]json.RawMessage
			if json.Unmarshal(envelope.Result, &block) == nil && string(block["number"]) == "\"0x8000000000000000\"" {
				block["timestamp"] = json.RawMessage("\"0x6553f100\"")
				var err error
				envelope.Result, err = json.Marshal(block)
				if err != nil {
					http.Error(w, "invalid fixture block", 500)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(envelope)
		}))
		t.Cleanup(endpoint.Close)
		cfg.RpcUrls = []string{endpoint.URL}
		rpcClient, err = ethclient.Dial(endpoint.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(rpcClient.Close)
		client = &CoreStClient{cfg: cfg, clients: map[string]*ethclient.Client{endpoint.URL: rpcClient}}
	} else {
		client, rpcClient = newStTransactionReconcileClient(t, fixture, cfg)
	}
	terminal, pending, hash, observeErr := client.observeTransactionAttempts(ctx, rpcClient, intent, []*model.StTransactionAttempt{attempt})
	return terminal, pending, hash, observeErr, model.GetStTransactionIntent(ctx, intent.LogicalKey), model.GetCurrentStTransactionAttempt(ctx, intent.IntentId)
}

// Another canonical transaction must not finalize this account's durable intent.
func TestStReceiptObservationAuthenticatesRequestedTransactionBeforeMutation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		for _, fault := range []string{"transaction", "zero-transaction"} {
			terminal, pending, hash, err, intent, attempt := stReceiptIdentityObservation(tb, fault)
			if err == nil || terminal || pending || hash != "" || !strings.Contains(err.Error(), "receipt response differs") {
				tb.Fatalf("wrong receipt changed durable transaction verdict: fault%s terminal%t pending%t error%v", fault, terminal, pending, err)
			}
			if intent == nil || attempt == nil || intent.Status != model.StTxBroadcast || attempt.Status != model.StTxBroadcast || attempt.InclusionBlock != nil || attempt.InclusionHash != nil {
				tb.Fatalf("wrong receipt mutated durable account state: fault%s", fault)
			}
		}
	})
}

// Narrowing a 65-bit height or accepting a missing block used to reach DB writes.
func TestStReceiptObservationRejectsMalformedInclusionBeforeMutation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		for _, fault := range []string{"overflow", "signed-overflow", "zero-number", "missing-number", "zero-block"} {
			terminal, pending, hash, err, intent, attempt := stReceiptIdentityObservation(tb, fault)
			if err == nil || terminal || pending || hash != "" {
				tb.Fatalf("malformed receipt changed durable transaction verdict: fault%s terminal%t pending%t error%v", fault, terminal, pending, err)
			}
			if intent == nil || attempt == nil || intent.Status != model.StTxBroadcast || attempt.Status != model.StTxBroadcast || attempt.InclusionBlock != nil || attempt.InclusionHash != nil {
				tb.Fatalf("malformed receipt mutated durable account state: fault%s", fault)
			}
		}
	})
}

// A valid mined receipt cannot grant an unrepresentable final boundary authority
// to terminally consume the intent. Retain its genuine inclusion for recovery.
func TestStReceiptObservationRejectsFinalizedHeightNarrowing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		terminal, pending, hash, err, intent, attempt := stReceiptIdentityObservation(tb, "finalized-overflow")
		if err == nil || terminal || !pending || hash != "" || !strings.Contains(err.Error(), "postgres bigint") {
			tb.Fatalf("oversized finality boundary changed durable verdict: terminal%t pending%t error%v", terminal, pending, err)
		}
		if intent == nil || attempt == nil || intent.Status != model.StTxMined || attempt.Status != model.StTxMined || attempt.InclusionBlock == nil || *attempt.InclusionBlock != 99 || attempt.FinalizedBlock != nil || attempt.FinalizedHash != nil {
			tb.Fatal("oversized finality boundary mutated terminal state")
		}
	})
}

// The legitimate canonical receipt still reaches the real finalized DB state.
func TestStReceiptObservationPreservesExactCanonicalFinalization(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		terminal, pending, hash, err, intent, attempt := stReceiptIdentityObservation(tb, "canonical")
		if err != nil || !terminal || pending || hash == "" || intent == nil || attempt == nil || intent.Status != model.StTxFinalized || attempt.Status != model.StTxFinalized || attempt.InclusionBlock == nil || *attempt.InclusionBlock != 99 {
			tb.Fatalf("exact canonical receipt did not finalize: terminal%t pending%t hash%s error%v", terminal, pending, hash, err)
		}
	})
}
