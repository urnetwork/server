package controller

// Deterministic database + RPC tests for account-wide ST nonce recovery.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Serves the exact RPC surface used by account reconciliation and records any
// raw transaction. Receipts are finalized immediately, eliminating wall-clock
// timing from the state-machine tests.
type stTransactionReconcileRPC struct {
	t              testing.TB
	stateLock      sync.Mutex
	finalizedBlock uint64
	finalizedNonce uint64
	receipts       map[string]*types.Receipt
	sent           []*types.Transaction
}

// Uses explicit nonzero hashes because Subtensor's RPC hash domain is not the
// locally recomputed Ethereum header hash.
func stTransactionReconcileBlockHash(block uint64) common.Hash {
	return common.HexToHash(fmt.Sprintf("0x%064x", block+1))
}

func stTransactionReconcileBlock(block uint64) map[string]any {
	header := &types.Header{
		ParentHash: stTransactionReconcileBlockHash(block - 1), UncleHash: types.EmptyUncleHash,
		Root: stTransactionReconcileBlockHash(block + 1), TxHash: types.EmptyTxsHash,
		ReceiptHash: types.EmptyReceiptsHash, Difficulty: big.NewInt(1), Number: new(big.Int).SetUint64(block),
		GasLimit: 30_000_000, Time: 1_700_000_000 + block, Extra: []byte{},
	}
	encoded, err := json.Marshal(header)
	if err != nil {
		panic(err)
	}
	result := map[string]any{}
	if err := json.Unmarshal(encoded, &result); err != nil {
		panic(err)
	}
	result["hash"] = stTransactionReconcileBlockHash(block).Hex()
	return result
}

func (self *stTransactionReconcileRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	self.t.Helper()
	defer request.Body.Close()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		self.t.Errorf("read transaction RPC request: %v", err)
		return
	}
	var call stEpochRPCRequest
	if err := json.Unmarshal(body, &call); err != nil {
		self.t.Errorf("decode transaction RPC request: %v", err)
		return
	}
	var result any
	switch call.Method {
	case "eth_chainId":
		result = "0x3b1"
	case "eth_getBlockByNumber":
		var selector string
		if len(call.Params) == 0 || json.Unmarshal(call.Params[0], &selector) != nil {
			self.t.Errorf("invalid block selector: %s", body)
			return
		}
		block := self.finalizedBlock
		if selector != "latest" && selector != "finalized" {
			block, err = hexutil.DecodeUint64(selector)
			if err != nil {
				self.t.Errorf("decode block selector %q: %v", selector, err)
				return
			}
		}
		result = stTransactionReconcileBlock(block)
	case "eth_getTransactionCount":
		result = hexutil.EncodeUint64(self.finalizedNonce)
	case "eth_gasPrice":
		result = "0x64"
	case "eth_estimateGas":
		result = "0xc350"
	case "eth_sendRawTransaction":
		var rawHex string
		if len(call.Params) != 1 || json.Unmarshal(call.Params[0], &rawHex) != nil {
			self.t.Errorf("invalid raw transaction request: %s", body)
			return
		}
		raw, decodeErr := hexutil.Decode(rawHex)
		if decodeErr != nil {
			self.t.Errorf("decode raw transaction: %v", decodeErr)
			return
		}
		var transaction types.Transaction
		if decodeErr := transaction.UnmarshalBinary(raw); decodeErr != nil {
			self.t.Errorf("decode signed transaction: %v", decodeErr)
			return
		}
		hash := strings.ToLower(transaction.Hash().Hex())
		self.stateLock.Lock()
		self.sent = append(self.sent, &transaction)
		self.receipts[hash] = &types.Receipt{
			Type: transaction.Type(), TxHash: transaction.Hash(), BlockHash: stTransactionReconcileBlockHash(self.finalizedBlock - 1),
			BlockNumber: new(big.Int).SetUint64(self.finalizedBlock - 1), TransactionIndex: 0,
			Status: types.ReceiptStatusSuccessful, CumulativeGasUsed: transaction.Gas(), GasUsed: transaction.Gas(),
			EffectiveGasPrice: transaction.GasPrice(), Logs: []*types.Log{},
		}
		self.stateLock.Unlock()
		result = hash
	case "eth_getTransactionReceipt":
		var hash string
		if len(call.Params) != 1 || json.Unmarshal(call.Params[0], &hash) != nil {
			self.t.Errorf("invalid receipt request: %s", body)
			return
		}
		self.stateLock.Lock()
		result = self.receipts[strings.ToLower(hash)]
		self.stateLock.Unlock()
	default:
		self.t.Errorf("unexpected transaction RPC method %s", call.Method)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result}); err != nil {
		self.t.Errorf("encode transaction RPC response: %v", err)
	}
}

func newStTransactionReconcileClient(t testing.TB, fixture *stTransactionReconcileRPC, cfg *StConfig) (*CoreStClient, *ethclient.Client) {
	t.Helper()
	server := httptest.NewServer(fixture)
	cfg.RpcUrls = []string{server.URL}
	rpcClient, err := ethclient.Dial(server.URL)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	client := &CoreStClient{cfg: cfg, clients: map[string]*ethclient.Client{server.URL: rpcClient}}
	t.Cleanup(func() {
		rpcClient.Close()
		server.Close()
	})
	return client, rpcClient
}

func stTransactionReconcileConfig() *StConfig {
	cfg := &StConfig{
		Profile: "testnet", DeploymentId: "current", ChainId: 945,
		ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000002"),
	}
	cfg.GenesisHash[0] = 1
	return cfg
}

// A stale prepared intent is never executed against its old coordinator. Its
// nonce is consumed by a finalized self-transaction before nonce N+1 exists.
func TestStAccountReconcileCancelsStalePreparedIntent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		key, err := crypto.HexToECDSA(strings.Repeat("1", 64))
		if err != nil {
			t.Fatal(err)
		}
		from := crypto.PubkeyToAddress(key.PublicKey)
		cfg := stTransactionReconcileConfig()
		genesisHash := "0x" + hex.EncodeToString(cfg.GenesisHash[:])
		stale := model.ReserveStTransactionIntent(
			context.Background(), "stale-prepared", "testnet", "old", model.StDeploymentKey("945:0x1000000000000000000000000000000000000001"),
			945, genesisHash, strings.ToLower(from.Hex()), "0x1000000000000000000000000000000000000001",
			"0x"+strings.Repeat("2", 64), []byte{1, 2, 3}, 7,
		)
		fixture := &stTransactionReconcileRPC{t: t, finalizedBlock: 100, finalizedNonce: 7, receipts: map[string]*types.Receipt{}}
		client, rpcClient := newStTransactionReconcileClient(t, fixture, cfg)
		if err := client.reconcileAccountIntents(context.Background(), rpcClient, key, from); err != nil {
			t.Fatal(err)
		}
		stored := model.GetStTransactionIntent(context.Background(), stale.LogicalKey)
		if stored == nil || stored.Status != model.StTxCanceled {
			t.Fatalf("stale intent = %+v", stored)
		}
		fixture.stateLock.Lock()
		if len(fixture.sent) != 1 {
			fixture.stateLock.Unlock()
			t.Fatalf("sent transactions = %d, want 1", len(fixture.sent))
		}
		transaction := fixture.sent[0]
		fixture.stateLock.Unlock()
		if transaction.Nonce() != 7 || transaction.To() == nil || *transaction.To() != from || len(transaction.Data()) != 0 || transaction.Value().Sign() != 0 || transaction.Gas() != 21_000 {
			t.Fatalf("cancellation transaction nonce/to/data/value/gas = %d/%v/%x/%s/%d", transaction.Nonce(), transaction.To(), transaction.Data(), transaction.Value(), transaction.Gas())
		}
	})
}

// A known old-deployment transaction that already won the nonce is finalized
// from its canonical receipt; reconciliation must not send a cancellation.
func TestStAccountReconcileAcceptsKnownCanonicalReceipt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		key, err := crypto.HexToECDSA(strings.Repeat("2", 64))
		if err != nil {
			t.Fatal(err)
		}
		from := crypto.PubkeyToAddress(key.PublicKey)
		cfg := stTransactionReconcileConfig()
		genesisHash := "0x" + hex.EncodeToString(cfg.GenesisHash[:])
		to := common.HexToAddress("0x1000000000000000000000000000000000000001")
		intent := model.ReserveStTransactionIntent(
			context.Background(), "stale-known-receipt", "testnet", "old", model.StDeploymentKey("945:0x1000000000000000000000000000000000000001"),
			945, genesisHash, strings.ToLower(from.Hex()), strings.ToLower(to.Hex()),
			"0x"+strings.Repeat("3", 64), []byte{1, 2, 3}, 7,
		)
		transaction := types.NewTx(&types.LegacyTx{
			Nonce: intent.Nonce, To: &to, Gas: 50_000, GasPrice: big.NewInt(100), Value: new(big.Int), Data: intent.Calldata,
		})
		transaction, err = types.SignTx(transaction, types.LatestSignerForChainID(big.NewInt(945)), key)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := transaction.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		price := "100"
		attempt := model.AddStTransactionAttempt(context.Background(), &model.StTransactionAttempt{
			IntentId: intent.IntentId, Attempt: 1, Kind: model.StTxAttemptExecution,
			TxHash: strings.ToLower(transaction.Hash().Hex()), RawTransaction: raw, GasLimit: transaction.Gas(), GasPrice: &price,
		})
		model.MarkStTransactionBroadcast(context.Background(), intent.IntentId, attempt.Attempt)
		block := uint64(99)
		receipt := &types.Receipt{
			Type: transaction.Type(), TxHash: transaction.Hash(), BlockHash: stTransactionReconcileBlockHash(block),
			BlockNumber: new(big.Int).SetUint64(block), Status: types.ReceiptStatusSuccessful,
			CumulativeGasUsed: transaction.Gas(), GasUsed: transaction.Gas(), EffectiveGasPrice: transaction.GasPrice(), Logs: []*types.Log{},
		}
		fixture := &stTransactionReconcileRPC{
			t: t, finalizedBlock: 100, finalizedNonce: 8,
			receipts: map[string]*types.Receipt{strings.ToLower(transaction.Hash().Hex()): receipt},
		}
		client, rpcClient := newStTransactionReconcileClient(t, fixture, cfg)
		if err := client.reconcileAccountIntents(context.Background(), rpcClient, key, from); err != nil {
			t.Fatal(err)
		}
		stored := model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
		if stored == nil || stored.Status != model.StTxFinalized || stored.CurrentTxHash == nil || *stored.CurrentTxHash != strings.ToLower(transaction.Hash().Hex()) {
			t.Fatalf("known canonical intent = %+v", stored)
		}
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		if len(fixture.sent) != 0 {
			t.Fatalf("known canonical receipt triggered %d cancellation sends", len(fixture.sent))
		}
	})
}

// If the finalized account nonce advanced through an unknown transaction, the
// old intent is terminally superseded and its obsolete calldata is never sent.
func TestStAccountReconcileMarksUnknownNonceConsumptionSuperseded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		key, err := crypto.HexToECDSA(strings.Repeat("3", 64))
		if err != nil {
			t.Fatal(err)
		}
		from := crypto.PubkeyToAddress(key.PublicKey)
		cfg := stTransactionReconcileConfig()
		genesisHash := "0x" + hex.EncodeToString(cfg.GenesisHash[:])
		intent := model.ReserveStTransactionIntent(
			context.Background(), "stale-external-consumption", "testnet", "old", model.StDeploymentKey("945:0x1000000000000000000000000000000000000001"),
			945, genesisHash, strings.ToLower(from.Hex()), "0x1000000000000000000000000000000000000001",
			"0x"+strings.Repeat("4", 64), []byte{4, 5, 6}, 7,
		)
		fixture := &stTransactionReconcileRPC{t: t, finalizedBlock: 100, finalizedNonce: 8, receipts: map[string]*types.Receipt{}}
		client, rpcClient := newStTransactionReconcileClient(t, fixture, cfg)
		if err := client.reconcileAccountIntents(context.Background(), rpcClient, key, from); err != nil {
			t.Fatal(err)
		}
		stored := model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
		if stored == nil || stored.Status != model.StTxSuperseded {
			t.Fatalf("externally consumed intent = %+v", stored)
		}
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		if len(fixture.sent) != 0 {
			t.Fatalf("external nonce consumption triggered %d sends", len(fixture.sent))
		}
	})
}

// Signed, broadcast, uncertain, and orphaned-mined stale attempts all converge
// on the same safe cancellation path. No status may replay old coordinator
// calldata or leave the following account nonce blocked.
func TestStAccountReconcileCancelsEveryStaleAttemptState(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		states := []string{model.StTxSigned, model.StTxBroadcast, model.StTxUncertain, model.StTxMined}
		for index, state := range states {
			key, err := crypto.HexToECDSA(strings.Repeat(fmt.Sprintf("%x", index+4), 64))
			if err != nil {
				t.Fatal(err)
			}
			from := crypto.PubkeyToAddress(key.PublicKey)
			cfg := stTransactionReconcileConfig()
			genesisHash := "0x" + hex.EncodeToString(cfg.GenesisHash[:])
			to := common.HexToAddress("0x1000000000000000000000000000000000000001")
			intent := model.ReserveStTransactionIntent(
				context.Background(), "stale-state-"+state, "testnet", "old",
				model.StDeploymentKey("945:0x1000000000000000000000000000000000000001"),
				945, genesisHash, strings.ToLower(from.Hex()), strings.ToLower(to.Hex()),
				"0x"+strings.Repeat(fmt.Sprintf("%x", index+5), 64), []byte{byte(index + 1)}, 7,
			)
			transaction := types.NewTx(&types.LegacyTx{
				Nonce: intent.Nonce, To: &to, Gas: 50_000, GasPrice: big.NewInt(100), Value: new(big.Int), Data: intent.Calldata,
			})
			transaction, err = types.SignTx(transaction, types.LatestSignerForChainID(big.NewInt(945)), key)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := transaction.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			price := "100"
			attempt := model.AddStTransactionAttempt(context.Background(), &model.StTransactionAttempt{
				IntentId: intent.IntentId, Attempt: 1, Kind: model.StTxAttemptExecution,
				TxHash: strings.ToLower(transaction.Hash().Hex()), RawTransaction: raw, GasLimit: transaction.Gas(), GasPrice: &price,
			})
			switch state {
			case model.StTxBroadcast:
				model.MarkStTransactionBroadcast(context.Background(), intent.IntentId, attempt.Attempt)
			case model.StTxUncertain:
				model.MarkStTransactionUncertain(context.Background(), intent.IntentId, attempt.Attempt, fmt.Errorf("unknown broadcast"))
			case model.StTxMined:
				model.MarkStTransactionMined(context.Background(), intent.IntentId, attempt.Attempt, attempt.TxHash, 98, stTransactionReconcileBlockHash(98).Hex())
			}

			fixture := &stTransactionReconcileRPC{t: t, finalizedBlock: 100, finalizedNonce: 7, receipts: map[string]*types.Receipt{}}
			client, rpcClient := newStTransactionReconcileClient(t, fixture, cfg)
			if err := client.reconcileAccountIntents(context.Background(), rpcClient, key, from); err != nil {
				t.Fatalf("state %s: %v", state, err)
			}
			stored := model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
			attempts := model.GetStTransactionAttempts(context.Background(), intent.IntentId)
			if stored == nil || stored.Status != model.StTxCanceled || len(attempts) != 2 || attempts[0].Kind != model.StTxAttemptCancellation || attempts[0].Status != model.StTxCanceled {
				t.Fatalf("state %s cancellation = %+v attempts=%+v", state, stored, attempts)
			}
			fixture.stateLock.Lock()
			sentCount := len(fixture.sent)
			fixture.stateLock.Unlock()
			if sentCount != 1 {
				t.Fatalf("state %s sent %d transactions, want one cancellation", state, sentCount)
			}
		}
	})
}

// An unresolved intent belonging to the exact active coordinator resumes its
// immutable target and calldata rather than taking the stale cancellation path.
func TestStAccountReconcileResumesCurrentDeploymentIntent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		key, err := crypto.HexToECDSA(strings.Repeat("8", 64))
		if err != nil {
			t.Fatal(err)
		}
		from := crypto.PubkeyToAddress(key.PublicKey)
		cfg := stTransactionReconcileConfig()
		genesisHash := "0x" + hex.EncodeToString(cfg.GenesisHash[:])
		calldata := []byte{9, 8, 7}
		intent := model.ReserveStTransactionIntent(
			context.Background(), "current-prepared", "testnet", cfg.DeploymentId, cfg.DeploymentKey(),
			945, genesisHash, strings.ToLower(from.Hex()), strings.ToLower(cfg.ContractAddress.Hex()),
			"0x"+strings.Repeat("9", 64), calldata, 7,
		)
		fixture := &stTransactionReconcileRPC{t: t, finalizedBlock: 100, finalizedNonce: 7, receipts: map[string]*types.Receipt{}}
		client, rpcClient := newStTransactionReconcileClient(t, fixture, cfg)
		if err := client.reconcileAccountIntents(context.Background(), rpcClient, key, from); err != nil {
			t.Fatal(err)
		}
		stored := model.GetStTransactionIntent(context.Background(), intent.LogicalKey)
		if stored == nil || stored.Status != model.StTxFinalized {
			t.Fatalf("current intent = %+v", stored)
		}
		fixture.stateLock.Lock()
		if len(fixture.sent) != 1 {
			fixture.stateLock.Unlock()
			t.Fatalf("sent transactions = %d, want 1", len(fixture.sent))
		}
		transaction := fixture.sent[0]
		fixture.stateLock.Unlock()
		if transaction.To() == nil || *transaction.To() != cfg.ContractAddress || string(transaction.Data()) != string(calldata) || transaction.Nonce() != intent.Nonce {
			t.Fatalf("resumed transaction nonce/to/data = %d/%v/%x", transaction.Nonce(), transaction.To(), transaction.Data())
		}
	})
}
